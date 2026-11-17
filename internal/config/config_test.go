package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var (
	yes     = true
	no      = false
	winEnv  = Env{Windows: &yes, IsRemoteDrive: func(p string) bool { return strings.HasPrefix(strings.ToUpper(p), "Z:") }}
	unixEnv = Env{Windows: &no}
	// nativeEnv uses the path rules of the system the tests run on; the store
	// tests need it because they work in real temporary folders.
	nativeEnv = Env{}
)

func validWin() *Config {
	c := Default(`D:\ButterCams\staging`)
	days := 30
	c.Destinations = []Destination{
		{ID: "nas-1", Name: "NAS main share", Type: DestUNC, Path: `\\nas01\cameras`, Username: `CORP\svc`, PasswordEnc: "x", CapGB: 20000, Enabled: true},
		{ID: "local-d", Name: "Local D drive", Type: DestLocal, Path: `E:\Recordings`, CapGB: 4000, MaxAgeDays: &days, Enabled: true},
	}
	c.Cameras = []Camera{{
		ID: "front-door", Name: "Front Door", Enabled: true, Host: "192.168.1.50", Port: 554,
		Path: "/cam/realmonitor?channel=1&subtype=0", Username: "admin", PasswordEnc: "x",
		Transport: "tcp", RecordAudio: true, DestinationIDs: []string{"nas-1"},
	}}
	c.Normalize()
	return c
}

func fieldsOf(t *testing.T, err error) map[string]string {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected a ValidationError, got %v", err)
	}
	out := map[string]string{}
	for _, f := range ve.Fields {
		out[f.Field] = f.Message
	}
	return out
}

func TestValidateAcceptsSpecExample(t *testing.T) {
	if err := Validate(validWin(), winEnv); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRejectsMappedDrive(t *testing.T) {
	c := validWin()
	c.Destinations[1].Path = `Z:\Recordings`
	f := fieldsOf(t, Validate(c, winEnv))
	if f["destinations[1].path"] != MappedDriveMessage {
		t.Fatalf("got %q", f["destinations[1].path"])
	}
	if !strings.Contains(MappedDriveMessage, `\\server\share`) {
		t.Fatalf("message must point at the UNC form: %q", MappedDriveMessage)
	}
}

func TestValidateFieldErrors(t *testing.T) {
	c := validWin()
	c.Server.Port = 70000
	c.Server.ListenAddress = "everywhere"
	c.Server.AllowedSubnets = []string{"192.168.1.0/24", "10.0.0.1"}
	c.Staging.Path = `\\nas01\staging`
	c.Staging.MinFreePercent = 95
	c.Logging.RetentionDays = 400
	c.Destinations[0].Path = `E:\share`
	c.Destinations[0].CapGB = 0
	zero := 0
	c.Destinations[1].MaxAgeDays = &zero
	c.Destinations[1].ID = "staging"
	c.Cameras[0].ID = "Front Door"
	c.Cameras[0].Host = "rtsp://192.168.1.50"
	c.Cameras[0].Port = 0
	c.Cameras[0].Path = "stream"
	c.Cameras[0].Transport = "http"
	c.Cameras[0].RecordingMode = "motion_only"
	c.Cameras[0].DestinationIDs = []string{"nope"}

	f := fieldsOf(t, Validate(c, winEnv))
	for _, field := range []string{
		"server.port", "server.listenAddress", "server.allowedSubnets[1]", "staging.path",
		"staging.minFreePercent", "logging.retentionDays", "destinations[0].path", "destinations[0].capGB",
		"destinations[1].maxAgeDays", "destinations[1].id", "cameras[0].id", "cameras[0].host",
		"cameras[0].port", "cameras[0].path", "cameras[0].transport", "cameras[0].recordingMode",
		"cameras[0].destinationIds",
	} {
		if f[field] == "" {
			t.Errorf("expected an error for %s; got %v", field, f)
		}
	}
	if _, ok := f["server.allowedSubnets[0]"]; ok {
		t.Errorf("a valid CIDR was rejected")
	}
}

func TestValidateDuplicateIDs(t *testing.T) {
	c := validWin()
	c.Cameras = append(c.Cameras, c.Cameras[0])
	c.Destinations = append(c.Destinations, c.Destinations[0])
	f := fieldsOf(t, Validate(c, winEnv))
	if f["cameras[1].id"] == "" || f["destinations[2].id"] == "" {
		t.Fatalf("duplicates not reported: %v", f)
	}
}

func TestReservedFieldsRoundTrip(t *testing.T) {
	raw := `{"schemaVersion":1,"server":{"listenAddress":"0.0.0.0","port":8650,"https":true,"allowedSubnets":[]},
	"staging":{"path":"/var/staging","minFreePercent":10,"minFreeGB":50},"destinations":[],
	"cameras":[{"id":"cam","name":"Cam","enabled":true,"host":"10.0.0.2","port":554,"path":"/s","transport":"tcp",
	"recordAudio":true,"destinationIds":[],"subStreamPath":"/sub","recordingMode":"continuous",
	"extensions":{"motion":{"sensitivity":3}}}]}`
	cfg, _, err := Parse([]byte(raw), unixEnv)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(cfg)
	cfg2, _, err := Parse(out, unixEnv)
	if err != nil {
		t.Fatal(err)
	}
	cam := cfg2.Cameras[0]
	if cam.SubStreamPath == nil || *cam.SubStreamPath != "/sub" {
		t.Fatalf("subStreamPath lost: %+v", cam.SubStreamPath)
	}
	if string(cam.Extensions["motion"]) != `{"sensitivity":3}` {
		t.Fatalf("extensions lost: %s", cam.Extensions["motion"])
	}
	if cfg2.Logging.RetentionDays != DefaultLogRetentionDays {
		t.Fatalf("logging default not applied")
	}
}

func TestStoreSaveKeepsTenBackupsAndIsAtomic(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir, nativeEnv)
	res, err := s.Load(filepath.Join(dir, "staging"))
	if err != nil || !res.Created {
		t.Fatalf("first load: %+v %v", res, err)
	}
	cfg := res.Config
	for i := 0; i < 14; i++ {
		cfg.Server.Port = 9000 + i
		if err := s.Save(cfg); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(s.Path() + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temporary file left behind")
	}
	for n := 1; n <= Backups; n++ {
		b, err := os.ReadFile(s.backupPath(n))
		if err != nil {
			t.Fatalf("backup %d missing: %v", n, err)
		}
		var c Config
		json.Unmarshal(b, &c)
		if want := 9000 + 13 - n; c.Server.Port != want {
			t.Fatalf("backup %d has port %d, want %d", n, c.Server.Port, want)
		}
	}
	if _, err := os.Stat(s.backupPath(Backups + 1)); !os.IsNotExist(err) {
		t.Fatalf("more than %d backups kept", Backups)
	}
}

func TestStoreRejectsInvalidAndLeavesFileUntouched(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir, nativeEnv)
	cfg := Default(filepath.Join(dir, "staging"))
	if err := s.Save(cfg); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(s.Path())
	bad := cfg.Clone()
	bad.Server.Port = -1
	if err := s.Save(bad); err == nil {
		t.Fatal("invalid config was saved")
	}
	after, _ := os.ReadFile(s.Path())
	if string(before) != string(after) {
		t.Fatal("config.json changed after a rejected save")
	}
}

func TestStoreFallsBackToNewestReadableBackup(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir, nativeEnv)
	cfg := Default(filepath.Join(dir, "staging"))
	for _, port := range []int{8001, 8002, 8003} {
		cfg.Server.Port = port
		if err := s.Save(cfg); err != nil {
			t.Fatal(err)
		}
	}
	// Corrupt the live file and the newest backup.
	os.WriteFile(s.Path(), []byte(`{"schemaVersion": 1, "server": `), 0o600)
	os.WriteFile(s.backupPath(1), []byte("garbage"), 0o600)
	res, err := s.Load(filepath.Join(dir, "staging"))
	if err != nil {
		t.Fatal(err)
	}
	if res.RecoveredFrom != "config.json.2" || res.Config.Server.Port != 8001 || res.Problem == "" {
		t.Fatalf("unexpected recovery: %+v port=%d", res, res.Config.Server.Port)
	}
}

func TestStoreFailsWhenNothingIsReadable(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir, nativeEnv)
	os.WriteFile(s.Path(), []byte("not json"), 0o600)
	if _, err := s.Load(filepath.Join(dir, "staging")); err == nil {
		t.Fatal("expected an error")
	}
}

func TestParseRTSPURL(t *testing.T) {
	cases := []struct {
		in   string
		want RTSPParts
	}{
		{"rtsp://admin:pa%40ss@192.168.1.50:554/cam/realmonitor?channel=1&subtype=0",
			RTSPParts{Host: "192.168.1.50", Port: 554, Path: "/cam/realmonitor?channel=1&subtype=0", Username: "admin", Password: "pa@ss"}},
		{"rtsp://cam.local/stream1", RTSPParts{Host: "cam.local", Port: 554, Path: "/stream1"}},
		{"RTSP://user@10.0.0.9:8554", RTSPParts{Host: "10.0.0.9", Port: 8554, Path: "/", Username: "user"}},
		{"rtsp://u:p:w@host/a@b", RTSPParts{Host: "host", Port: 554, Path: "/a@b", Username: "u", Password: "p:w"}},
		{"rtsp://[fe80::1]:8554/x", RTSPParts{Host: "fe80::1", Port: 8554, Path: "/x"}},
		{"rtsp://host?token=1", RTSPParts{Host: "host", Port: 554, Path: "/?token=1"}},
	}
	for _, c := range cases {
		got, err := ParseRTSPURL(c.in)
		if err != nil {
			t.Errorf("%s: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s:\n got %+v\nwant %+v", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"http://host/x", "rtsp://", "rtsp://host:99999/x", "host/stream"} {
		if _, err := ParseRTSPURL(bad); err == nil {
			t.Errorf("%s should be rejected", bad)
		}
	}
	cam := Camera{Host: "192.168.1.50", Port: 554, Path: "/cam/realmonitor?channel=1&subtype=0"}
	if got := cam.URL(); got != "rtsp://192.168.1.50:554/cam/realmonitor?channel=1&subtype=0" {
		t.Fatalf("URL() = %s", got)
	}
}

func TestRedactNeverLeaksPasswords(t *testing.T) {
	cases := map[string]string{
		"connecting to rtsp://admin:hunter2@192.168.1.50:554/stream": "connecting to rtsp://admin:***@192.168.1.50:554/stream",
		"RTSPS://u:p@h/x and rtsp://a:b@c/d":                         "RTSPS://u:***@h/x and rtsp://a:***@c/d",
		`{"username":"admin","password":"hunter2"}`:                  `{"username":"admin","password":"***"}`,
		`{"passwordEnc": "AAAA=="}`:                                  `{"passwordEnc": "***"}`,
		"rtsp://192.168.1.50:554/stream":                             "rtsp://192.168.1.50:554/stream",
		"nothing secret here":                                        "nothing secret here",
	}
	for in, want := range cases {
		got := Redact(in)
		if got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
		if strings.Contains(got, "hunter2") {
			t.Errorf("password leaked: %q", got)
		}
	}
}

func TestSlugifyAndUniqueID(t *testing.T) {
	if got := Slugify("  Front Door #2 (North) "); got != "front-door-2-north" {
		t.Fatalf("got %q", got)
	}
	long := Slugify(strings.Repeat("Camera ", 20))
	if len(long) > 48 || strings.HasSuffix(long, "-") {
		t.Fatalf("bad slug %q", long)
	}
	taken := map[string]bool{"lobby": true, "lobby-2": true}
	if got := UniqueID("lobby", func(s string) bool { return taken[s] }); got != "lobby-3" {
		t.Fatalf("got %q", got)
	}
}

func TestDiff(t *testing.T) {
	a := validWin()
	b := a.Clone()
	if !Diff(a, b).Empty() {
		t.Fatalf("identical configs differ: %+v", Diff(a, b))
	}
	b.Cameras[0].Host = "192.168.1.51"
	b.Cameras = append(b.Cameras, Camera{ID: "lobby", Name: "Lobby", Host: "10.0.0.3", Port: 554, Path: "/", Transport: "tcp", RecordingMode: "continuous"})
	b.Destinations[0].CapGB = 1
	b.Staging.MinFreeGB = 5
	b.Server.Port = 9000
	ch := Diff(a, b)
	if len(ch.CamerasChanged) != 1 || ch.CamerasChanged[0] != "front-door" || len(ch.CamerasAdded) != 1 ||
		!ch.DestinationsChanged || !ch.StagingFloorChanged || ch.StagingPathChanged || !ch.ServerChanged || ch.LoggingChanged {
		t.Fatalf("unexpected diff %+v", ch)
	}
	c := a.Clone()
	c.Cameras = nil
	if got := Diff(a, c).CamerasRemoved; len(got) != 1 || got[0] != "front-door" {
		t.Fatalf("removed = %v", got)
	}
}

func TestProtectRoundTrip(t *testing.T) {
	enc, err := Protect("s3cret")
	if err != nil {
		t.Fatal(err)
	}
	if enc == "s3cret" || enc == "" {
		t.Fatalf("Protect returned %q", enc)
	}
	got, err := Unprotect(enc)
	if err != nil || got != "s3cret" {
		t.Fatalf("Unprotect = %q, %v", got, err)
	}
	if e, _ := Protect(""); e != "" {
		t.Fatalf("empty secret must stay empty")
	}
}

func TestSanitizedRemovesPasswords(t *testing.T) {
	out, _ := json.Marshal(validWin().Sanitized())
	if strings.Contains(string(out), "passwordEnc") {
		t.Fatalf("export still contains passwords: %s", out)
	}
}

func TestV1ConfigMigratesToV2(t *testing.T) {
	raw := `{"schemaVersion":1,"server":{"listenAddress":"0.0.0.0","port":8650,"https":true,"allowedSubnets":[]},
	"staging":{"path":"/var/staging","minFreePercent":10,"minFreeGB":50},"destinations":[],"cameras":[]}`
	cfg, from, err := Parse([]byte(raw), unixEnv)
	if err != nil {
		t.Fatal(err)
	}
	if from != 1 || cfg.SchemaVersion != 2 || !cfg.Live.Enabled || !cfg.Live.MainStream || cfg.Live.Views == nil {
		t.Fatalf("from=%d cfg=%+v", from, cfg.Live)
	}
}

func TestLiveViewsValidated(t *testing.T) {
	c := Default("/var/staging")
	c.Cameras = []Camera{{ID: "cam", Name: "Cam", Enabled: true, Host: "10.0.0.2"}}
	c.Live.Views = []View{
		{ID: "v1", Name: "A", Layout: 4, CameraIDs: []string{"cam"}},
		{ID: "v1", Name: "", Layout: 5, CameraIDs: []string{"nope", "cam", "cam"}},
	}
	c.Normalize()
	f := fieldsOf(t, Validate(c, unixEnv))
	for _, k := range []string{"live.views[1].id", "live.views[1].name", "live.views[1].layout", "live.views[1].cameraIds"} {
		if f[k] == "" {
			t.Errorf("%s not reported: %v", k, f)
		}
	}
	if f["live.views[0].id"] != "" {
		t.Errorf("valid view rejected: %v", f)
	}
}

func TestSubStreamChangeIsNotARecorderChange(t *testing.T) {
	a := Default("/var/staging")
	a.Cameras = []Camera{{ID: "cam", Name: "Cam", Enabled: true, Host: "10.0.0.2"}}
	a.Normalize()
	b := a.Clone()
	sub := "/sub"
	b.Cameras[0].SubStreamPath = &sub
	if b.Cameras[0].SubURL() != "rtsp://10.0.0.2:554/sub" {
		t.Fatalf("sub url %q", b.Cameras[0].SubURL())
	}
	b.Live.MainStream = false
	ch := Diff(a, b)
	if !ch.LiveChanged {
		t.Fatal("live change not seen")
	}
}
