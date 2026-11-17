// Package config defines config.json: its schema, validation, atomic storage
// with backups, migrations, change diffing and secret protection (spec 5).
package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// SchemaVersion is the config schema this build reads and writes.
const SchemaVersion = 2

// CamerasPerWorker is the number of cameras one recorder worker handles.
const CamerasPerWorker = 6

// Defaults.
const (
	DefaultPort             = 8650
	DefaultListenAddress    = "0.0.0.0"
	DefaultMinFreePercent   = 10
	DefaultMinFreeGB        = 50
	DefaultLogRetentionDays = 14
	DefaultRTSPPort         = 554
)

// Config is the whole of config.json.
type Config struct {
	SchemaVersion int           `json:"schemaVersion"`
	Server        Server        `json:"server"`
	Staging       Staging       `json:"staging"`
	Logging       Logging       `json:"logging"`
	Destinations  []Destination `json:"destinations"`
	Cameras       []Camera      `json:"cameras"`
	Live          Live          `json:"live"`
}

// Live configures live view and playback (spec 16.8).
type Live struct {
	// Enabled runs the media process. Off hides live view and playback.
	Enabled bool `json:"enabled"`
	// MainStream lets the recorder workers offer their loopback restream.
	// Off, live view uses sub streams only.
	MainStream bool `json:"mainStream"`
	// Views are the shared views every user sees and Admins edit.
	Views []View `json:"views"`
}

// MaxViews is the most views one list (shared, or one user's own) may hold.
const MaxViews = 50

// View is a saved arrangement of cameras on the Live page.
type View struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Layout    int      `json:"layout"`
	CameraIDs []string `json:"cameraIds"`
}

// Server configures the web UI / API listener.
type Server struct {
	ListenAddress  string   `json:"listenAddress"`
	Port           int      `json:"port"`
	HTTPS          bool     `json:"https"`
	AllowedSubnets []string `json:"allowedSubnets"`
}

// Staging is the local folder every camera records into first.
type Staging struct {
	Path           string  `json:"path"`
	MinFreePercent float64 `json:"minFreePercent"`
	MinFreeGB      float64 `json:"minFreeGB"`
}

// Logging holds the log retention setting of the System page (spec 10.3).
type Logging struct {
	RetentionDays int `json:"retentionDays"`
}

// Destination types.
const (
	DestLocal = "local"
	DestUNC   = "unc"
)

// Destination is a place finished recordings are copied to.
type Destination struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Type        string  `json:"type"`
	Path        string  `json:"path"`
	Username    string  `json:"username,omitempty"`
	PasswordEnc string  `json:"passwordEnc,omitempty"`
	CapGB       float64 `json:"capGB"`
	MaxAgeDays  *int    `json:"maxAgeDays"`
	Enabled     bool    `json:"enabled"`
}

// Camera is one RTSP camera.
type Camera struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Enabled        bool     `json:"enabled"`
	Host           string   `json:"host"`
	Port           int      `json:"port"`
	Path           string   `json:"path"`
	Username       string   `json:"username,omitempty"`
	PasswordEnc    string   `json:"passwordEnc,omitempty"`
	Transport      string   `json:"transport"`
	RecordAudio    bool     `json:"recordAudio"`
	DestinationIDs []string `json:"destinationIds"`

	// SubStreamPath is the path of the camera's low-resolution stream, used
	// by the live grid (spec 16.3). It is never recorded. nil = none.
	SubStreamPath *string `json:"subStreamPath"`

	// Reserved for later versions: saved and loaded, never used.
	RecordingMode string                     `json:"recordingMode"`
	Extensions    map[string]json.RawMessage `json:"extensions"`
}

// Default returns the configuration used on first start.
func Default(stagingPath string) *Config {
	return &Config{
		SchemaVersion: SchemaVersion,
		Server: Server{
			ListenAddress:  DefaultListenAddress,
			Port:           DefaultPort,
			HTTPS:          true,
			AllowedSubnets: []string{},
		},
		Staging: Staging{
			Path:           stagingPath,
			MinFreePercent: DefaultMinFreePercent,
			MinFreeGB:      DefaultMinFreeGB,
		},
		Logging:      Logging{RetentionDays: DefaultLogRetentionDays},
		Destinations: []Destination{},
		Cameras:      []Camera{},
		Live:         Live{Enabled: true, MainStream: true, Views: []View{}},
	}
}

// Normalize fills in defaults and replaces nil slices and maps so the saved
// file always has the same shape.
func (c *Config) Normalize() {
	if c.SchemaVersion == 0 {
		c.SchemaVersion = SchemaVersion
	}
	if c.Server.ListenAddress == "" {
		c.Server.ListenAddress = DefaultListenAddress
	}
	if c.Server.Port == 0 {
		c.Server.Port = DefaultPort
	}
	if c.Server.AllowedSubnets == nil {
		c.Server.AllowedSubnets = []string{}
	}
	if c.Logging.RetentionDays == 0 {
		c.Logging.RetentionDays = DefaultLogRetentionDays
	}
	if c.Destinations == nil {
		c.Destinations = []Destination{}
	}
	if c.Cameras == nil {
		c.Cameras = []Camera{}
	}
	for i := range c.Destinations {
		d := &c.Destinations[i]
		d.Name = strings.TrimSpace(d.Name)
		d.Path = strings.TrimSpace(d.Path)
	}
	for i := range c.Cameras {
		cam := &c.Cameras[i]
		cam.Name = strings.TrimSpace(cam.Name)
		cam.Host = strings.TrimSpace(cam.Host)
		if cam.Port == 0 {
			cam.Port = DefaultRTSPPort
		}
		if cam.Path == "" {
			cam.Path = "/"
		}
		if cam.Transport == "" {
			cam.Transport = "tcp"
		}
		if cam.RecordingMode == "" {
			cam.RecordingMode = "continuous"
		}
		if cam.DestinationIDs == nil {
			cam.DestinationIDs = []string{}
		}
		if cam.Extensions == nil {
			cam.Extensions = map[string]json.RawMessage{}
		}
		if cam.SubStreamPath != nil {
			if sub := strings.TrimSpace(*cam.SubStreamPath); sub == "" {
				cam.SubStreamPath = nil
			} else {
				cam.SubStreamPath = &sub
			}
		}
	}
	known := map[string]bool{}
	for _, cam := range c.Cameras {
		known[cam.ID] = true
	}
	c.Live.Views = NormalizeViews(c.Live.Views, func(id string) bool { return known[id] })
}

// NormalizeViews trims names and removes cameras that keep reports as gone,
// so that deleting a camera never leaves a view pointing at nothing. keep nil
// keeps every camera.
func NormalizeViews(views []View, keep func(cameraID string) bool) []View {
	out := make([]View, 0, len(views))
	for _, v := range views {
		v.Name = strings.TrimSpace(v.Name)
		ids := make([]string, 0, len(v.CameraIDs))
		for _, id := range v.CameraIDs {
			if keep == nil || keep(id) {
				ids = append(ids, id)
			}
		}
		v.CameraIDs = ids
		out = append(out, v)
	}
	return out
}

// ValidateViews checks a list of views; field names start with prefix.
func ValidateViews(views []View, prefix string) []FieldError {
	var out []FieldError
	add := func(field, msg string) { out = append(out, FieldError{Field: field, Message: msg}) }
	if len(views) > MaxViews {
		add(prefix, fmt.Sprintf("At most %d views can be saved.", MaxViews))
	}
	seen := map[string]bool{}
	for i, v := range views {
		f := func(name string) string { return fmt.Sprintf("%s[%d].%s", prefix, i, name) }
		switch {
		case !viewIDRe.MatchString(v.ID) || v.ID == "all":
			add(f("id"), "The view id may contain letters, digits, hyphens and underscores, 1 to 64 characters.")
		case seen[v.ID]:
			add(f("id"), fmt.Sprintf("Another view already uses the id %q.", v.ID))
		}
		seen[v.ID] = true
		if v.Name == "" {
			add(f("name"), "Give the view a name.")
		} else if len(v.Name) > 60 {
			add(f("name"), "The name is too long (60 characters at most).")
		}
		if v.Layout != 1 && v.Layout != 4 && v.Layout != 9 && v.Layout != 16 {
			add(f("layout"), "The layout must be 1, 4, 9 or 16 tiles.")
		}
		if len(v.CameraIDs) > 64 {
			add(f("cameraIds"), "A view can hold at most 64 cameras.")
		}
		dup := map[string]bool{}
		for _, id := range v.CameraIDs {
			if dup[id] {
				add(f("cameraIds"), fmt.Sprintf("Camera %q is listed twice.", id))
			}
			dup[id] = true
		}
	}
	return out
}

var viewIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// SubURL returns the RTSP URL of the camera's sub stream without
// credentials, and false when the camera has none.
func (c Camera) SubURL() (string, bool) {
	if c.SubStreamPath == nil || *c.SubStreamPath == "" {
		return "", false
	}
	sub := c
	sub.Path = *c.SubStreamPath
	return sub.URL(), true
}

// Clone returns a deep copy.
func (c *Config) Clone() *Config {
	b, err := json.Marshal(c)
	if err != nil {
		panic(err) // Config contains only marshalable fields
	}
	var out Config
	if err := json.Unmarshal(b, &out); err != nil {
		panic(err)
	}
	out.Normalize()
	return &out
}

// Sanitized returns a copy with every password removed, for export.
func (c *Config) Sanitized() *Config {
	out := c.Clone()
	for i := range out.Destinations {
		out.Destinations[i].PasswordEnc = ""
	}
	for i := range out.Cameras {
		out.Cameras[i].PasswordEnc = ""
	}
	return out
}

// Camera returns the camera with the given id.
func (c *Config) Camera(id string) (Camera, bool) {
	for _, cam := range c.Cameras {
		if cam.ID == id {
			return cam, true
		}
	}
	return Camera{}, false
}

// Destination returns the destination with the given id.
func (c *Config) Destination(id string) (Destination, bool) {
	for _, d := range c.Destinations {
		if d.ID == id {
			return d, true
		}
	}
	return Destination{}, false
}

// URL returns the camera's RTSP URL without credentials.
func (c Camera) URL() string {
	p := c.Path
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	host := c.Host
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]" // IPv6 literal
	}
	return fmt.Sprintf("rtsp://%s:%d%s", host, c.Port, p)
}

// RTSPParts is a full RTSP URL split into the camera fields.
type RTSPParts struct {
	Host     string
	Port     int
	Path     string
	Username string
	Password string
}

// ParseRTSPURL splits a pasted rtsp:// URL into fields and extracts embedded
// credentials (spec 5.2).
func ParseRTSPURL(raw string) (RTSPParts, error) {
	raw = strings.TrimSpace(raw)
	low := strings.ToLower(raw)
	if !strings.HasPrefix(low, "rtsp://") {
		return RTSPParts{}, fmt.Errorf("the URL must start with rtsp://")
	}
	rest := raw[len("rtsp://"):]
	var p RTSPParts
	// Split authority from path first: camera paths often contain characters
	// that net/url would reject or re-encode, so the path is kept verbatim.
	authority, path := rest, "/"
	if i := strings.IndexAny(rest, "/?"); i >= 0 {
		authority, path = rest[:i], rest[i:]
		if strings.HasPrefix(path, "?") {
			path = "/" + path
		}
	}
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		userinfo := authority[:at]
		authority = authority[at+1:]
		user, pass, _ := strings.Cut(userinfo, ":")
		if u, err := url.PathUnescape(user); err == nil {
			user = u
		}
		if pw, err := url.PathUnescape(pass); err == nil {
			pass = pw
		}
		p.Username, p.Password = user, pass
	}
	host, port := authority, ""
	if strings.HasPrefix(authority, "[") {
		end := strings.Index(authority, "]")
		if end < 0 {
			return RTSPParts{}, fmt.Errorf("the URL has an unterminated IPv6 address")
		}
		host = authority[1:end]
		if tail := authority[end+1:]; strings.HasPrefix(tail, ":") {
			port = tail[1:]
		}
	} else if i := strings.LastIndex(authority, ":"); i >= 0 {
		host, port = authority[:i], authority[i+1:]
	}
	if host == "" {
		return RTSPParts{}, fmt.Errorf("the URL has no host")
	}
	p.Host = host
	p.Port = DefaultRTSPPort
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return RTSPParts{}, fmt.Errorf("the URL has an invalid port %q", port)
		}
		p.Port = n
	}
	p.Path = path
	return p, nil
}

var (
	urlCredRe  = regexp.MustCompile(`(?i)\b(rtsps?|https?|smb)://([^\s:@/]*):([^\s@/]*)@`)
	slugStrip  = regexp.MustCompile(`[^a-z0-9]+`)
	jsonPassRe = regexp.MustCompile(`(?i)("(?:password|passwordEnc|pass|secret|token)"\s*:\s*")[^"]*(")`)
)

// Redact removes credentials from any URLs and password fields found in s, so
// they never reach a log (spec 10.3): rtsp://user:pw@host → rtsp://user:***@host.
func Redact(s string) string {
	s = urlCredRe.ReplaceAllString(s, "$1://$2:***@")
	s = jsonPassRe.ReplaceAllString(s, "${1}***${2}")
	return s
}

// Slugify derives an id from a display name: lowercase letters, digits and
// hyphens, at most 48 characters.
func Slugify(name string) string {
	s := slugStrip.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "-")
	s = strings.Trim(s, "-")
	if len(s) > 48 {
		s = strings.Trim(s[:48], "-")
	}
	return s
}

// UniqueID returns base, or base with a numeric suffix, such that taken
// reports false for it.
func UniqueID(base string, taken func(string) bool) string {
	if base == "" {
		base = "item"
	}
	if !taken(base) {
		return base
	}
	for n := 2; ; n++ {
		suffix := "-" + strconv.Itoa(n)
		b := base
		if len(b)+len(suffix) > 48 {
			b = strings.Trim(b[:48-len(suffix)], "-")
		}
		if id := b + suffix; !taken(id) {
			return id
		}
	}
}
