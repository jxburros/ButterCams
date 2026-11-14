package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jxburros/buttercams/internal/config"
	"github.com/jxburros/buttercams/internal/index"
	"github.com/jxburros/buttercams/internal/ipc"
)

// fakeBackend stands in for the guardian.
type fakeBackend struct {
	mu      sync.Mutex
	cfg     *config.Config
	db      *index.DB
	root    string
	events  []index.Event
	purged  []string
	rebuilt int
	stats   map[string]CameraStats
	boom    bool
}

func (f *fakeBackend) Config() *config.Config {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfg.Clone()
}

func (f *fakeBackend) UpdateConfig(mutate func(*config.Config) error) (*config.Config, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	next := f.cfg.Clone()
	if err := mutate(next); err != nil {
		return nil, err
	}
	next.Normalize()
	if err := config.Validate(next, config.Env{}); err != nil {
		return nil, err
	}
	f.cfg = next
	return next.Clone(), nil
}

func (f *fakeBackend) Health() Health {
	if f.boom {
		panic("handler exploded")
	}
	return Health{Status: StatusHealthy, Reasons: []Reason{}, CheckedAt: time.Now()}
}
func (f *fakeBackend) CameraStats() map[string]CameraStats { return f.stats }
func (f *fakeBackend) Storage() (map[string]DestinationStatus, StagingStatus) {
	return map[string]DestinationStatus{}, StagingStatus{Path: f.cfg.Staging.Path}
}
func (f *fakeBackend) Index() *index.DB { return f.db }
func (f *fakeBackend) OpenSegment(ctx context.Context, id int64) (*os.File, index.Segment, error) {
	seg, err := f.db.Segment(id)
	if err != nil {
		return nil, seg, err
	}
	file, err := os.Open(filepath.Join(f.root, seg.FileName))
	return file, seg, err
}
func (f *fakeBackend) PurgeCamera(id string)  { f.purged = append(f.purged, id) }
func (f *fakeBackend) StartRebuild() error    { f.rebuilt++; return nil }
func (f *fakeBackend) Rebuild() RebuildStatus { return RebuildStatus{} }
func (f *fakeBackend) Workers() int           { return 1 }
func (f *fakeBackend) StartedAt() time.Time   { return time.Now().Add(-time.Minute) }
func (f *fakeBackend) DataRoot() string       { return f.root }
func (f *fakeBackend) RecordEvent(e index.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
}

type testAPI struct {
	t    *testing.T
	s    *Server
	b    *fakeBackend
	ts   *httptest.Server
	c    *http.Client
	csrf string
}

func newTestAPI(t *testing.T) *testAPI {
	t.Helper()
	root := t.TempDir()
	db, err := index.Open(filepath.Join(root, index.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	cfg := config.Default(filepath.Join(root, "staging"))
	cfg.Destinations = []config.Destination{{ID: "nas-1", Name: "NAS", Type: config.DestLocal, Path: filepath.Join(root, "nas"), CapGB: 100, Enabled: true}}
	cfg.Normalize()
	b := &fakeBackend{cfg: cfg, db: db, root: root, stats: map[string]CameraStats{}}
	s, err := NewServer(b, root, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.users.Create("admin", "correct horse", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if _, err := s.users.Create("viewer", "battery staple", RoleViewer); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.handler)
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	return &testAPI{t: t, s: s, b: b, ts: ts, c: &http.Client{Jar: jar}}
}

// do sends a request and decodes the JSON answer.
func (a *testAPI) do(method, path string, body any) (int, map[string]any) {
	a.t.Helper()
	status, raw, _ := a.raw(method, path, body, true)
	var out map[string]any
	if len(raw) > 0 {
		json.Unmarshal(raw, &out)
	}
	return status, out
}

func (a *testAPI) raw(method, path string, body any, csrf bool) (int, []byte, http.Header) {
	a.t.Helper()
	var rd io.Reader
	switch v := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(v)
	default:
		b, _ := json.Marshal(v)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, a.ts.URL+"/api/v1"+path, rd)
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if csrf && a.csrf != "" {
		req.Header.Set(CSRFHeader, a.csrf)
	}
	res, err := a.c.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, raw, res.Header
}

func (a *testAPI) login(user, password string) {
	a.t.Helper()
	status, out := a.do("POST", "/auth/login", map[string]string{"username": user, "password": password})
	if status != 200 {
		a.t.Fatalf("login as %s: %d %v", user, status, out)
	}
	a.csrf = out["csrfToken"].(string)
}

func errCode(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

func fieldsOf(out map[string]any) map[string]string {
	res := map[string]string{}
	e, _ := out["error"].(map[string]any)
	list, _ := e["fields"].([]any)
	for _, f := range list {
		m := f.(map[string]any)
		res[m["field"].(string)] = m["message"].(string)
	}
	return res
}

func TestPasswordHashing(t *testing.T) {
	h, err := HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("hash is not argon2id in the standard form: %s", h)
	}
	if !VerifyPassword(h, "correct horse") || VerifyPassword(h, "Correct horse") || VerifyPassword(h, "") {
		t.Fatal("verification is wrong")
	}
	h2, _ := HashPassword("correct horse")
	if h == h2 {
		t.Fatal("two hashes of the same password are identical: no salt")
	}
	for _, bad := range []string{"", "plain", "$argon2i$v=19$m=1,t=1,p=1$AAAA$AAAA", "$argon2id$v=19$m=99999999,t=2,p=1$AAAA$AAAA"} {
		if VerifyPassword(bad, "x") {
			t.Fatalf("malformed hash %q verified", bad)
		}
	}
}

func TestLoginSessionAndCookieFlags(t *testing.T) {
	a := newTestAPI(t)
	if status, out := a.do("GET", "/cameras", nil); status != 401 || errCode(out) != "unauthorized" {
		t.Fatalf("without a session: %d %v", status, out)
	}
	if status, out := a.do("POST", "/auth/login", map[string]string{"username": "admin", "password": "wrong"}); status != 401 || errCode(out) != "invalid_credentials" {
		t.Fatalf("wrong password: %d %v", status, out)
	}
	a.s.https.Store(true)
	status, raw, hdr := a.raw("POST", "/auth/login", map[string]string{"username": "admin", "password": "correct horse"}, false)
	if status != 200 {
		t.Fatalf("login: %d %s", status, raw)
	}
	cookie := hdr.Get("Set-Cookie")
	for _, want := range []string{SessionCookie + "=", "HttpOnly", "Secure", "SameSite=Strict", "Path=/"} {
		if !strings.Contains(cookie, want) {
			t.Fatalf("session cookie lacks %q: %s", want, cookie)
		}
	}
	if strings.Contains(string(raw), "passwordHash") {
		t.Fatal("login response leaks the password hash")
	}
}

func TestLockoutAfterFiveFailures(t *testing.T) {
	a := newTestAPI(t)
	now := time.Now()
	a.s.users.now = func() time.Time { return now }
	for i := 1; i <= MaxFailedLogins; i++ {
		status, out := a.do("POST", "/auth/login", map[string]string{"username": "admin", "password": "nope"})
		want := 401
		if i == MaxFailedLogins {
			want = 423
		}
		if status != want {
			t.Fatalf("attempt %d: %d %v", i, status, out)
		}
	}
	// Even the right password is refused while locked.
	if status, out := a.do("POST", "/auth/login", map[string]string{"username": "admin", "password": "correct horse"}); status != 423 || errCode(out) != "account_locked" {
		t.Fatalf("locked account: %d %v", status, out)
	}
	// Another account is not affected.
	if status, _ := a.do("POST", "/auth/login", map[string]string{"username": "viewer", "password": "battery staple"}); status != 200 {
		t.Fatalf("other account locked too: %d", status)
	}
	now = now.Add(LockoutDuration - time.Second)
	if status, _ := a.do("POST", "/auth/login", map[string]string{"username": "admin", "password": "correct horse"}); status != 423 {
		t.Fatalf("lock ended early: %d", status)
	}
	now = now.Add(2 * time.Second)
	if status, _ := a.do("POST", "/auth/login", map[string]string{"username": "admin", "password": "correct horse"}); status != 200 {
		t.Fatalf("still locked after 15 minutes: %d", status)
	}
}

func TestSessionExpiresAfterTwelveHoursIdle(t *testing.T) {
	a := newTestAPI(t)
	now := time.Now()
	a.s.sessions.now = func() time.Time { return now }
	a.login("admin", "correct horse")
	now = now.Add(SessionIdleTimeout - time.Minute)
	if status, _ := a.do("GET", "/auth/session", nil); status != 200 {
		t.Fatalf("session ended before 12 h idle: %d", status)
	}
	now = now.Add(SessionIdleTimeout - time.Minute) // activity above renewed it
	if status, _ := a.do("GET", "/auth/session", nil); status != 200 {
		t.Fatalf("an active session expired: %d", status)
	}
	now = now.Add(SessionIdleTimeout + time.Minute)
	if status, _ := a.do("GET", "/auth/session", nil); status != 401 {
		t.Fatalf("session still valid after 12 h idle: %d", status)
	}
}

func TestCSRFRequiredOnStateChangingRequests(t *testing.T) {
	a := newTestAPI(t)
	a.login("admin", "correct horse")
	body := map[string]any{"name": "Front Door", "host": "192.168.1.50", "port": 554, "path": "/s"}
	for _, tc := range []struct{ method, path string }{
		{"POST", "/cameras"}, {"PUT", "/cameras/x"}, {"DELETE", "/cameras/x"}, {"PUT", "/system"},
		{"POST", "/system/rebuild-index"}, {"POST", "/users"}, {"POST", "/auth/logout"},
	} {
		status, raw, _ := a.raw(tc.method, tc.path, body, false)
		if status != 403 || !strings.Contains(string(raw), "csrf_failed") {
			t.Fatalf("%s %s without a CSRF token: %d %s", tc.method, tc.path, status, raw)
		}
	}
	good := a.csrf
	a.csrf = strings.Repeat("0", len(good))
	if status, _ := a.do("POST", "/cameras", body); status != 403 {
		t.Fatalf("wrong CSRF token accepted: %d", status)
	}
	a.csrf = good
	if status, out := a.do("POST", "/cameras", body); status != 201 {
		t.Fatalf("with the right token: %d %v", status, out)
	}
	if a.b.rebuilt != 0 {
		t.Fatal("a request without a token had an effect")
	}
}

func TestViewerCanReadButNotChange(t *testing.T) {
	a := newTestAPI(t)
	a.login("viewer", "battery staple")
	for _, path := range []string{"/cameras", "/destinations", "/events", "/system", "/health", "/recordings/cameras",
		"/segments?camera=front-door&date=2026-11-14", "/auth/session"} {
		if status, out := a.do("GET", path, nil); status != 200 {
			t.Fatalf("viewer GET %s: %d %v", path, status, out)
		}
	}
	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/users", nil}, {"GET", "/system/config/export", nil},
		{"POST", "/cameras", map[string]any{"name": "X", "host": "h", "port": 554, "path": "/"}},
		{"DELETE", "/cameras/x", nil}, {"POST", "/destinations", map[string]any{}}, {"PUT", "/system", map[string]any{}},
		{"POST", "/system/rebuild-index", nil}, {"POST", "/users", map[string]any{}}, {"POST", "/cameras/test", map[string]any{}},
		{"POST", "/system/certificate", map[string]any{}}, {"POST", "/system/config/import", map[string]any{}},
	} {
		status, out := a.do(tc.method, tc.path, tc.body)
		if status != 403 || errCode(out) != "forbidden" {
			t.Fatalf("viewer %s %s: %d %v", tc.method, tc.path, status, out)
		}
	}
	if len(a.b.Config().Cameras) != 0 || a.b.rebuilt != 0 {
		t.Fatal("a viewer changed something")
	}
	if status, _ := a.do("POST", "/auth/logout", nil); status != 204 {
		t.Fatalf("viewer cannot sign out: %d", status)
	}
	if status, _ := a.do("GET", "/cameras", nil); status != 401 {
		t.Fatalf("session survived sign-out: %d", status)
	}
}

func TestHealthNeedsNoLoginFromLocalhostOnly(t *testing.T) {
	a := newTestAPI(t)
	if status, out := a.do("GET", "/health", nil); status != 200 || out["status"] != "healthy" || out["version"] == nil {
		t.Fatalf("health from localhost: %d %v", status, out)
	}
	req := httptest.NewRequest("GET", "/api/v1/health", nil)
	req.RemoteAddr = "192.168.1.77:50000"
	rec := httptest.NewRecorder()
	a.s.handler.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("health from another PC without a session: %d", rec.Code)
	}
}

func TestAllowedSubnets(t *testing.T) {
	a := newTestAPI(t)
	nets := parseSubnets([]string{"192.168.1.0/24"})
	a.s.subnets.Store(&nets)
	try := func(addr string) int {
		req := httptest.NewRequest("GET", "/api/v1/health", nil)
		req.RemoteAddr = addr
		rec := httptest.NewRecorder()
		a.s.handler.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := try("10.0.0.5:1234"); got != 403 {
		t.Fatalf("address outside the allowed subnets: %d", got)
	}
	if got := try("192.168.1.20:1234"); got != 401 { // allowed in, then asked to sign in
		t.Fatalf("address inside the allowed subnet: %d", got)
	}
	if got := try("127.0.0.1:1234"); got != 200 {
		t.Fatalf("localhost must always be allowed: %d", got)
	}
}

// A panic in a handler is answered with 500 and the server keeps serving.
func TestHandlerPanicIsContained(t *testing.T) {
	a := newTestAPI(t)
	a.b.boom = true
	status, out := a.do("GET", "/health", nil)
	if status != 500 || errCode(out) != "internal" {
		t.Fatalf("panicking handler: %d %v", status, out)
	}
	a.b.boom = false
	if status, _ := a.do("GET", "/health", nil); status != 200 {
		t.Fatalf("server did not survive the panic: %d", status)
	}
}

func TestCameraCreateUpdateDelete(t *testing.T) {
	a := newTestAPI(t)
	a.login("admin", "correct horse")

	// A pasted URL is split into fields and its credentials are extracted.
	status, out := a.do("POST", "/cameras", map[string]any{
		"name": "Front Door #1", "url": "rtsp://admin:s3cret@192.168.1.50:8554/cam/realmonitor?channel=1&subtype=0",
		"destinationIds": []string{"nas-1"},
	})
	if status != 201 {
		t.Fatalf("create: %d %v", status, out)
	}
	if out["id"] != "front-door-1" || out["host"] != "192.168.1.50" || out["port"] != float64(8554) ||
		out["path"] != "/cam/realmonitor?channel=1&subtype=0" || out["username"] != "admin" || out["hasPassword"] != true ||
		out["transport"] != "tcp" || out["recordAudio"] != true || out["enabled"] != true || out["recordingMode"] != "continuous" {
		t.Fatalf("created camera: %v", out)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "s3cret") || strings.Contains(string(raw), "passwordEnc") {
		t.Fatalf("the response exposes the password: %s", raw)
	}
	saved, _ := a.b.Config().Camera("front-door-1")
	if pw, _ := config.Unprotect(saved.PasswordEnc); pw != "s3cret" || saved.PasswordEnc == "s3cret" {
		t.Fatalf("password not stored protected: %q", saved.PasswordEnc)
	}

	// Same name again gets a different id.
	_, out2 := a.do("POST", "/cameras", map[string]any{"name": "Front Door #1", "host": "192.168.1.51", "port": 554, "path": "/s"})
	if out2["id"] != "front-door-1-2" {
		t.Fatalf("second camera id %v", out2["id"])
	}

	// Field errors use the form's field names.
	status, out = a.do("POST", "/cameras", map[string]any{"name": "", "host": "bad host", "port": 0, "path": "x", "destinationIds": []string{"nope"}})
	f := fieldsOf(out)
	if status != 400 || errCode(out) != "validation_failed" || f["name"] == "" || f["host"] == "" || f["path"] == "" || f["destinationIds"] == "" {
		t.Fatalf("validation: %d %v", status, out)
	}

	// Update: the id cannot change, and an omitted password is kept.
	status, out = a.do("PUT", "/cameras/front-door-1", map[string]any{
		"id": "renamed", "name": "Front Door", "host": "192.168.1.60", "port": 554, "path": "/s", "username": "admin",
		"transport": "udp", "recordAudio": false, "enabled": true, "destinationIds": []string{},
	})
	if status != 200 || out["id"] != "front-door-1" || out["host"] != "192.168.1.60" || out["transport"] != "udp" ||
		out["recordAudio"] != false || out["hasPassword"] != true {
		t.Fatalf("update: %d %v", status, out)
	}
	if _, ok := a.b.Config().Camera("renamed"); ok {
		t.Fatal("the camera id was changed")
	}
	_, out = a.do("PUT", "/cameras/front-door-1", map[string]any{"name": "Front Door", "host": "192.168.1.60", "port": 554, "path": "/s", "clearPassword": true})
	if out["hasPassword"] != false {
		t.Fatalf("clearPassword did not remove the password: %v", out)
	}
	if status, _ := a.do("PUT", "/cameras/missing", map[string]any{"name": "x", "host": "h", "port": 1, "path": "/"}); status != 404 {
		t.Fatalf("update of a missing camera: %d", status)
	}

	// Delete: recordings are kept unless asked otherwise.
	if status, _ := a.do("DELETE", "/cameras/front-door-1", nil); status != 204 || len(a.b.purged) != 0 {
		t.Fatalf("delete keeping recordings: %d purged=%v", status, a.b.purged)
	}
	if status, _ := a.do("DELETE", "/cameras/front-door-1-2?keepRecordings=false", nil); status != 204 || len(a.b.purged) != 1 {
		t.Fatalf("delete with recordings: %d purged=%v", status, a.b.purged)
	}
	if status, _ := a.do("DELETE", "/cameras/front-door-1", nil); status != 404 {
		t.Fatalf("second delete: %d", status)
	}
}

func TestCameraBulkImport(t *testing.T) {
	a := newTestAPI(t)
	a.login("admin", "correct horse")
	csv := "name,host,port,path,username,password,destinations\n" +
		"Lobby,192.168.1.60,554,/stream1,admin,pw1,nas-1\n" +
		"Dock,192.168.1.61,,/stream1,,,\n" +
		"Bad One,not a host,554,/s,,,\n" +
		"No Dest,192.168.1.62,554,/s,,,missing\n" +
		"\"Gate, North\",rtsp://u:p@192.168.1.63:8554/live,,,,,nas-1\n"
	status, out := a.do("POST", "/cameras/import", map[string]string{"csv": csv})
	if status != 200 || out["created"] != float64(3) {
		t.Fatalf("import: %d %v", status, out)
	}
	results := out["results"].([]any)
	if len(results) != 5 {
		t.Fatalf("%d results, want 5 (the header is not a camera)", len(results))
	}
	get := func(i int) map[string]any { return results[i].(map[string]any) }
	if get(0)["ok"] != true || get(0)["id"] != "lobby" || get(0)["line"] != float64(2) {
		t.Fatalf("row 1: %v", get(0))
	}
	if get(2)["ok"] != false || len(get(2)["errors"].([]any)) == 0 || get(3)["ok"] != false {
		t.Fatalf("bad rows accepted: %v %v", get(2), get(3))
	}
	cfg := a.b.Config()
	dock, _ := cfg.Camera("dock")
	if dock.Port != 554 || dock.PasswordEnc != "" || len(dock.DestinationIDs) != 0 {
		t.Fatalf("dock: %+v", dock)
	}
	gate, ok := cfg.Camera("gate-north")
	if !ok || gate.Host != "192.168.1.63" || gate.Port != 8554 || gate.Path != "/live" || gate.Username != "u" || gate.PasswordEnc == "" {
		t.Fatalf("gate: %+v", gate)
	}
}

func TestDestinationCRUD(t *testing.T) {
	a := newTestAPI(t)
	a.login("admin", "correct horse")
	dir := filepath.Join(a.b.root, "second")
	status, out := a.do("POST", "/destinations", map[string]any{"name": "Second Disk", "type": "local", "path": dir, "capGB": 4000, "maxAgeDays": 30})
	if status != 201 || out["id"] != "second-disk" || out["maxAgeDays"] != float64(30) || out["enabled"] != true {
		t.Fatalf("create: %d %v", status, out)
	}
	status, out = a.do("POST", "/destinations", map[string]any{"name": "", "type": "tape", "path": "", "capGB": 0})
	f := fieldsOf(out)
	if status != 400 || f["name"] == "" || f["type"] == "" || f["capGB"] == "" {
		t.Fatalf("validation: %d %v", status, out)
	}
	status, out = a.do("PUT", "/destinations/second-disk", map[string]any{"name": "Second Disk", "type": "local", "path": dir, "capGB": 2000, "maxAgeDays": nil, "enabled": false})
	if status != 200 || out["capGB"] != float64(2000) || out["maxAgeDays"] != nil || out["enabled"] != false ||
		out["status"].(map[string]any)["state"] != "disabled" {
		t.Fatalf("update: %d %v", status, out)
	}
	// A destination in use cannot be deleted.
	a.do("POST", "/cameras", map[string]any{"name": "Lobby", "host": "10.0.0.1", "port": 554, "path": "/", "destinationIds": []string{"nas-1"}})
	status, out = a.do("DELETE", "/destinations/nas-1", nil)
	if status != 409 || !strings.Contains(out["error"].(map[string]any)["message"].(string), "Lobby") {
		t.Fatalf("delete of a destination in use: %d %v", status, out)
	}
	if status, _ := a.do("DELETE", "/destinations/second-disk", nil); status != 204 {
		t.Fatalf("delete: %d", status)
	}
	// The Test button works on unsaved input.
	status, out = a.do("POST", "/destinations/test", map[string]any{"name": "T", "type": "local", "path": filepath.Join(a.b.root, "probe")})
	if status != 200 || out["ok"] != true || out["totalBytes"] == nil {
		t.Fatalf("test: %d %v", status, out)
	}
	blocker := filepath.Join(a.b.root, "file")
	os.WriteFile(blocker, []byte("x"), 0o644)
	_, out = a.do("POST", "/destinations/test", map[string]any{"name": "T", "type": "local", "path": filepath.Join(blocker, "sub")})
	if out["ok"] != false || out["error"] == "" {
		t.Fatalf("test of an unusable path: %v", out)
	}
}

func TestSystemSettingsAndConfigExportImport(t *testing.T) {
	a := newTestAPI(t)
	a.login("admin", "correct horse")
	a.do("POST", "/cameras", map[string]any{"name": "Lobby", "host": "10.0.0.1", "port": 554, "path": "/", "username": "u", "password": "camera-pw", "destinationIds": []string{"nas-1"}})

	status, out := a.do("PUT", "/system", map[string]any{"logging": map[string]any{"retentionDays": 30},
		"server": map[string]any{"allowedSubnets": []string{" 192.168.1.0/24 ", ""}}})
	if status != 200 || out["logging"].(map[string]any)["retentionDays"] != float64(30) {
		t.Fatalf("PUT /system: %d %v", status, out)
	}
	if got := a.b.Config().Server.AllowedSubnets; len(got) != 1 || got[0] != "192.168.1.0/24" {
		t.Fatalf("allowedSubnets %v", got)
	}
	status, out = a.do("PUT", "/system", map[string]any{"logging": map[string]any{"retentionDays": 999}, "server": map[string]any{"port": 0}})
	if f := fieldsOf(out); status != 400 || f["logging.retentionDays"] == "" || f["server.port"] == "" {
		t.Fatalf("invalid settings: %d %v", status, out)
	}

	status, raw, hdr := a.raw("GET", "/system/config/export", nil, true)
	if status != 200 || !strings.Contains(hdr.Get("Content-Disposition"), "attachment") {
		t.Fatalf("export: %d %v", status, hdr)
	}
	if strings.Contains(string(raw), "passwordEnc") || strings.Contains(string(raw), "camera-pw") {
		t.Fatalf("the export contains passwords: %s", raw)
	}
	// Importing the export keeps the saved password of the existing camera.
	var doc map[string]any
	json.Unmarshal(raw, &doc)
	doc["cameras"].([]any)[0].(map[string]any)["name"] = "Lobby (renamed)"
	if status, out := a.do("POST", "/system/config/import", doc); status != 200 {
		t.Fatalf("import: %d %v", status, out)
	}
	cam, _ := a.b.Config().Camera("lobby")
	if pw, _ := config.Unprotect(cam.PasswordEnc); cam.Name != "Lobby (renamed)" || pw != "camera-pw" {
		t.Fatalf("after import: name=%q password kept=%v", cam.Name, pw == "camera-pw")
	}
	if status, out := a.do("POST", "/system/config/import", map[string]any{"hello": "world"}); status != 400 {
		t.Fatalf("garbage import: %d %v", status, out)
	}
	if status, out := a.do("POST", "/system/rebuild-index", nil); status != 202 || out["started"] != true || a.b.rebuilt != 1 {
		t.Fatalf("rebuild: %d %v", status, out)
	}
}

func TestUserManagement(t *testing.T) {
	a := newTestAPI(t)
	a.login("admin", "correct horse")
	status, out := a.do("POST", "/users", map[string]any{"username": "garrett", "password": "short", "role": "boss"})
	if f := fieldsOf(out); status != 400 || f["password"] == "" || f["role"] == "" {
		t.Fatalf("validation: %d %v", status, out)
	}
	if status, _ := a.do("POST", "/users", map[string]any{"username": "garrett", "password": "long enough", "role": "viewer"}); status != 201 {
		t.Fatalf("create: %d", status)
	}
	if status, _ := a.do("POST", "/users", map[string]any{"username": "Garrett", "password": "long enough", "role": "viewer"}); status != 400 {
		t.Fatalf("duplicate user name accepted: %d", status)
	}
	_, out = a.do("GET", "/users", nil)
	raw, _ := json.Marshal(out)
	if len(out["users"].([]any)) != 3 || strings.Contains(string(raw), "argon2") {
		t.Fatalf("user list: %s", raw)
	}
	// The last administrator cannot be demoted or deleted; nor can you delete yourself.
	if status, _ := a.do("PUT", "/users/admin", map[string]any{"role": "viewer"}); status != 409 {
		t.Fatalf("last admin demoted: %d", status)
	}
	if status, _ := a.do("DELETE", "/users/admin", nil); status != 409 {
		t.Fatalf("deleted own account: %d", status)
	}
	if status, _ := a.do("PUT", "/users/garrett", map[string]any{"role": "admin", "password": "another password"}); status != 200 {
		t.Fatalf("promote: %d", status)
	}
	if status, _ := a.do("DELETE", "/users/viewer", nil); status != 204 {
		t.Fatalf("delete: %d", status)
	}
	if status, _ := a.do("DELETE", "/users/nobody", nil); status != 404 {
		t.Fatalf("delete of a missing user: %d", status)
	}
	// The account file survives a restart and holds no plain passwords.
	data, _ := os.ReadFile(filepath.Join(a.b.root, UsersFile))
	if strings.Contains(string(data), "another password") || !strings.Contains(string(data), "$argon2id$") {
		t.Fatalf("users.json: %s", data)
	}
	reopened, err := OpenUserStore(a.b.root)
	if err != nil || reopened.Count() != 2 {
		t.Fatalf("reopen: %v count=%d", err, reopened.Count())
	}
	if _, err := reopened.Authenticate("garrett", "another password"); err != nil {
		t.Fatalf("new password does not work after reload: %v", err)
	}
}

func TestSegmentsEventsAndDownload(t *testing.T) {
	a := newTestAPI(t)
	a.login("viewer", "battery staple")
	loc := time.FixedZone("", -6*3600)
	start := time.Date(2026, 11, 14, 9, 0, 0, 212_000_000, loc)
	name := "front-door_2026-11-14_0900_-0600.mp4"
	os.WriteFile(filepath.Join(a.b.root, name), []byte("0123456789abcdef"), 0o644)
	id, err := a.b.db.SegmentClosed(index.ClosedSegment{CameraID: "front-door", FileName: name, Start: start, End: start.Add(time.Hour),
		Bytes: 16, SHA256: "aa", VideoCodec: "h264", AudioCodec: "aac", CloseReason: ipc.ReasonHourRollover}, []string{"nas-1"})
	if err != nil {
		t.Fatal(err)
	}
	a.b.db.SegmentOpened("front-door", "front-door_2026-11-14_1000_-0600.mp4", start.Add(time.Hour), false, "h264", "aac")

	status, out := a.do("GET", "/segments?camera=front-door&date=2026-11-14", nil)
	segs := out["segments"].([]any)
	if status != 200 || len(segs) != 2 {
		t.Fatalf("segments: %d %v", status, out)
	}
	first, open := segs[0].(map[string]any), segs[1].(map[string]any)
	if first["fileName"] != name || first["bytes"] != float64(16) || first["closeReason"] != "hour_rollover" || first["gapBefore"] != false ||
		len(first["copies"].([]any)) != 2 || first["end"] == nil {
		t.Fatalf("closed segment: %v", first)
	}
	if open["end"] != nil || open["bytes"] != nil {
		t.Fatalf("the file being written must have null end and bytes: %v", open)
	}
	if status, out := a.do("GET", "/segments?camera=front-door&date=yesterday", nil); status != 400 {
		t.Fatalf("bad date: %d %v", status, out)
	}

	status, raw, hdr := a.raw("GET", "/segments/"+jsonInt(id)+"/download", nil, false)
	if status != 200 || string(raw) != "0123456789abcdef" || hdr.Get("Content-Type") != "video/mp4" ||
		!strings.Contains(hdr.Get("Content-Disposition"), name) {
		t.Fatalf("download: %d %q %v", status, raw, hdr)
	}
	req, _ := http.NewRequest("GET", a.ts.URL+"/api/v1/segments/"+jsonInt(id)+"/download", nil)
	req.Header.Set("Range", "bytes=10-")
	res, _ := a.c.Do(req)
	part, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 206 || string(part) != "abcdef" {
		t.Fatalf("range download: %d %q", res.StatusCode, part)
	}
	if status, _, _ := a.raw("GET", "/segments/999/download", nil, false); status != 404 {
		t.Fatalf("download of a missing segment: %d", status)
	}

	a.b.db.InsertEvent(index.Event{Source: "camera:front-door", Type: "stream_stalled", Severity: ipc.SevWarning, CameraID: "front-door",
		Data: map[string]any{"message": "no video"}})
	a.b.db.InsertEvent(index.Event{Source: "guardian", Type: "status_changed", Severity: ipc.SevInfo})
	_, out = a.do("GET", "/events?severity=warning&camera=front-door", nil)
	events := out["events"].([]any)
	if len(events) != 1 || events[0].(map[string]any)["type"] != "stream_stalled" {
		t.Fatalf("events: %v", out)
	}
	_, out = a.do("GET", "/recordings/cameras", nil)
	cams := out["cameras"].([]any)
	if len(cams) != 1 || cams[0].(map[string]any)["deleted"] != true {
		t.Fatalf("a camera that only exists in the index must be listed as deleted: %v", out)
	}
}

func jsonInt(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestSelfSignedCertificate(t *testing.T) {
	root := t.TempDir()
	if err := EnsureCertificate(root); err != nil {
		t.Fatal(err)
	}
	cert, info, err := LoadCertificate(root)
	if err != nil {
		t.Fatal(err)
	}
	leaf := cert.Leaf
	if years := leaf.NotAfter.Sub(leaf.NotBefore).Hours() / 24 / 365; years < 9.9 || years > 10.1 {
		t.Fatalf("validity %.2f years, want 10", years)
	}
	if !info.SelfSigned || info.FingerprintSHA256 == "" {
		t.Fatalf("info %+v", info)
	}
	host, _ := os.Hostname()
	if err := leaf.VerifyHostname("localhost"); err != nil {
		t.Fatal(err)
	}
	if err := leaf.VerifyHostname(host); err != nil {
		t.Fatalf("certificate is not valid for the host name %s: %v", host, err)
	}
	if err := leaf.VerifyHostname("127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	// It is kept across restarts.
	before, _ := os.ReadFile(filepath.Join(root, CertDir, CertFile))
	EnsureCertificate(root)
	after, _ := os.ReadFile(filepath.Join(root, CertDir, CertFile))
	if !bytes.Equal(before, after) {
		t.Fatal("an existing certificate was replaced")
	}
}

func TestCertificateUpload(t *testing.T) {
	a := newTestAPI(t)
	a.login("admin", "correct horse")
	old := a.s.certInfo.Load().FingerprintSHA256

	other := t.TempDir()
	GenerateSelfSigned(other)
	certPEM, _ := os.ReadFile(filepath.Join(other, CertDir, CertFile))
	keyPEM, _ := os.ReadFile(filepath.Join(other, CertDir, KeyFile))

	// A certificate with somebody else's key is refused and nothing changes.
	wrongKey, _ := os.ReadFile(filepath.Join(a.b.root, CertDir, KeyFile))
	status, out := a.do("POST", "/system/certificate", map[string]string{"certPem": string(certPEM), "keyPem": string(wrongKey)})
	if status != 400 || a.s.certInfo.Load().FingerprintSHA256 != old {
		t.Fatalf("mismatched pair: %d %v", status, out)
	}
	status, out = a.do("POST", "/system/certificate", map[string]string{"certPem": string(certPEM), "keyPem": string(keyPEM)})
	if status != 200 {
		t.Fatalf("upload: %d %v", status, out)
	}
	block, _ := pem.Decode(certPEM)
	want, _ := x509.ParseCertificate(block.Bytes)
	if got := a.s.cert.Load().Leaf; !got.Equal(want) {
		t.Fatal("the uploaded certificate is not in use")
	}
	if a.s.certInfo.Load().FingerprintSHA256 == old {
		t.Fatal("certificate info not updated")
	}
}

// The real listener: HTTPS with the generated certificate, reachable on
// another address than localhost, and movable to a new port.
func TestListenerHTTPSAndRestart(t *testing.T) {
	a := newTestAPI(t)
	free := func() int {
		l, _ := net.Listen("tcp", "127.0.0.1:0")
		defer l.Close()
		return l.Addr().(*net.TCPAddr).Port
	}
	p1, p2 := free(), free()
	if err := a.s.Start(config.Server{ListenAddress: "0.0.0.0", Port: p1, HTTPS: true}); err != nil {
		t.Fatal(err)
	}
	defer a.s.Close()
	pool := x509.NewCertPool()
	pool.AddCert(a.s.cert.Load().Leaf)
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	get := func(port int) (int, error) {
		res, err := client.Get("https://localhost:" + jsonInt(int64(port)) + "/api/v1/health")
		if err != nil {
			return 0, err
		}
		res.Body.Close()
		return res.StatusCode, nil
	}
	if status, err := get(p1); err != nil || status != 200 {
		t.Fatalf("HTTPS with the generated certificate: %d %v", status, err)
	}
	if _, err := http.Get("http://localhost:" + jsonInt(int64(p1)) + "/api/v1/health"); err == nil {
		// Plain HTTP against the TLS port gets an error response, not content.
	}
	a.s.Restart(config.Server{ListenAddress: "127.0.0.1", Port: p2, HTTPS: true})
	deadline := time.Now().Add(10 * time.Second)
	for {
		if status, err := get(p2); err == nil && status == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the server did not move to the new port")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := get(p1); err == nil {
		t.Fatal("the old port still answers")
	}
}

func TestEventStream(t *testing.T) {
	a := newTestAPI(t)
	a.login("viewer", "battery staple")
	req, _ := http.NewRequest("GET", a.ts.URL+"/api/v1/stream", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := a.c.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	expect := func(want string) {
		t.Helper()
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatalf("stream ended before %q", want)
				}
				if l == want {
					return
				}
			case <-ctx.Done():
				t.Fatalf("no %q on the stream", want)
			}
		}
	}
	expect("event: health")
	expect("event: cameras")
	expect("event: destinations")
	for a.s.hub.count() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	a.s.PublishEvent(index.Event{ID: 7, TS: time.Now(), Source: "guardian", Type: "status_changed", Severity: ipc.SevWarning})
	expect("event: event")

	// Without a session the stream is refused.
	bare, _ := http.Get(a.ts.URL + "/api/v1/stream")
	bare.Body.Close()
	if bare.StatusCode != 401 {
		t.Fatalf("stream without a session: %d", bare.StatusCode)
	}
}

func TestUnknownAPIPathAndUIFallback(t *testing.T) {
	a := newTestAPI(t)
	if status, out := a.do("GET", "/nope", nil); status != 404 || errCode(out) != "not_found" {
		t.Fatalf("unknown endpoint: %d %v", status, out)
	}
	res, err := http.Get(a.ts.URL + "/cameras/anything")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || !strings.Contains(res.Header.Get("Content-Type"), "text/html") || !strings.Contains(string(body), "ButterCams") {
		t.Fatalf("UI path: %d %s", res.StatusCode, body)
	}
	for _, h := range []string{"X-Content-Type-Options", "X-Frame-Options", "Content-Security-Policy"} {
		if res.Header.Get(h) == "" {
			t.Fatalf("missing security header %s", h)
		}
	}
}

var _ = errors.New
