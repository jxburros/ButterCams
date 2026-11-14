package api

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jxburros/buttercams/internal/config"
	"github.com/jxburros/buttercams/internal/index"
	"github.com/jxburros/buttercams/internal/ipc"
	"github.com/jxburros/buttercams/internal/mover"
	"github.com/jxburros/buttercams/internal/naming"
	"github.com/jxburros/buttercams/internal/version"
	"github.com/jxburros/buttercams/internal/worker"
)

// ---------------------------------------------------------------------- auth

type userView struct {
	Username string `json:"username"`
	Role     string `json:"role"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &in); err != nil {
		s.writeFail(w, err)
		return
	}
	u, err := s.users.Authenticate(strings.TrimSpace(in.Username), in.Password)
	if err != nil {
		var locked *ErrLocked
		if errors.As(err, &locked) {
			s.log.Warn("sign-in refused: account locked", "user", in.Username, "client", clientIP(r).String())
			writeError(w, http.StatusLocked, "account_locked", locked.Error())
			return
		}
		s.log.Warn("sign-in failed", "user", in.Username, "client", clientIP(r).String())
		writeError(w, http.StatusUnauthorized, "invalid_credentials", ErrBadCredentials.Error())
		return
	}
	sess := s.sessions.create(u)
	s.setCookie(w, sess.token)
	s.log.Info("signed in", "user", u.Username, "role", u.Role, "client", clientIP(r).String())
	writeJSON(w, http.StatusOK, map[string]any{"user": userView{u.Username, u.Role}, "csrfToken": sess.csrf})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if sess, ok := sessionOf(r); ok {
		s.sessions.delete(sess.token)
	}
	s.clearCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionOf(r)
	writeJSON(w, http.StatusOK, map[string]any{"user": userView{sess.username, sess.role}, "csrfToken": sess.csrf})
}

// -------------------------------------------------------------------- health

type healthView struct {
	Health
	Version   string `json:"version"`
	UptimeSec int64  `json:"uptimeSec"`
}

func (s *Server) healthView() healthView {
	return healthView{Health: s.backend.Health(), Version: version.Version,
		UptimeSec: int64(time.Since(s.backend.StartedAt()).Seconds())}
}

// handleHealth needs no session from localhost, so monitoring tools on the
// server itself can poll it; from anywhere else a session is required.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if !isLoopback(r) {
		ok := false
		if c, err := r.Cookie(SessionCookie); err == nil {
			_, ok = s.sessions.get(c.Value)
		}
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Sign in to continue.")
			return
		}
	}
	writeJSON(w, http.StatusOK, s.healthView())
}

// ------------------------------------------------------------------- cameras

type cameraView struct {
	ID             string                     `json:"id"`
	Name           string                     `json:"name"`
	Enabled        bool                       `json:"enabled"`
	Host           string                     `json:"host"`
	Port           int                        `json:"port"`
	Path           string                     `json:"path"`
	Username       string                     `json:"username"`
	HasPassword    bool                       `json:"hasPassword"`
	Transport      string                     `json:"transport"`
	RecordAudio    bool                       `json:"recordAudio"`
	DestinationIDs []string                   `json:"destinationIds"`
	SubStreamPath  *string                    `json:"subStreamPath"`
	RecordingMode  string                     `json:"recordingMode"`
	Extensions     map[string]json.RawMessage `json:"extensions"`
	Stats          *statsView                 `json:"stats"`
}

type statsView struct {
	State        string     `json:"state"`
	StateSince   *time.Time `json:"stateSince"`
	Message      string     `json:"message"`
	LastPacketTS *time.Time `json:"lastPacketTs"`
	// LastPacketAgeSec is how old the last video packet was when the recorder
	// reported; ReportAgeSec is how long ago that report was. Both are
	// measured on the server, so they do not depend on the viewer's clock.
	LastPacketAgeSec   *float64   `json:"lastPacketAgeSec"`
	ReportAgeSec       float64    `json:"reportAgeSec"`
	VideoCodec         string     `json:"videoCodec"`
	AudioCodec         string     `json:"audioCodec"`
	AudioNote          string     `json:"audioNote"`
	Width              int        `json:"width"`
	Height             int        `json:"height"`
	FPS                float64    `json:"fps"`
	KeyframeIntervalMs int        `json:"keyframeIntervalMs"`
	BitrateKbps        int        `json:"bitrateKbps"`
	CurrentFile        string     `json:"currentFile"`
	CurrentFileBytes   int64      `json:"currentFileBytes"`
	SingleSession      bool       `json:"singleSession"`
	WorkerID           int        `json:"workerId"`
	UpdatedAt          *time.Time `json:"updatedAt"`
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func round1(v float64) float64 { return float64(int(v*10+0.5)) / 10 }

func toStatsView(st CameraStats) *statsView {
	var packetAge *float64
	if !st.LastPacketTS.IsZero() && !st.TS.IsZero() {
		age := round1(st.TS.Sub(st.LastPacketTS.Time).Seconds())
		if age < 0 {
			age = 0
		}
		packetAge = &age
	}
	return &statsView{
		LastPacketAgeSec: packetAge, ReportAgeSec: round1(time.Since(st.UpdatedAt).Seconds()),
		State: st.State, StateSince: timePtr(st.StateSince.Time), Message: st.Message,
		LastPacketTS: timePtr(st.LastPacketTS.Time), VideoCodec: st.VideoCodec, AudioCodec: st.AudioCodec,
		AudioNote: st.AudioNote, Width: st.Width, Height: st.Height, FPS: st.FPS,
		KeyframeIntervalMs: st.KeyframeIntervalMs, BitrateKbps: st.BitrateKbps, CurrentFile: st.CurrentFile,
		CurrentFileBytes: st.CurrentFileBytes, SingleSession: st.SingleSession, WorkerID: st.WorkerID,
		UpdatedAt: timePtr(st.UpdatedAt),
	}
}

func toCameraView(c config.Camera, stats map[string]CameraStats) cameraView {
	v := cameraView{
		ID: c.ID, Name: c.Name, Enabled: c.Enabled, Host: c.Host, Port: c.Port, Path: c.Path, Username: c.Username,
		HasPassword: c.PasswordEnc != "", Transport: c.Transport, RecordAudio: c.RecordAudio,
		DestinationIDs: c.DestinationIDs, SubStreamPath: c.SubStreamPath, RecordingMode: c.RecordingMode,
		Extensions: c.Extensions,
	}
	if v.DestinationIDs == nil {
		v.DestinationIDs = []string{}
	}
	if v.Extensions == nil {
		v.Extensions = map[string]json.RawMessage{}
	}
	if st, ok := stats[c.ID]; ok {
		v.Stats = toStatsView(st)
	}
	return v
}

func (s *Server) handleCamerasList(w http.ResponseWriter, r *http.Request) {
	cfg := s.backend.Config()
	stats := s.backend.CameraStats()
	out := make([]cameraView, 0, len(cfg.Cameras))
	for _, c := range cfg.Cameras {
		out = append(out, toCameraView(c, stats))
	}
	writeJSON(w, http.StatusOK, map[string]any{"cameras": out})
}

type cameraInput struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Enabled        *bool    `json:"enabled"`
	URL            string   `json:"url"`
	Host           string   `json:"host"`
	Port           int      `json:"port"`
	Path           string   `json:"path"`
	Username       string   `json:"username"`
	Password       string   `json:"password"`
	ClearPassword  bool     `json:"clearPassword"`
	Transport      string   `json:"transport"`
	RecordAudio    *bool    `json:"recordAudio"`
	DestinationIDs []string `json:"destinationIds"`
}

// apply copies the input onto a camera. prev is the saved camera when
// editing (its password is kept unless a new one is given).
func (in cameraInput) apply(prev *config.Camera) (config.Camera, error) {
	var cam config.Camera
	if prev != nil {
		cam = *prev
	} else {
		cam.Enabled, cam.RecordAudio = true, true
	}
	cam.Name = strings.TrimSpace(in.Name)
	cam.Host, cam.Port, cam.Path = strings.TrimSpace(in.Host), in.Port, strings.TrimSpace(in.Path)
	cam.Username = strings.TrimSpace(in.Username)
	password := in.Password
	if strings.TrimSpace(in.URL) != "" {
		parts, err := config.ParseRTSPURL(in.URL)
		if err != nil {
			return cam, FieldErr("url", "This is not a usable RTSP address: "+err.Error()+".")
		}
		cam.Host, cam.Port, cam.Path = parts.Host, parts.Port, parts.Path
		if parts.Username != "" {
			cam.Username = parts.Username
		}
		if parts.Password != "" {
			password = parts.Password
		}
	}
	if in.Enabled != nil {
		cam.Enabled = *in.Enabled
	}
	if in.RecordAudio != nil {
		cam.RecordAudio = *in.RecordAudio
	}
	if in.Transport != "" {
		cam.Transport = in.Transport
	}
	if in.DestinationIDs != nil {
		cam.DestinationIDs = in.DestinationIDs
	}
	switch {
	case in.ClearPassword:
		cam.PasswordEnc = ""
	case password != "":
		enc, err := config.Protect(password)
		if err != nil {
			return cam, err
		}
		cam.PasswordEnc = enc
	}
	return cam, nil
}

var indexedField = regexp.MustCompile(`^(cameras|destinations)\[(\d+)\]\.`)

// localFields rewrites "cameras[3].host" to "host" for errors about the item
// being edited, which is how the form names its inputs. Errors about other
// items are dropped: they are not this request's problem to report.
func localFields(err error, kind string, idx int) error {
	var ve *config.ValidationError
	if !errors.As(err, &ve) {
		return err
	}
	out := &config.ValidationError{}
	prefix := fmt.Sprintf("%s[%d].", kind, idx)
	for _, f := range ve.Fields {
		switch {
		case strings.HasPrefix(f.Field, prefix):
			out.Fields = append(out.Fields, config.FieldError{Field: strings.TrimPrefix(f.Field, prefix), Message: f.Message})
		case !indexedField.MatchString(f.Field):
			out.Fields = append(out.Fields, f)
		}
	}
	if len(out.Fields) == 0 {
		return ve // something else in the configuration is invalid: report it as it is
	}
	return out
}

func (s *Server) handleCameraCreate(w http.ResponseWriter, r *http.Request) {
	var in cameraInput
	if err := readJSON(r, &in); err != nil {
		s.writeFail(w, err)
		return
	}
	cam, err := in.apply(nil)
	if err != nil {
		s.writeFail(w, err)
		return
	}
	idx := -1
	cfg, err := s.backend.UpdateConfig(func(c *config.Config) error {
		taken := func(id string) bool { _, ok := c.Camera(id); return ok }
		cam.ID = strings.TrimSpace(in.ID)
		if cam.ID == "" {
			base := config.Slugify(cam.Name)
			if base == "" {
				base = "camera"
			}
			cam.ID = config.UniqueID(base, taken)
		} else if taken(cam.ID) {
			return FieldErr("id", fmt.Sprintf("Another camera already uses the id %q.", cam.ID))
		}
		c.Cameras = append(c.Cameras, cam)
		idx = len(c.Cameras) - 1
		return nil
	})
	if err != nil {
		s.writeFail(w, localFields(err, "cameras", idx))
		return
	}
	created, _ := cfg.Camera(cam.ID)
	s.audit(r, "camera_added", "Camera "+created.Name+" was added.", created.ID)
	writeJSON(w, http.StatusCreated, toCameraView(created, s.backend.CameraStats()))
}

func (s *Server) handleCameraUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var in cameraInput
	if err := readJSON(r, &in); err != nil {
		s.writeFail(w, err)
		return
	}
	idx := -1
	cfg, err := s.backend.UpdateConfig(func(c *config.Config) error {
		for i := range c.Cameras {
			if c.Cameras[i].ID == id {
				idx = i
			}
		}
		if idx < 0 {
			return errNotFound
		}
		cam, err := in.apply(&c.Cameras[idx])
		if err != nil {
			return err
		}
		cam.ID = id // the id names folders and can never change
		c.Cameras[idx] = cam
		return nil
	})
	if err != nil {
		s.writeFail(w, localFields(err, "cameras", idx))
		return
	}
	cam, _ := cfg.Camera(id)
	writeJSON(w, http.StatusOK, toCameraView(cam, s.backend.CameraStats()))
}

func (s *Server) handleCameraDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	keep := r.URL.Query().Get("keepRecordings") != "false"
	name := id
	_, err := s.backend.UpdateConfig(func(c *config.Config) error {
		for i := range c.Cameras {
			if c.Cameras[i].ID == id {
				name = c.Cameras[i].Name
				c.Cameras = append(c.Cameras[:i], c.Cameras[i+1:]...)
				return nil
			}
		}
		return errNotFound
	})
	if err != nil {
		s.writeFail(w, err)
		return
	}
	if keep {
		s.audit(r, "camera_deleted", "Camera "+name+" was deleted; its recordings were kept.", id)
	} else {
		s.audit(r, "camera_deleted", "Camera "+name+" was deleted together with its recordings.", id)
		s.backend.PurgeCamera(id)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleCameraTest(w http.ResponseWriter, r *http.Request) {
	var in cameraInput
	if err := readJSON(r, &in); err != nil {
		s.writeFail(w, err)
		return
	}
	cfg := s.backend.Config()
	var prev *config.Camera
	if in.ID != "" {
		if c, ok := cfg.Camera(in.ID); ok {
			prev = &c
		}
	}
	cam, err := in.apply(prev)
	if err != nil {
		s.writeFail(w, err)
		return
	}
	if cam.ID == "" {
		cam.ID = "test"
	}
	if cam.Name == "" {
		cam.Name = "test"
	}
	// Validate only the connection fields of this one camera.
	probeCfg := config.Default(cfg.Staging.Path)
	cam.DestinationIDs = nil
	probeCfg.Cameras = []config.Camera{cam}
	probeCfg.Normalize()
	if err := config.Validate(probeCfg, config.Env{}); err != nil {
		if local := localFields(err, "cameras", 0); local != err {
			s.writeFail(w, local)
			return
		}
	}
	cam = probeCfg.Cameras[0]
	password, err := config.Unprotect(cam.PasswordEnc)
	if err != nil {
		s.writeFail(w, FieldErr("password", "The saved password cannot be read on this computer. Enter the camera password again."))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), HandlerTimeout-3*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, worker.Probe(ctx, cam, password))
}

type importResult struct {
	Line   int      `json:"line"`
	Name   string   `json:"name"`
	ID     string   `json:"id"`
	OK     bool     `json:"ok"`
	Errors []string `json:"errors"`
}

// handleCameraImport adds cameras from CSV text with the columns
// name,host,port,path,username,password,destinations (spec 11.2). A header
// line is optional. Each row is added on its own: a bad row does not stop
// the good ones.
func (s *Server) handleCameraImport(w http.ResponseWriter, r *http.Request) {
	var in struct {
		CSV string `json:"csv"`
	}
	if err := readJSON(r, &in); err != nil {
		s.writeFail(w, err)
		return
	}
	rd := csv.NewReader(strings.NewReader(in.CSV))
	rd.FieldsPerRecord = -1
	rd.TrimLeadingSpace = true
	results := []importResult{}
	created := 0
	for line := 1; ; line++ {
		rec, err := rd.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			results = append(results, importResult{Line: line, Errors: []string{"This line could not be read: " + err.Error()}})
			continue
		}
		if ln, _ := rd.FieldPos(0); ln > 0 {
			line = ln
		}
		for i := range rec {
			rec[i] = strings.TrimSpace(rec[i])
		}
		if len(rec) == 0 || (len(rec) == 1 && rec[0] == "") {
			continue
		}
		if strings.EqualFold(rec[0], "name") && len(rec) > 1 && strings.EqualFold(rec[1], "host") {
			continue // header
		}
		get := func(i int) string {
			if i < len(rec) {
				return rec[i]
			}
			return ""
		}
		res := importResult{Line: line, Name: get(0), Errors: []string{}}
		ci := cameraInput{Name: get(0), Host: get(1), Path: get(3), Username: get(4), Password: get(5)}
		if strings.HasPrefix(strings.ToLower(ci.Host), "rtsp://") {
			ci.URL, ci.Host = ci.Host, ""
		}
		ci.Port = config.DefaultRTSPPort
		if p := get(2); p != "" {
			n, err := strconv.Atoi(p)
			if err != nil {
				res.Errors = append(res.Errors, "Port must be a number between 1 and 65535.")
				results = append(results, res)
				continue
			}
			ci.Port = n
		}
		ci.DestinationIDs = []string{}
		for _, d := range strings.FieldsFunc(get(6), func(r rune) bool { return r == ';' || r == '|' }) {
			if d = strings.TrimSpace(d); d != "" {
				ci.DestinationIDs = append(ci.DestinationIDs, d)
			}
		}
		cam, err := ci.apply(nil)
		if err != nil {
			res.Errors = append(res.Errors, errorTexts(err)...)
			results = append(results, res)
			continue
		}
		idx := -1
		_, err = s.backend.UpdateConfig(func(c *config.Config) error {
			base := config.Slugify(cam.Name)
			if base == "" {
				base = "camera"
			}
			cam.ID = config.UniqueID(base, func(id string) bool { _, ok := c.Camera(id); return ok })
			c.Cameras = append(c.Cameras, cam)
			idx = len(c.Cameras) - 1
			return nil
		})
		if err != nil {
			res.Errors = append(res.Errors, errorTexts(localFields(err, "cameras", idx))...)
		} else {
			res.OK, res.ID = true, cam.ID
			created++
		}
		results = append(results, res)
	}
	if created > 0 {
		s.audit(r, "cameras_imported", fmt.Sprintf("%d cameras were imported from CSV.", created), "")
	}
	writeJSON(w, http.StatusOK, map[string]any{"created": created, "results": results})
}

func errorTexts(err error) []string {
	var ve *config.ValidationError
	if errors.As(err, &ve) {
		out := make([]string, len(ve.Fields))
		for i, f := range ve.Fields {
			out[i] = f.Message
		}
		return out
	}
	return []string{err.Error()}
}

// -------------------------------------------------------------- destinations

type destinationView struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Type        string            `json:"type"`
	Path        string            `json:"path"`
	Username    string            `json:"username"`
	HasPassword bool              `json:"hasPassword"`
	CapGB       float64           `json:"capGB"`
	MaxAgeDays  *int              `json:"maxAgeDays"`
	Enabled     bool              `json:"enabled"`
	Status      DestinationStatus `json:"status"`
}

func toDestinationView(d config.Destination, st map[string]DestinationStatus) destinationView {
	status, ok := st[d.ID]
	if !ok {
		status = DestinationStatus{State: "ok"}
		if !d.Enabled {
			status.State = "disabled"
		}
	}
	return destinationView{ID: d.ID, Name: d.Name, Type: d.Type, Path: d.Path, Username: d.Username,
		HasPassword: d.PasswordEnc != "", CapGB: d.CapGB, MaxAgeDays: d.MaxAgeDays, Enabled: d.Enabled, Status: status}
}

func (s *Server) destinationsPayload() map[string]any {
	cfg := s.backend.Config()
	statuses, staging := s.backend.Storage()
	out := make([]destinationView, 0, len(cfg.Destinations))
	for _, d := range cfg.Destinations {
		out = append(out, toDestinationView(d, statuses))
	}
	return map[string]any{"destinations": out, "staging": staging}
}

func (s *Server) handleDestinationsList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.destinationsPayload())
}

type destinationInput struct {
	ID            string          `json:"id"`
	Name          string          `json:"name"`
	Type          string          `json:"type"`
	Path          string          `json:"path"`
	Username      string          `json:"username"`
	Password      string          `json:"password"`
	ClearPassword bool            `json:"clearPassword"`
	CapGB         float64         `json:"capGB"`
	MaxAgeDays    json.RawMessage `json:"maxAgeDays"`
	Enabled       *bool           `json:"enabled"`
}

func (in destinationInput) apply(prev *config.Destination) (config.Destination, error) {
	var d config.Destination
	if prev != nil {
		d = *prev
	} else {
		d.Enabled = true
	}
	d.Name, d.Type, d.Path = strings.TrimSpace(in.Name), in.Type, strings.TrimSpace(in.Path)
	d.Username = strings.TrimSpace(in.Username)
	d.CapGB = in.CapGB
	if in.Enabled != nil {
		d.Enabled = *in.Enabled
	}
	if len(in.MaxAgeDays) > 0 {
		raw := strings.TrimSpace(string(in.MaxAgeDays))
		if raw == "null" || raw == `""` {
			d.MaxAgeDays = nil
		} else {
			var n float64
			if err := json.Unmarshal(in.MaxAgeDays, &n); err != nil || n != float64(int(n)) {
				return d, FieldErr("maxAgeDays", "Maximum age must be a whole number of days, or empty for no limit.")
			}
			days := int(n)
			d.MaxAgeDays = &days
		}
	}
	if d.Type == config.DestLocal {
		d.Username, d.PasswordEnc = "", ""
		return d, nil
	}
	switch {
	case in.ClearPassword || d.Username == "":
		d.PasswordEnc = ""
	case in.Password != "":
		enc, err := config.Protect(in.Password)
		if err != nil {
			return d, err
		}
		d.PasswordEnc = enc
	}
	return d, nil
}

func (s *Server) handleDestinationCreate(w http.ResponseWriter, r *http.Request) {
	var in destinationInput
	if err := readJSON(r, &in); err != nil {
		s.writeFail(w, err)
		return
	}
	d, err := in.apply(nil)
	if err != nil {
		s.writeFail(w, err)
		return
	}
	idx := -1
	cfg, err := s.backend.UpdateConfig(func(c *config.Config) error {
		taken := func(id string) bool { _, ok := c.Destination(id); return ok || id == ipc.StagingID }
		d.ID = strings.TrimSpace(in.ID)
		if d.ID == "" {
			base := config.Slugify(d.Name)
			if base == "" {
				base = "destination"
			}
			d.ID = config.UniqueID(base, taken)
		} else if taken(d.ID) {
			return FieldErr("id", fmt.Sprintf("The id %q is already in use.", d.ID))
		}
		c.Destinations = append(c.Destinations, d)
		idx = len(c.Destinations) - 1
		return nil
	})
	if err != nil {
		s.writeFail(w, localFields(err, "destinations", idx))
		return
	}
	created, _ := cfg.Destination(d.ID)
	s.audit(r, "destination_added", "Destination "+created.Name+" was added.", "")
	statuses, _ := s.backend.Storage()
	writeJSON(w, http.StatusCreated, toDestinationView(created, statuses))
}

func (s *Server) handleDestinationUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var in destinationInput
	if err := readJSON(r, &in); err != nil {
		s.writeFail(w, err)
		return
	}
	idx := -1
	cfg, err := s.backend.UpdateConfig(func(c *config.Config) error {
		for i := range c.Destinations {
			if c.Destinations[i].ID == id {
				idx = i
			}
		}
		if idx < 0 {
			return errNotFound
		}
		d, err := in.apply(&c.Destinations[idx])
		if err != nil {
			return err
		}
		d.ID = id
		c.Destinations[idx] = d
		return nil
	})
	if err != nil {
		s.writeFail(w, localFields(err, "destinations", idx))
		return
	}
	d, _ := cfg.Destination(id)
	statuses, _ := s.backend.Storage()
	writeJSON(w, http.StatusOK, toDestinationView(d, statuses))
}

func (s *Server) handleDestinationDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	name := id
	_, err := s.backend.UpdateConfig(func(c *config.Config) error {
		var users []string
		for _, cam := range c.Cameras {
			for _, d := range cam.DestinationIDs {
				if d == id {
					users = append(users, cam.Name)
				}
			}
		}
		if len(users) > 0 {
			return &conflictError{"This destination is still used by: " + strings.Join(users, ", ") +
				". Remove it from those cameras first."}
		}
		for i := range c.Destinations {
			if c.Destinations[i].ID == id {
				name = c.Destinations[i].Name
				c.Destinations = append(c.Destinations[:i], c.Destinations[i+1:]...)
				return nil
			}
		}
		return errNotFound
	})
	if err != nil {
		s.writeFail(w, err)
		return
	}
	s.audit(r, "destination_deleted", "Destination "+name+" was removed. The recordings on it were left in place.", "")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDestinationTest(w http.ResponseWriter, r *http.Request) {
	var in destinationInput
	if err := readJSON(r, &in); err != nil {
		s.writeFail(w, err)
		return
	}
	cfg := s.backend.Config()
	var prev *config.Destination
	if in.ID != "" {
		if d, ok := cfg.Destination(in.ID); ok {
			prev = &d
		}
	}
	d, err := in.apply(prev)
	if err != nil {
		s.writeFail(w, err)
		return
	}
	if d.ID == "" {
		d.ID = "test"
	}
	if d.Name == "" {
		d.Name = "test"
	}
	if d.CapGB <= 0 {
		d.CapGB = 1 // the cap does not matter for a connection test
	}
	testCfg := config.Default(cfg.Staging.Path)
	testCfg.Destinations = []config.Destination{d}
	if err := config.Validate(testCfg, s.validateEnv()); err != nil {
		if local := localFields(err, "destinations", 0); local != err {
			s.writeFail(w, local)
			return
		}
	}
	password, err := config.Unprotect(d.PasswordEnc)
	if err != nil {
		s.writeFail(w, FieldErr("password", "The saved password cannot be read on this computer. Enter it again."))
		return
	}
	type outcome struct {
		res mover.TestResult
		err error
	}
	start := time.Now()
	ch := make(chan outcome, 1)
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				ch <- outcome{err: fmt.Errorf("internal error: %v", rec)}
			}
		}()
		res, err := mover.TestDestination(d, password)
		ch <- outcome{res, err}
	}()
	out := map[string]any{"ok": false, "error": "", "freeBytes": nil, "totalBytes": nil}
	select {
	case o := <-ch:
		if o.err != nil {
			out["error"] = config.Redact(o.err.Error())
		} else {
			out["ok"], out["freeBytes"], out["totalBytes"] = true, o.res.FreeBytes, o.res.TotalBytes
		}
	case <-time.After(HandlerTimeout - 5*time.Second):
		out["error"] = "The destination did not answer within 25 seconds."
	}
	out["durationMs"] = time.Since(start).Milliseconds()
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------- recordings

func (s *Server) handleRecordingCameras(w http.ResponseWriter, r *http.Request) {
	cfg := s.backend.Config()
	type item struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Deleted bool   `json:"deleted"`
	}
	out := []item{}
	seen := map[string]bool{}
	for _, c := range cfg.Cameras {
		out = append(out, item{ID: c.ID, Name: c.Name})
		seen[c.ID] = true
	}
	if rows, err := s.backend.Index().Cameras(); err == nil {
		for _, row := range rows {
			if !seen[row.ID] {
				out = append(out, item{ID: row.ID, Name: row.Name, Deleted: true})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Deleted != out[j].Deleted {
			return !out[i].Deleted
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	writeJSON(w, http.StatusOK, map[string]any{"cameras": out})
}

type copyView struct {
	DestinationID string     `json:"destinationId"`
	State         string     `json:"state"`
	LastError     string     `json:"lastError"`
	UpdatedTS     *time.Time `json:"updatedTs"`
}

type segmentView struct {
	ID          int64      `json:"id"`
	CameraID    string     `json:"cameraId"`
	FileName    string     `json:"fileName"`
	Start       time.Time  `json:"start"`
	End         *time.Time `json:"end"`
	Bytes       *int64     `json:"bytes"`
	VideoCodec  string     `json:"videoCodec"`
	AudioCodec  string     `json:"audioCodec"`
	GapBefore   bool       `json:"gapBefore"`
	CloseReason *string    `json:"closeReason"`
	Protected   bool       `json:"protected"`
	Copies      []copyView `json:"copies"`
}

func toSegmentView(seg index.Segment) segmentView {
	v := segmentView{ID: seg.ID, CameraID: seg.CameraID, FileName: seg.FileName, Start: seg.Start,
		VideoCodec: seg.VideoCodec, AudioCodec: seg.AudioCodec, GapBefore: seg.GapBefore, Protected: seg.Protected,
		Copies: []copyView{}}
	if seg.Closed() {
		end, bytes := seg.End, seg.Bytes
		v.End, v.Bytes = &end, &bytes
	}
	if seg.CloseReason != "" {
		reason := seg.CloseReason
		v.CloseReason = &reason
	}
	for _, c := range seg.Copies {
		v.Copies = append(v.Copies, copyView{c.DestinationID, c.State, c.LastError, timePtr(c.Updated)})
	}
	return v
}

func (s *Server) handleSegments(w http.ResponseWriter, r *http.Request) {
	cam, date := r.URL.Query().Get("camera"), r.URL.Query().Get("date")
	if !naming.ValidID(cam) {
		s.writeFail(w, FieldErr("camera", "Choose a camera."))
		return
	}
	if _, err := time.Parse("2006-01-02", date); err != nil {
		s.writeFail(w, FieldErr("date", "Choose a date (YYYY-MM-DD)."))
		return
	}
	segs, err := s.backend.Index().ListSegments(cam, date)
	if err != nil {
		s.writeFail(w, err)
		return
	}
	out := make([]segmentView, 0, len(segs))
	for _, seg := range segs {
		out = append(out, toSegmentView(seg))
	}
	writeJSON(w, http.StatusOK, map[string]any{"segments": out})
}

// handleDownload streams a recording from the nearest available copy. Range
// requests work, so a download can be resumed.
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such recording.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), HandlerTimeout)
	f, seg, err := s.backend.OpenSegment(ctx, id)
	cancel()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, "not_found", "No copy of this recording can be reached right now.")
		} else {
			writeError(w, http.StatusNotFound, "not_found", "No such recording.")
		}
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Content-Disposition", `attachment; filename="`+seg.FileName+`"`)
	st, _ := f.Stat()
	mod := time.Time{}
	if st != nil {
		mod = st.ModTime()
	}
	http.ServeContent(w, r, seg.FileName, mod, f)
}

// -------------------------------------------------------------------- events

type eventView struct {
	ID       int64          `json:"id"`
	TS       time.Time      `json:"ts"`
	Source   string         `json:"source"`
	Type     string         `json:"type"`
	Severity string         `json:"severity"`
	CameraID *string        `json:"cameraId"`
	Data     map[string]any `json:"data"`
}

func toEventView(e index.Event) eventView {
	v := eventView{ID: e.ID, TS: e.TS, Source: e.Source, Type: e.Type, Severity: e.Severity, Data: e.Data}
	if e.CameraID != "" {
		cam := e.CameraID
		v.CameraID = &cam
	}
	if v.Data == nil {
		v.Data = map[string]any{}
	}
	return v
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := index.EventFilter{Severity: q.Get("severity"), CameraID: q.Get("camera")}
	if f.Severity != "" && f.Severity != ipc.SevInfo && f.Severity != ipc.SevWarning && f.Severity != ipc.SevCritical {
		s.writeFail(w, FieldErr("severity", "Severity must be info, warning or critical."))
		return
	}
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			s.writeFail(w, FieldErr("since", "Enter the start time as a date and time."))
			return
		}
		f.Since = t
	}
	if v := q.Get("limit"); v != "" {
		f.Limit, _ = strconv.Atoi(v)
	}
	if v := q.Get("before"); v != "" {
		f.Before, _ = strconv.ParseInt(v, 10, 64)
	}
	events, err := s.backend.Index().ListEvents(f)
	if err != nil {
		s.writeFail(w, err)
		return
	}
	out := make([]eventView, 0, len(events))
	for _, e := range events {
		out = append(out, toEventView(e))
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}

// audit records a configuration change made through the UI.
func (s *Server) audit(r *http.Request, typ, message, cameraID string) {
	data := map[string]any{"message": message}
	if sess, ok := sessionOf(r); ok {
		data["user"] = sess.username
	}
	s.backend.RecordEvent(index.Event{Source: "guardian", Type: typ, Severity: ipc.SevInfo, CameraID: cameraID, Data: data})
}

// -------------------------------------------------------------------- system

func (s *Server) systemPayload() map[string]any {
	cfg := s.backend.Config()
	_, staging := s.backend.Storage()
	host, _ := os.Hostname()
	enabled := 0
	for _, c := range cfg.Cameras {
		if c.Enabled {
			enabled++
		}
	}
	started := s.backend.StartedAt()
	return map[string]any{
		"version":       version.Version,
		"startedAt":     started,
		"uptimeSec":     int64(time.Since(started).Seconds()),
		"hostname":      host,
		"os":            runtime.GOOS + "/" + runtime.GOARCH,
		"dataRoot":      s.backend.DataRoot(),
		"schemaVersion": cfg.SchemaVersion,
		"server":        cfg.Server,
		"staging": map[string]any{
			"path": cfg.Staging.Path, "minFreePercent": cfg.Staging.MinFreePercent, "minFreeGB": cfg.Staging.MinFreeGB,
			"freeBytes": staging.FreeBytes, "totalBytes": staging.TotalBytes,
		},
		"logging":     cfg.Logging,
		"certificate": s.certInfo.Load(),
		"counts": map[string]int{"cameras": len(cfg.Cameras), "enabledCameras": enabled,
			"destinations": len(cfg.Destinations), "workers": s.backend.Workers()},
		"rebuild": s.backend.Rebuild(),
	}
}

func (s *Server) handleSystemGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.systemPayload())
}

func (s *Server) validateEnv() config.Env { return config.Env{IsRemoteDrive: isRemoteDrive} }

func (s *Server) handleSystemPut(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Server *struct {
			ListenAddress  *string   `json:"listenAddress"`
			Port           *int      `json:"port"`
			HTTPS          *bool     `json:"https"`
			AllowedSubnets *[]string `json:"allowedSubnets"`
		} `json:"server"`
		Staging *struct {
			Path           *string  `json:"path"`
			MinFreePercent *float64 `json:"minFreePercent"`
			MinFreeGB      *float64 `json:"minFreeGB"`
		} `json:"staging"`
		Logging *struct {
			RetentionDays *int `json:"retentionDays"`
		} `json:"logging"`
	}
	if err := readJSON(r, &in); err != nil {
		s.writeFail(w, err)
		return
	}
	_, err := s.backend.UpdateConfig(func(c *config.Config) error {
		if in.Server != nil {
			if in.Server.ListenAddress != nil {
				c.Server.ListenAddress = strings.TrimSpace(*in.Server.ListenAddress)
			}
			if in.Server.Port != nil {
				c.Server.Port = *in.Server.Port
			}
			if in.Server.HTTPS != nil {
				c.Server.HTTPS = *in.Server.HTTPS
			}
			if in.Server.AllowedSubnets != nil {
				subnets := []string{}
				for _, sn := range *in.Server.AllowedSubnets {
					if sn = strings.TrimSpace(sn); sn != "" {
						subnets = append(subnets, sn)
					}
				}
				c.Server.AllowedSubnets = subnets
			}
		}
		if in.Staging != nil {
			if in.Staging.Path != nil {
				c.Staging.Path = strings.TrimSpace(*in.Staging.Path)
			}
			if in.Staging.MinFreePercent != nil {
				c.Staging.MinFreePercent = *in.Staging.MinFreePercent
			}
			if in.Staging.MinFreeGB != nil {
				c.Staging.MinFreeGB = *in.Staging.MinFreeGB
			}
		}
		if in.Logging != nil && in.Logging.RetentionDays != nil {
			c.Logging.RetentionDays = *in.Logging.RetentionDays
		}
		return nil
	})
	if err != nil {
		s.writeFail(w, err)
		return
	}
	s.audit(r, "settings_changed", "System settings were changed.", "")
	writeJSON(w, http.StatusOK, s.systemPayload())
}

func (s *Server) handleRebuild(w http.ResponseWriter, r *http.Request) {
	if err := s.backend.StartRebuild(); err != nil {
		writeError(w, http.StatusConflict, "conflict", "An index rebuild is already running.")
		return
	}
	s.audit(r, "index_rebuild_requested", "An index rebuild was started from the System page.", "")
	writeJSON(w, http.StatusAccepted, map[string]any{"started": true})
}

func (s *Server) handleCertificate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		CertPEM   string `json:"certPem"`
		KeyPEM    string `json:"keyPem"`
		PFXBase64 string `json:"pfxBase64"`
		Password  string `json:"password"`
	}
	if err := readJSON(r, &in); err != nil {
		s.writeFail(w, err)
		return
	}
	var err error
	if strings.TrimSpace(in.PFXBase64) != "" {
		err = InstallPFX(s.dataRoot, in.PFXBase64, in.Password)
	} else {
		err = InstallPEM(s.dataRoot, in.CertPEM, in.KeyPEM)
	}
	if err == nil {
		err = s.reloadCertificate() // new connections use it at once; no restart needed
	}
	if err != nil {
		s.writeFail(w, err)
		return
	}
	s.audit(r, "certificate_changed", "The HTTPS certificate was replaced.", "")
	writeJSON(w, http.StatusOK, map[string]any{"certificate": s.certInfo.Load()})
}

func (s *Server) handleConfigExport(w http.ResponseWriter, r *http.Request) {
	data, err := json.MarshalIndent(s.backend.Config().Sanitized(), "", "  ")
	if err != nil {
		s.writeFail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="buttercams-config.json"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Write(append(data, '\n'))
}

// handleConfigImport replaces the configuration with an uploaded one. An
// export carries no passwords, so the saved password is kept for every camera
// and destination whose id already exists.
func (s *Server) handleConfigImport(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeFail(w, FieldErr("", "The file could not be read."))
		return
	}
	// Parse on its own first so a file that is not a configuration is
	// rejected with a clear message.
	imported, _, err := config.Parse(raw, s.validateEnv())
	if err != nil {
		var ve *config.ValidationError
		if errors.As(err, &ve) {
			s.writeFail(w, ve)
			return
		}
		s.writeFail(w, FieldErr("", "This is not a ButterCams configuration file: "+err.Error()+"."))
		return
	}
	_, err = s.backend.UpdateConfig(func(c *config.Config) error {
		camPw := map[string]string{}
		for _, cam := range c.Cameras {
			camPw[cam.ID] = cam.PasswordEnc
		}
		destPw := map[string]string{}
		for _, d := range c.Destinations {
			destPw[d.ID] = d.PasswordEnc
		}
		for i := range imported.Cameras {
			if imported.Cameras[i].PasswordEnc == "" {
				imported.Cameras[i].PasswordEnc = camPw[imported.Cameras[i].ID]
			}
		}
		for i := range imported.Destinations {
			if imported.Destinations[i].PasswordEnc == "" && imported.Destinations[i].Username != "" {
				imported.Destinations[i].PasswordEnc = destPw[imported.Destinations[i].ID]
			}
		}
		*c = *imported
		return nil
	})
	if err != nil {
		s.writeFail(w, err)
		return
	}
	s.audit(r, "config_imported", "A configuration file was imported.", "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --------------------------------------------------------------------- users

type accountView struct {
	Username  string    `json:"username"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"createdAt"`
	Locked    bool      `json:"locked"`
}

func (s *Server) accountView(u User) accountView {
	return accountView{u.Username, u.Role, u.CreatedAt, s.users.Locked(u.Username)}
}

func (s *Server) handleUsersList(w http.ResponseWriter, r *http.Request) {
	list := s.users.List()
	out := make([]accountView, 0, len(list))
	for _, u := range list {
		out = append(out, s.accountView(u))
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
}

func (s *Server) handleUserCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if err := readJSON(r, &in); err != nil {
		s.writeFail(w, err)
		return
	}
	u, err := s.users.Create(strings.TrimSpace(in.Username), in.Password, in.Role)
	if err != nil {
		s.writeFail(w, err)
		return
	}
	s.audit(r, "user_added", "User "+u.Username+" ("+u.Role+") was added.", "")
	writeJSON(w, http.StatusCreated, s.accountView(u))
}

func (s *Server) handleUserUpdate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("username")
	var in struct {
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if err := readJSON(r, &in); err != nil {
		s.writeFail(w, err)
		return
	}
	u, err := s.users.Update(name, in.Password, in.Role)
	if err != nil {
		s.writeFail(w, err)
		return
	}
	me, _ := sessionOf(r)
	if in.Password != "" {
		s.sessions.dropUser(u.Username, me.token) // other sessions of that user must sign in again
	}
	s.sessions.setRole(u.Username, u.Role)
	s.audit(r, "user_changed", "User "+u.Username+" was changed.", "")
	writeJSON(w, http.StatusOK, s.accountView(u))
}

func (s *Server) handleUserDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("username")
	if me, ok := sessionOf(r); ok && strings.EqualFold(me.username, name) {
		writeError(w, http.StatusConflict, "conflict", "You cannot delete the account you are signed in with.")
		return
	}
	if err := s.users.Delete(name); err != nil {
		s.writeFail(w, err)
		return
	}
	s.sessions.dropUser(name, "")
	s.audit(r, "user_deleted", "User "+name+" was deleted.", "")
	w.WriteHeader(http.StatusNoContent)
}
