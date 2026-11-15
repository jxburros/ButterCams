package api

import (
	"net/http"
	"time"

	"github.com/jxburros/buttercams/internal/config"
	"github.com/jxburros/buttercams/internal/index"
	"github.com/jxburros/buttercams/internal/naming"
)

// ---------------------------------------------------------------- saved views

type viewView struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Layout    int      `json:"layout"`
	CameraIDs []string `json:"cameraIds"`
}

// visibleViews returns views with only the cameras that exist and that the
// user may see (spec 16.6).
func visibleViews(views []config.View, cfg *config.Config, sc Scope) []viewView {
	out := make([]viewView, 0, len(views))
	for _, v := range views {
		ids := make([]string, 0, len(v.CameraIDs))
		for _, id := range v.CameraIDs {
			if _, exists := cfg.Camera(id); exists && sc.Allows(id) {
				ids = append(ids, id)
			}
		}
		out = append(out, viewView{ID: v.ID, Name: v.Name, Layout: v.Layout, CameraIDs: ids})
	}
	return out
}

func (s *Server) viewsPayload(r *http.Request) map[string]any {
	cfg := s.backend.Config()
	sc := s.scopeOf(r)
	sess, _ := sessionOf(r)
	var mine []config.View
	if u, ok := s.users.Get(sess.username); ok {
		mine = u.Views
	}
	return map[string]any{
		"shared": visibleViews(cfg.Live.Views, cfg, sc),
		"mine":   visibleViews(mine, cfg, sc),
	}
}

// handleViewsGet is GET /live/views: the shared views and the user's own.
func (s *Server) handleViewsGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.viewsPayload(r))
}

type viewsInput struct {
	Views []config.View `json:"views"`
}

// handleViewsPutMine replaces the signed-in user's own views. Every user may
// do this, Viewers included.
func (s *Server) handleViewsPutMine(w http.ResponseWriter, r *http.Request) {
	var in viewsInput
	if err := readJSON(r, &in); err != nil {
		s.writeFail(w, err)
		return
	}
	cfg := s.backend.Config()
	sc := s.scopeOf(r)
	// Cameras that do not exist, or that this user may not see, are dropped:
	// a view is a list of what to show, never a way to reach a camera.
	views := config.NormalizeViews(in.Views, func(id string) bool {
		_, exists := cfg.Camera(id)
		return exists && sc.Allows(id)
	})
	sess, _ := sessionOf(r)
	if _, err := s.users.SetViews(sess.username, views); err != nil {
		s.writeFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.viewsPayload(r))
}

// handleViewsPutShared replaces the shared views (Admins).
func (s *Server) handleViewsPutShared(w http.ResponseWriter, r *http.Request) {
	var in viewsInput
	if err := readJSON(r, &in); err != nil {
		s.writeFail(w, err)
		return
	}
	if in.Views == nil {
		in.Views = []config.View{}
	}
	_, err := s.backend.UpdateConfig(func(c *config.Config) error {
		known := map[string]bool{}
		for _, cam := range c.Cameras {
			known[cam.ID] = true
		}
		c.Live.Views = config.NormalizeViews(in.Views, func(id string) bool { return known[id] })
		return nil
	})
	if err != nil {
		s.writeFail(w, err)
		return
	}
	s.audit(r, "views_changed", "The shared live views were changed.", "")
	writeJSON(w, http.StatusOK, s.viewsPayload(r))
}

// ------------------------------------------------------------------- timeline

// MaxTimelineRange is the longest span one timeline request may cover.
const MaxTimelineRange = 31 * 24 * time.Hour

type timelineSegment struct {
	ID         int64      `json:"id"`
	Start      time.Time  `json:"start"`
	End        *time.Time `json:"end"` // null while the file is being recorded
	GapBefore  bool       `json:"gapBefore"`
	VideoCodec string     `json:"videoCodec"`
	AudioCodec string     `json:"audioCodec"`
	Open       bool       `json:"open"`
	FileName   string     `json:"fileName"`
	Bytes      *int64     `json:"bytes"`
}

// timelineEventTypes are the recorder events drawn on the timeline: stalls,
// reconnects and clock changes (spec 16.5).
var timelineEventTypes = map[string]bool{
	"stream_stalled": true, "camera_unreachable": true, "camera_recovered": true, "auth_failed": true,
	"clock_jump": true, "refresh_failed": true, "write_failed": true, "packets_dropped": true,
	"camera_panic": true, "unsupported_codec": true,
}

// handleTimeline is GET /timeline?camera=&from=&to=: the recorded spans of
// one camera that overlap [from, to) and its recorder events in that range,
// straight from the index.
func (s *Server) handleTimeline(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cam := q.Get("camera")
	if !naming.ValidID(cam) {
		s.writeFail(w, FieldErr("camera", "Choose a camera."))
		return
	}
	if !s.cameraAllowed(w, r, cam) {
		return
	}
	from, err1 := time.Parse(time.RFC3339Nano, q.Get("from"))
	to, err2 := time.Parse(time.RFC3339Nano, q.Get("to"))
	switch {
	case err1 != nil:
		s.writeFail(w, FieldErr("from", "Enter the start of the range as a date and time."))
		return
	case err2 != nil:
		s.writeFail(w, FieldErr("to", "Enter the end of the range as a date and time."))
		return
	case !to.After(from):
		s.writeFail(w, FieldErr("to", "The end of the range must be after its start."))
		return
	case to.Sub(from) > MaxTimelineRange:
		s.writeFail(w, FieldErr("to", "The range is too long (31 days at most)."))
		return
	}
	db := s.backend.Index()
	segs, err := db.TimelineSegments(cam, from, to)
	if err != nil {
		s.writeFail(w, err)
		return
	}
	outSegs := make([]timelineSegment, 0, len(segs))
	for _, seg := range segs {
		ts := timelineSegment{ID: seg.ID, Start: seg.Start, GapBefore: seg.GapBefore, VideoCodec: seg.VideoCodec,
			AudioCodec: seg.AudioCodec, Open: !seg.Closed(), FileName: seg.FileName}
		if seg.Closed() {
			end, bytes := seg.End, seg.Bytes
			ts.End, ts.Bytes = &end, &bytes
		}
		outSegs = append(outSegs, ts)
	}
	events, err := db.ListEvents(index.EventFilter{CameraID: cam, Since: from, Until: to, Limit: 1000, Oldest: true})
	if err != nil {
		s.writeFail(w, err)
		return
	}
	outEvents := make([]eventView, 0, len(events))
	for _, e := range events {
		if timelineEventTypes[e.Type] {
			outEvents = append(outEvents, toEventView(e))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"camera": cam, "from": from, "to": to, "now": time.Now(),
		"segments": outSegs, "events": outEvents,
	})
}
