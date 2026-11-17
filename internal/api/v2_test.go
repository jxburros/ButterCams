package api

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/jxburros/buttercams/internal/config"
	"github.com/jxburros/buttercams/internal/index"
	"github.com/jxburros/buttercams/internal/ipc"
)

// fakeMedia stands in for the media process behind the guardian's proxy.
type fakeMedia struct {
	ts    *httptest.Server
	mu    sync.Mutex
	seen  []*http.Request
	hold  chan struct{} // when set, /live requests wait for it
	lives atomic.Int32
}

func startFakeMedia(t *testing.T, a *testAPI) *fakeMedia {
	t.Helper()
	m := &fakeMedia{}
	up := websocket.Upgrader{}
	m.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.seen = append(m.seen, r.Clone(context.Background()))
		hold := m.hold
		m.mu.Unlock()
		if r.Header.Get(ipc.TokenHeader) != "media-token" {
			http.Error(w, "no token", http.StatusForbidden)
			return
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/live/") && websocket.IsWebSocketUpgrade(r):
			c, err := up.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer c.Close()
			c.WriteMessage(websocket.TextMessage, []byte(`{"type":"init","mime":"video/mp4"}`))
			c.WriteMessage(websocket.BinaryMessage, []byte("fragment of "+r.URL.Path))
			c.ReadMessage() // until the client leaves
		case strings.HasPrefix(r.URL.Path, "/live/"):
			m.lives.Add(1)
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			if hold != nil {
				select {
				case <-hold:
				case <-r.Context().Done():
				}
			}
		default:
			fmt.Fprintf(w, "played %s?%s", r.URL.Path, r.URL.RawQuery)
		}
	}))
	t.Cleanup(m.ts.Close)
	a.b.mediaAddr = strings.TrimPrefix(m.ts.URL, "http://")
	a.b.mediaToken = "media-token"
	return m
}

func (m *fakeMedia) last() *http.Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.seen) == 0 {
		return nil
	}
	return m.seen[len(m.seen)-1]
}

// twoCameras adds an allowed camera ("front-door") and one the limited
// Viewer must never see ("vault"), with recordings and events for both.
func twoCameras(t *testing.T, a *testAPI) (allowedSeg, forbiddenSeg int64) {
	t.Helper()
	_, err := a.b.UpdateConfig(func(c *config.Config) error {
		for _, id := range []string{"front-door", "vault"} {
			c.Cameras = append(c.Cameras, config.Camera{ID: id, Name: "Camera " + id, Enabled: true, Host: "10.0.0.9",
				Port: 554, Path: "/" + id, Transport: "tcp", RecordingMode: "continuous"})
		}
		c.Live.Views = []config.View{{ID: "shared-1", Name: "Shared", Layout: 4, CameraIDs: []string{"vault", "front-door"}}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	a.b.stats = map[string]CameraStats{
		"front-door": {CameraStats: ipc.CameraStats{CameraID: "front-door", State: ipc.StateRecording}, UpdatedAt: time.Now()},
		"vault":      {CameraStats: ipc.CameraStats{CameraID: "vault", State: ipc.StateRecording}, UpdatedAt: time.Now()},
	}
	start := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	ids := map[string]int64{}
	for _, cam := range []string{"front-door", "vault"} {
		name := cam + "_2026-11-14_0900_+0000.mp4"
		id, err := a.b.db.SegmentClosed(index.ClosedSegment{CameraID: cam, FileName: name, Start: start, End: start.Add(time.Hour),
			Bytes: 16, SHA256: "aa", VideoCodec: "h264", CloseReason: ipc.ReasonHourRollover}, nil)
		if err != nil {
			t.Fatal(err)
		}
		ids[cam] = id
		a.b.db.InsertEvent(index.Event{TS: start.Add(time.Minute), Source: "camera:" + cam, Type: "stream_stalled",
			Severity: ipc.SevWarning, CameraID: cam, Data: map[string]any{"message": "stalled " + cam}})
	}
	a.b.db.InsertEvent(index.Event{TS: start, Source: "guardian", Type: "worker_restarted", Severity: ipc.SevWarning,
		Data: map[string]any{"message": "Worker 1 was restarted. Cameras: Camera vault.", "cameras": []string{"vault"}}})
	a.b.files = map[int64]string{ids["front-door"]: "/rec/front-door.mp4", ids["vault"]: "/rec/vault.mp4"}
	a.b.health = &Health{Status: StatusWarning, CheckedAt: time.Now(), Reasons: []Reason{
		{Severity: ipc.SevWarning, Code: "camera_not_recording", Message: "Camera vault is not recording.", CameraID: "vault"},
		{Severity: ipc.SevWarning, Code: "camera_not_recording", Message: "Camera front-door is not recording.", CameraID: "front-door"},
		{Severity: ipc.SevWarning, Code: "worker_not_responding", Message: "Worker 1 is not responding. Cameras: Camera vault."},
	}}
	return ids["front-door"], ids["vault"]
}

func limitViewer(t *testing.T, a *testAPI, cams ...string) {
	t.Helper()
	if _, err := a.s.users.SetCameras("viewer", &cams); err != nil {
		t.Fatal(err)
	}
}

// A Viewer limited to some cameras cannot reach any other camera through any
// endpoint (spec 16.6 and 16.11). The test walks every registered route, so a
// route added later is covered automatically; one that names a camera must
// be added to the table below or the test fails.
func TestLimitedViewerIsFencedIn(t *testing.T) {
	a := newTestAPI(t)
	okSeg, noSeg := twoCameras(t, a)
	startFakeMedia(t, a)
	limitViewer(t, a, "front-door")
	a.login("viewer", "battery staple")

	const day = "from=2026-11-14T00:00:00Z&to=2026-11-15T00:00:00Z"
	// For every route that names a camera: a request for the forbidden
	// camera, which must be refused, and one for the allowed camera, which
	// must work.
	checked := map[string][2]string{
		"GET /api/v1/segments":                  {"/segments?camera=vault&date=2026-11-14", "/segments?camera=front-door&date=2026-11-14"},
		"GET /api/v1/segments/{id}/download":    {fmt.Sprintf("/segments/%d/download", noSeg), ""},
		"GET /api/v1/timeline":                  {"/timeline?camera=vault&" + day, "/timeline?camera=front-door&" + day},
		"GET /api/v1/segments/{id}/play/{what}": {fmt.Sprintf("/segments/%d/play/index", noSeg), fmt.Sprintf("/segments/%d/play/index", okSeg)},
		"GET /api/v1/live/{camera}/{stream}":    {"/live/vault/sub", "/live/front-door/sub"},
	}
	used := map[string]bool{}
	for _, route := range a.s.Routes() {
		method, path, _ := strings.Cut(route.Pattern, " ")
		path = strings.TrimPrefix(path, "/api/v1")
		concrete := strings.NewReplacer("{id}", "vault", "{username}", "viewer", "{camera}", "vault",
			"{stream}", "sub", "{what}", "index").Replace(path)
		if route.Pattern == "POST /api/v1/auth/logout" || route.Pattern == "POST /api/v1/auth/login" {
			continue // they name no camera, and signing out would end the walk
		}
		switch route.Cameras {
		case CamAdmin:
			if status, _, _ := a.raw(method, concrete, map[string]any{}, true); status != http.StatusForbidden {
				t.Errorf("%s: a Viewer got %d, want 403", route.Pattern, status)
			}
		case CamChecked:
			urls, ok := checked[route.Pattern]
			if !ok {
				t.Errorf("%s names a camera but this test has no forbidden request for it", route.Pattern)
				continue
			}
			used[route.Pattern] = true
			status, raw, _ := a.raw(method, urls[0], nil, true)
			if status != http.StatusForbidden && status != http.StatusNotFound {
				t.Errorf("%s: the forbidden camera answered %d: %s", route.Pattern, status, raw)
			}
			if strings.Contains(string(raw), "vault.mp4") || strings.Contains(string(raw), "played") {
				t.Errorf("%s: the forbidden camera's data came back: %s", route.Pattern, raw)
			}
			if urls[1] != "" {
				if status, raw, _ := a.raw(method, urls[1], nil, true); status != http.StatusOK {
					t.Errorf("%s: the allowed camera answered %d: %s", route.Pattern, status, raw)
				}
			}
		case CamFiltered, CamNone:
			if route.Pattern == "GET /api/v1/stream" {
				continue // checked below
			}
			var body any
			if method != "GET" {
				body = map[string]any{"views": []any{map[string]any{"id": "mine-1", "name": "Mine", "layout": 4,
					"cameraIds": []string{"vault", "front-door"}}}}
			}
			status, raw, _ := a.raw(method, concrete, body, true)
			if strings.Contains(string(raw), "vault") {
				t.Errorf("%s (%d) shows the forbidden camera: %s", route.Pattern, status, raw)
			}
			if route.Cameras == CamFiltered && method == "GET" && status != http.StatusOK {
				t.Errorf("%s answered %d", route.Pattern, status)
			}
		default:
			t.Errorf("%s has an unknown camera rule", route.Pattern)
		}
	}
	for pattern := range checked {
		if !used[pattern] {
			t.Errorf("the test names %s, which is not a route any more", pattern)
		}
	}

	// What a limited Viewer does see.
	_, out := a.do("GET", "/cameras", nil)
	if cams := out["cameras"].([]any); len(cams) != 1 || cams[0].(map[string]any)["id"] != "front-door" {
		t.Fatalf("cameras: %v", out)
	}
	_, out = a.do("GET", "/events", nil)
	if evs := out["events"].([]any); len(evs) != 1 || *strPtr(evs[0].(map[string]any)["cameraId"]) != "front-door" {
		t.Fatalf("events: %v", out)
	}
	_, out = a.do("GET", "/health", nil)
	if rs := out["reasons"].([]any); out["status"] != StatusWarning || len(rs) != 1 || rs[0].(map[string]any)["cameraId"] != "front-door" {
		t.Fatalf("health: %v", out)
	}
	_, out = a.do("GET", "/live/views", nil)
	shared := out["shared"].([]any)[0].(map[string]any)
	if ids := shared["cameraIds"].([]any); len(ids) != 1 || ids[0] != "front-door" {
		t.Fatalf("a shared view must show only the cameras the user may see: %v", out)
	}
	if mine := out["mine"].([]any); len(mine) != 1 || len(mine[0].(map[string]any)["cameraIds"].([]any)) != 1 {
		t.Fatalf("own views: %v", out)
	}

	// The event stream: only this user's cameras, in every kind of event.
	req, _ := http.NewRequest("GET", a.ts.URL+"/api/v1/stream", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := a.c.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	lines := make(chan string, 256)
	go func() {
		sc := bufio.NewScanner(res.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	for a.s.hub.count() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	vault, front := "vault", "front-door"
	a.s.PublishEvent(index.Event{ID: 90, TS: time.Now(), Source: "camera:vault", Type: "stream_stalled", Severity: ipc.SevWarning, CameraID: vault})
	a.s.PublishEvent(index.Event{ID: 91, TS: time.Now(), Source: "guardian", Type: "worker_restarted", Severity: ipc.SevWarning,
		Data: map[string]any{"message": "Cameras: Camera vault."}})
	a.s.PublishHealth(a.b.Health())
	a.s.PublishLive()
	a.s.PublishEvent(index.Event{ID: 92, TS: time.Now(), Source: "camera:front-door", Type: "the_last_one", Severity: ipc.SevInfo, CameraID: front})
	sawCameras := false
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatal("the stream ended early")
			}
			if strings.Contains(l, "vault") {
				t.Fatalf("the event stream shows the forbidden camera: %s", l)
			}
			sawCameras = sawCameras || (strings.HasPrefix(l, "data:") && strings.Contains(l, `"cameras":[{"id":"front-door"`))
			if strings.Contains(l, "the_last_one") {
				if !sawCameras {
					t.Fatal("the camera states of the allowed camera never arrived")
				}
				return
			}
		case <-ctx.Done():
			t.Fatal("the allowed camera's event never arrived on the stream")
		}
	}
}

func strPtr(v any) *string {
	s, _ := v.(string)
	return &s
}

// An Admin, and a Viewer without a limit, see everything.
func TestUnlimitedUsersSeeEveryCamera(t *testing.T) {
	a := newTestAPI(t)
	twoCameras(t, a)
	for _, who := range [][2]string{{"viewer", "battery staple"}, {"admin", "correct horse"}} {
		a.login(who[0], who[1])
		_, out := a.do("GET", "/cameras", nil)
		if len(out["cameras"].([]any)) != 2 {
			t.Fatalf("%s sees %v", who[0], out)
		}
		_, out = a.do("GET", "/events", nil)
		if len(out["events"].([]any)) != 3 {
			t.Fatalf("%s sees events %v", who[0], out)
		}
		_, out = a.do("GET", "/health", nil)
		if len(out["reasons"].([]any)) != 3 {
			t.Fatalf("%s sees health %v", who[0], out)
		}
	}
	// A limit set on an Admin account is ignored.
	limit := []string{"front-door"}
	a.s.users.SetCameras("admin", &limit)
	_, out := a.do("GET", "/cameras", nil)
	if len(out["cameras"].([]any)) != 2 {
		t.Fatalf("an Admin must always see every camera: %v", out)
	}
}

func TestUserCameraLimitsThroughTheAPI(t *testing.T) {
	a := newTestAPI(t)
	a.login("admin", "correct horse")
	status, out := a.do("POST", "/users", map[string]any{"username": "gate", "password": "long enough", "role": "viewer",
		"cameras": []string{"front-door", "front-door", " "}})
	if status != 201 || len(out["cameras"].([]any)) != 1 {
		t.Fatalf("create with a limit: %d %v", status, out)
	}
	_, out = a.do("GET", "/users", nil)
	for _, u := range out["users"].([]any) {
		u := u.(map[string]any)
		if u["username"] == "viewer" && u["cameras"] != nil {
			t.Fatalf("a user without a limit must have cameras null: %v", u)
		}
	}
	// Absent keeps, a list replaces, null removes.
	if _, out = a.do("PUT", "/users/gate", map[string]any{"role": "viewer"}); len(out["cameras"].([]any)) != 1 {
		t.Fatalf("a request without cameras changed the limit: %v", out)
	}
	if _, out = a.do("PUT", "/users/gate", map[string]any{"cameras": []string{}}); out["cameras"] == nil || len(out["cameras"].([]any)) != 0 {
		t.Fatalf("an empty list means no camera at all: %v", out)
	}
	if _, out = a.do("PUT", "/users/gate", map[string]any{"cameras": nil}); out["cameras"] != nil {
		t.Fatalf("null must remove the limit: %v", out)
	}
	if status, out := a.do("PUT", "/users/gate", map[string]any{"cameras": "all"}); status != 400 || fieldsOf(out)["cameras"] == "" {
		t.Fatalf("a bad cameras value: %d %v", status, out)
	}
	// The limit is stored in users.json and survives a restart.
	limit := []string{"front-door"}
	a.s.users.SetCameras("gate", &limit)
	again, err := OpenUserStore(a.b.root)
	if err != nil {
		t.Fatal(err)
	}
	if sc := again.ScopeOf("gate"); sc.All() || !sc.Allows("front-door") || sc.Allows("vault") {
		t.Fatalf("scope after reload: %+v", sc)
	}
	if sc := again.ScopeOf("nobody"); sc.All() || sc.Allows("front-door") {
		t.Fatal("an unknown user must see nothing")
	}
}

// Each user has their own views; Admins also keep shared ones (spec 16.6).
func TestSavedViews(t *testing.T) {
	a := newTestAPI(t)
	twoCameras(t, a)
	view := func(id string, cams ...string) map[string]any {
		return map[string]any{"views": []any{map[string]any{"id": id, "name": "View " + id, "layout": 4, "cameraIds": cams}}}
	}

	a.login("viewer", "battery staple")
	status, out := a.do("PUT", "/live/views/mine", view("v-viewer", "front-door", "no-such-camera"))
	if status != 200 || len(out["mine"].([]any)) != 1 {
		t.Fatalf("a Viewer saving their own view: %d %v", status, out)
	}
	if ids := out["mine"].([]any)[0].(map[string]any)["cameraIds"].([]any); len(ids) != 1 {
		t.Fatalf("a camera that does not exist must be dropped: %v", ids)
	}
	if status, out := a.do("PUT", "/live/views/shared", view("x", "front-door")); status != 403 {
		t.Fatalf("a Viewer writing shared views: %d %v", status, out)
	}
	if status, out := a.do("PUT", "/live/views/mine", map[string]any{"views": []any{map[string]any{"id": "a b", "name": "", "layout": 7}}}); status != 400 ||
		fieldsOf(out)["views[0].layout"] == "" || fieldsOf(out)["views[0].name"] == "" || fieldsOf(out)["views[0].id"] == "" {
		t.Fatalf("an invalid view: %d %v", status, out)
	}
	// Without the CSRF token the write is refused like any other.
	if status, _, _ := a.raw("PUT", "/live/views/mine", view("v2"), false); status != 403 {
		t.Fatalf("a view saved without the CSRF token: %d", status)
	}

	// The Admin does not see the Viewer's view, and the other way round.
	a.login("admin", "correct horse")
	_, out = a.do("GET", "/live/views", nil)
	if len(out["mine"].([]any)) != 0 || len(out["shared"].([]any)) != 1 {
		t.Fatalf("the Admin sees someone else's views: %v", out)
	}
	if status, out := a.do("PUT", "/live/views/mine", view("v-admin", "vault")); status != 200 {
		t.Fatalf("%d %v", status, out)
	}
	status, out = a.do("PUT", "/live/views/shared", view("everyone", "vault", "front-door"))
	if status != 200 || out["shared"].([]any)[0].(map[string]any)["id"] != "everyone" {
		t.Fatalf("an Admin sharing a view: %d %v", status, out)
	}
	if got := a.b.Config().Live.Views; len(got) != 1 || got[0].ID != "everyone" || len(got[0].CameraIDs) != 2 {
		t.Fatalf("shared views are stored in config.json: %+v", got)
	}

	a.login("viewer", "battery staple")
	_, out = a.do("GET", "/live/views", nil)
	mine := out["mine"].([]any)
	if len(mine) != 1 || mine[0].(map[string]any)["id"] != "v-viewer" {
		t.Fatalf("the Viewer's own views: %v", out)
	}
	if len(out["shared"].([]any)[0].(map[string]any)["cameraIds"].([]any)) != 2 {
		t.Fatalf("an unlimited Viewer sees the whole shared view: %v", out)
	}

	// Deleting a camera removes it from the shared views instead of failing.
	a.login("admin", "correct horse")
	if status, out := a.do("DELETE", "/cameras/vault", nil); status != 204 {
		t.Fatalf("deleting a camera that is in a shared view: %d %v", status, out)
	}
	if got := a.b.Config().Live.Views[0].CameraIDs; len(got) != 1 || got[0] != "front-door" {
		t.Fatalf("shared view after the camera was deleted: %v", got)
	}
	// ... and it disappears from own views when they are read.
	_, out = a.do("GET", "/live/views", nil)
	if ids := out["mine"].([]any)[0].(map[string]any)["cameraIds"].([]any); len(ids) != 0 {
		t.Fatalf("own view still shows the deleted camera: %v", ids)
	}
}

func TestTimeline(t *testing.T) {
	a := newTestAPI(t)
	twoCameras(t, a)
	a.login("viewer", "battery staple")
	base := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	// 09:00–10:00 exists. Add the open file from 10:00 and an event outside
	// the range.
	a.b.db.SegmentOpened("front-door", "front-door_2026-11-14_1000_+0000.mp4", base.Add(time.Hour), false, "h264", "aac")
	a.b.db.InsertEvent(index.Event{TS: base.Add(-48 * time.Hour), Source: "camera:front-door", Type: "stream_stalled",
		Severity: ipc.SevWarning, CameraID: "front-door"})
	a.b.db.InsertEvent(index.Event{TS: base.Add(2 * time.Minute), Source: "guardian", Type: "camera_changed",
		Severity: ipc.SevInfo, CameraID: "front-door"})

	q := func(from, to time.Time) string {
		return "/timeline?camera=front-door&from=" + url.QueryEscape(from.Format(time.RFC3339)) + "&to=" + url.QueryEscape(to.Format(time.RFC3339))
	}
	status, out := a.do("GET", q(base.Add(-9*time.Hour), base.Add(15*time.Hour)), nil)
	segs, _ := out["segments"].([]any)
	if status != 200 || len(segs) != 2 {
		t.Fatalf("timeline: %d %v", status, out)
	}
	first, open := segs[0].(map[string]any), segs[1].(map[string]any)
	if first["open"] != false || first["end"] == nil || first["videoCodec"] != "h264" || first["gapBefore"] != false || first["id"] == nil {
		t.Fatalf("closed span: %v", first)
	}
	if open["open"] != true || open["end"] != nil {
		t.Fatalf("the hour being recorded must be on the timeline with end null: %v", open)
	}
	if evs := out["events"].([]any); len(evs) != 1 || evs[0].(map[string]any)["type"] != "stream_stalled" {
		t.Fatalf("only recorder events inside the range belong on the timeline: %v", out["events"])
	}
	// A range that only touches the end of the first file still returns it.
	_, out = a.do("GET", q(base.Add(59*time.Minute), base.Add(59*time.Minute+time.Second)), nil)
	if segs := out["segments"].([]any); len(segs) != 1 {
		t.Fatalf("overlap: %v", out)
	}
	// Before any recording: nothing.
	_, out = a.do("GET", q(base.Add(-3*time.Hour), base.Add(-2*time.Hour)), nil)
	if segs := out["segments"].([]any); len(segs) != 0 {
		t.Fatalf("empty range: %v", out)
	}
	for _, bad := range []string{
		"/timeline?camera=front-door&from=x&to=y",
		q(base, base),
		q(base, base.Add(32*24*time.Hour)),
		"/timeline?camera=&from=2026-11-14T00:00:00Z&to=2026-11-15T00:00:00Z",
	} {
		if status, out := a.do("GET", bad, nil); status != 400 {
			t.Errorf("%s: %d %v", bad, status, out)
		}
	}
}

// The guardian only forwards: it adds the token, passes the file paths and
// never hands the browser's session to the media process (spec 16.2).
func TestMediaProxy(t *testing.T) {
	a := newTestAPI(t)
	okSeg, _ := twoCameras(t, a)
	a.login("viewer", "battery staple")

	// No media process: 503 media_unavailable.
	status, out := a.do("GET", fmt.Sprintf("/segments/%d/play/index", okSeg), nil)
	if status != 503 || errCode(out) != "media_unavailable" {
		t.Fatalf("without the media process: %d %v", status, out)
	}

	m := startFakeMedia(t, a)
	status, raw, _ := a.raw("GET", fmt.Sprintf("/segments/%d/play/keyframes?from=1000&to=5000", okSeg), nil, false)
	if status != 200 || string(raw) != "played /play/keyframes?from=1000&to=5000" {
		t.Fatalf("proxied playback request: %d %q", status, raw)
	}
	got := m.last()
	if got.Header.Get("Cookie") != "" || got.Header.Get(ipc.TokenHeader) != "media-token" {
		t.Fatalf("headers passed to the media process: %v", got.Header)
	}
	list, _ := base64.RawURLEncoding.DecodeString(got.Header.Get(ipc.PathsHeader))
	var paths []string
	json.Unmarshal(list, &paths)
	if len(paths) != 1 || paths[0] != "/rec/front-door.mp4" || got.Header.Get(ipc.OpenHeader) != "" {
		t.Fatalf("paths passed to the media process: %q open=%q", list, got.Header.Get(ipc.OpenHeader))
	}
	// A client cannot smuggle its own paths or token in.
	req, _ := http.NewRequest("GET", a.ts.URL+fmt.Sprintf("/api/v1/segments/%d/play/data", okSeg), nil)
	req.Header.Set(ipc.PathsHeader, base64.RawURLEncoding.EncodeToString([]byte(`["/etc/passwd"]`)))
	req.Header.Set(ipc.OpenHeader, "1")
	req.Header.Set("Range", "bytes=0-9")
	res, err := a.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	got = m.last()
	list, _ = base64.RawURLEncoding.DecodeString(got.Header.Get(ipc.PathsHeader))
	if strings.Contains(string(list), "passwd") || got.Header.Get(ipc.OpenHeader) != "" || got.Header.Get("Range") != "bytes=0-9" {
		t.Fatalf("client headers reached the media process: %q %v", list, got.Header)
	}
	for _, bad := range []string{"/segments/999/play/index", fmt.Sprintf("/segments/%d/play/secrets", okSeg), "/live/front-door/both", "/live/nope/sub"} {
		if status, _, _ := a.raw("GET", bad, nil, false); status != 404 {
			t.Errorf("%s: %d, want 404", bad, status)
		}
	}

	// The open segment is announced as such.
	openID, _ := a.b.db.SegmentOpened("front-door", "front-door_2026-11-14_1000_+0000.mp4", time.Now(), false, "h264", "")
	a.b.files[openID] = "/rec/open.part.mp4"
	a.raw("GET", fmt.Sprintf("/segments/%d/play/index", openID), nil, false)
	if m.last().Header.Get(ipc.OpenHeader) != "1" {
		t.Fatal("the open segment was not marked")
	}

	// Live view switched off: 503 live_disabled.
	a.b.UpdateConfig(func(c *config.Config) error { c.Live.Enabled = false; return nil })
	if status, out := a.do("GET", "/live/front-door/sub", nil); status != 503 || errCode(out) != "live_disabled" {
		t.Fatalf("live view switched off: %d %v", status, out)
	}
}

func TestLiveStreamLimitsAndWebSocket(t *testing.T) {
	a := newTestAPI(t)
	twoCameras(t, a)
	m := startFakeMedia(t, a)
	a.login("viewer", "battery staple")

	// Four streams of one camera per session; the fifth is refused.
	m.mu.Lock()
	m.hold = make(chan struct{})
	m.mu.Unlock()
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	for i := 0; i < MaxStreamsPerCamera; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequestWithContext(ctx, "GET", a.ts.URL+"/api/v1/live/front-door/sub", nil)
			if res, err := a.c.Do(req); err == nil {
				io.Copy(io.Discard, res.Body)
				res.Body.Close()
			}
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for m.lives.Load() < MaxStreamsPerCamera && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if status, out := a.do("GET", "/live/front-door/sub", nil); status != 503 || errCode(out) != "too_many_streams" {
		t.Fatalf("fifth stream of one camera: %d %v", status, out)
	}
	// Another camera is not affected.
	other := make(chan int, 1)
	go func() {
		req, _ := http.NewRequestWithContext(ctx, "GET", a.ts.URL+"/api/v1/live/vault/sub", nil)
		res, err := a.c.Do(req)
		if err != nil {
			other <- 0
			return
		}
		other <- res.StatusCode
		res.Body.Close()
	}()
	if status := <-other; status != 200 {
		t.Fatalf("a stream of another camera while the first is at its limit: %d", status)
	}
	close(m.hold)
	cancel()
	wg.Wait()
	m.mu.Lock()
	m.hold = nil
	m.mu.Unlock()
	// The slots are free again.
	deadline = time.Now().Add(5 * time.Second)
	for {
		a.s.media.mu.Lock()
		n := len(a.s.media.streams)
		a.s.media.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d stream slots were not released", n)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// A WebSocket passes through the proxy, with the session cookie.
	wsURL := "ws" + strings.TrimPrefix(a.ts.URL, "http") + "/api/v1/live/front-door/main?audio=1"
	u, _ := url.Parse(a.ts.URL)
	hdr := http.Header{}
	for _, c := range a.c.Jar.Cookies(u) {
		hdr.Add("Cookie", c.String())
	}
	hdr.Set("Origin", a.ts.URL)
	c, res, err := websocket.DefaultDialer.Dial(wsURL, hdr)
	if err != nil {
		t.Fatalf("WebSocket through the proxy: %v (%v)", err, res)
	}
	typ, msg, err := c.ReadMessage()
	if err != nil || typ != websocket.TextMessage || !strings.Contains(string(msg), `"init"`) {
		t.Fatalf("first message: %d %q %v", typ, msg, err)
	}
	typ, msg, err = c.ReadMessage()
	if err != nil || typ != websocket.BinaryMessage || string(msg) != "fragment of /live/front-door/main" {
		t.Fatalf("second message: %d %q %v", typ, msg, err)
	}
	c.Close()
	if q := m.last().URL.RawQuery; q != "audio=1" {
		t.Fatalf("query passed on: %q", q)
	}

	// A page of another site may not open streams with the user's cookie.
	hdr.Set("Origin", "https://evil.example")
	if _, res, err := websocket.DefaultDialer.Dial(wsURL, hdr); err == nil || res == nil || res.StatusCode != 403 {
		t.Fatalf("a cross-site WebSocket was accepted: %v %v", err, res)
	}
	// Without a session: 401.
	if _, res, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Origin": {a.ts.URL}}); err == nil || res == nil || res.StatusCode != 401 {
		t.Fatalf("a WebSocket without a session: %v %v", err, res)
	}
}
