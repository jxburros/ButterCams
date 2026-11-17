package api

import (
	"net/http"

	"github.com/jxburros/buttercams/internal/index"
)

// Per-user camera limits (spec 16.6). A Viewer can be limited to a list of
// cameras; the limit applies wherever a camera appears. Admins see all.

const noCameraAccess = "Your account does not have access to this camera."

// scopeOf returns the cameras the request's user may see. Without a session
// (the public health route from localhost) it is every camera.
func (s *Server) scopeOf(r *http.Request) Scope {
	sess, ok := sessionOf(r)
	if !ok {
		if c, err := r.Cookie(SessionCookie); err == nil {
			sess, ok = s.sessions.get(c.Value)
		}
	}
	if !ok || sess.role == RoleAdmin {
		return AllCameras
	}
	return s.users.ScopeOf(sess.username)
}

// cameraAllowed answers 403 and returns false when the user may not see the
// camera.
func (s *Server) cameraAllowed(w http.ResponseWriter, r *http.Request, cameraID string) bool {
	if s.scopeOf(r).Allows(cameraID) {
		return true
	}
	writeError(w, http.StatusForbidden, "forbidden", noCameraAccess)
	return false
}

// segmentAllowed looks a segment up and checks its camera. A segment of a
// camera the user may not see is answered exactly like one that does not
// exist, so ids cannot be probed.
func (s *Server) segmentAllowed(w http.ResponseWriter, r *http.Request, id int64) (index.Segment, bool) {
	seg, err := s.backend.Index().Segment(id)
	if err != nil || !s.scopeOf(r).Allows(seg.CameraID) {
		writeError(w, http.StatusNotFound, "not_found", "No such recording.")
		return seg, false
	}
	return seg, true
}

// restrictHealth keeps the overall status and the reasons that belong to the
// user's cameras. Reasons about the system as a whole often name cameras in
// their text, so a limited Viewer does not get them.
func restrictHealth(h Health, sc Scope) Health {
	out := h
	out.Reasons = []Reason{}
	for _, r := range h.Reasons {
		if r.CameraID != "" && sc.Allows(r.CameraID) {
			out.Reasons = append(out.Reasons, r)
		}
	}
	return out
}

// eventAllowed reports whether a limited Viewer may see an event: only the
// events of their own cameras.
func eventAllowed(cameraID *string, sc Scope) bool {
	return sc.All() || (cameraID != nil && sc.Allows(*cameraID))
}

// restrictStream is the event stream's filter for limited Viewers.
func (s *Server) restrictStream(username, event string, v any) (any, bool) {
	sc := s.users.ScopeOf(username)
	if sc.All() {
		return v, true
	}
	switch event {
	case "event":
		ev, ok := v.(eventView)
		return v, ok && eventAllowed(ev.CameraID, sc)
	case "health":
		hv, ok := v.(healthView)
		if !ok {
			return nil, false
		}
		hv.Health = restrictHealth(hv.Health, sc)
		return hv, true
	case "cameras":
		return s.camerasLive(sc), true
	case "destinations":
		return v, true
	}
	return nil, false // an event kind added later is not shown until it has a rule
}

func (s *Server) streamLimited(username string) bool {
	u, ok := s.users.Get(username)
	return !ok || !u.scope().All()
}
