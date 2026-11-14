package config

import (
	"fmt"
	"net"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/jxburros/buttercams/internal/naming"
)

// FieldError is one validation problem, addressed to a form field.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// ValidationError is returned when a configuration is rejected.
type ValidationError struct {
	Fields []FieldError
}

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Fields))
	for _, f := range e.Fields {
		parts = append(parts, f.Field+": "+f.Message)
	}
	return "invalid configuration: " + strings.Join(parts, "; ")
}

func (e *ValidationError) add(field, format string, args ...any) {
	e.Fields = append(e.Fields, FieldError{Field: field, Message: fmt.Sprintf(format, args...)})
}

// MappedDriveMessage is shown when a destination uses a mapped network drive.
const MappedDriveMessage = `Mapped drives aren't visible to Windows services. Use the \\server\share path instead.`

// Env supplies the platform checks validation needs. The zero value uses the
// rules of the operating system the binary runs on.
type Env struct {
	// Windows selects Windows path rules. Tests set it explicitly.
	Windows *bool
	// IsRemoteDrive reports whether a drive-letter path is a mapped network
	// drive (DRIVE_REMOTE). nil means "never".
	IsRemoteDrive func(path string) bool
}

func (e Env) windows() bool {
	if e.Windows != nil {
		return *e.Windows
	}
	return runtime.GOOS == "windows"
}

var (
	winDriveRe = regexp.MustCompile(`^[A-Za-z]:[\\/]`)
	uncRe      = regexp.MustCompile(`^\\\\[^\\/]+\\[^\\/]+`)
	hostRe     = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?$`)
)

// IsUNC reports whether p is a \\server\share path.
func IsUNC(p string) bool { return uncRe.MatchString(p) }

func (e Env) validLocalPath(p string) bool {
	if e.windows() {
		return winDriveRe.MatchString(p)
	}
	return filepath.IsAbs(p)
}

func (e Env) remoteDrive(p string) bool {
	return e.IsRemoteDrive != nil && winDriveRe.MatchString(p) && e.IsRemoteDrive(p)
}

// Validate checks the whole configuration and returns a *ValidationError
// listing every problem, or nil.
func Validate(c *Config, env Env) error {
	ve := &ValidationError{}

	if c.SchemaVersion != SchemaVersion {
		ve.add("schemaVersion", "Unsupported schema version %d (this build uses %d).", c.SchemaVersion, SchemaVersion)
	}

	// server
	if net.ParseIP(c.Server.ListenAddress) == nil {
		ve.add("server.listenAddress", "Enter an IP address to listen on, for example 0.0.0.0.")
	}
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		ve.add("server.port", "Port must be a number between 1 and 65535.")
	}
	for i, s := range c.Server.AllowedSubnets {
		if _, _, err := net.ParseCIDR(strings.TrimSpace(s)); err != nil {
			ve.add(fmt.Sprintf("server.allowedSubnets[%d]", i), "%q is not a subnet in CIDR form, for example 192.168.1.0/24.", s)
		}
	}

	// staging
	switch {
	case strings.TrimSpace(c.Staging.Path) == "":
		ve.add("staging.path", "Choose a folder on a local disk for staging.")
	case IsUNC(c.Staging.Path) || strings.HasPrefix(c.Staging.Path, "//"):
		ve.add("staging.path", "Staging must be on a local disk, not a network path.")
	case !env.validLocalPath(c.Staging.Path):
		ve.add("staging.path", "Enter a full path, for example D:\\ButterCams\\staging.")
	case env.remoteDrive(c.Staging.Path):
		ve.add("staging.path", "Staging must be on a local disk, not a mapped network drive.")
	}
	if c.Staging.MinFreePercent < 0 || c.Staging.MinFreePercent > 90 {
		ve.add("staging.minFreePercent", "Minimum free space must be between 0 and 90 percent.")
	}
	if c.Staging.MinFreeGB < 0 {
		ve.add("staging.minFreeGB", "Minimum free space in GB cannot be negative.")
	}

	// logging
	if c.Logging.RetentionDays < 1 || c.Logging.RetentionDays > 365 {
		ve.add("logging.retentionDays", "Log retention must be between 1 and 365 days.")
	}

	// destinations
	destIDs := map[string]bool{}
	for i, d := range c.Destinations {
		f := func(name string) string { return fmt.Sprintf("destinations[%d].%s", i, name) }
		validateDestination(ve, f, d, env)
		if destIDs[d.ID] {
			ve.add(f("id"), "Another destination already uses the id %q.", d.ID)
		}
		destIDs[d.ID] = true
	}

	// cameras
	camIDs := map[string]bool{}
	for i, cam := range c.Cameras {
		f := func(name string) string { return fmt.Sprintf("cameras[%d].%s", i, name) }
		validateCamera(ve, f, cam, destIDs)
		if camIDs[cam.ID] {
			ve.add(f("id"), "Another camera already uses the id %q.", cam.ID)
		}
		camIDs[cam.ID] = true
	}

	if len(ve.Fields) > 0 {
		return ve
	}
	return nil
}

func validateDestination(ve *ValidationError, f func(string) string, d Destination, env Env) {
	if !naming.ValidID(d.ID) {
		ve.add(f("id"), "The id may contain lowercase letters, digits and hyphens, 1 to 48 characters.")
	} else if d.ID == "staging" {
		ve.add(f("id"), `The id "staging" is reserved.`)
	}
	if d.Name == "" {
		ve.add(f("name"), "Enter a name for the destination.")
	} else if len(d.Name) > 100 {
		ve.add(f("name"), "The name is too long (100 characters at most).")
	}
	switch d.Type {
	case DestLocal:
		switch {
		case d.Path == "":
			ve.add(f("path"), "Enter the folder to store recordings in.")
		case IsUNC(d.Path):
			ve.add(f("path"), `This is a network path. Change the type to "Network share".`)
		case !env.validLocalPath(d.Path):
			ve.add(f("path"), "Enter a full path, for example E:\\Recordings.")
		case env.remoteDrive(d.Path):
			ve.add(f("path"), MappedDriveMessage)
		}
		if d.Username != "" || d.PasswordEnc != "" {
			ve.add(f("username"), "A local destination does not use a user name or password.")
		}
	case DestUNC:
		if !IsUNC(d.Path) {
			ve.add(f("path"), `Enter the share as \\server\share\folder.`)
		}
	default:
		ve.add(f("type"), `The type must be "local" or "unc".`)
	}
	if d.CapGB <= 0 {
		ve.add(f("capGB"), "Enter the storage limit in GB (greater than 0).")
	}
	if d.MaxAgeDays != nil && *d.MaxAgeDays < 1 {
		ve.add(f("maxAgeDays"), "Maximum age must be at least 1 day, or empty for no limit.")
	}
}

func validateCamera(ve *ValidationError, f func(string) string, cam Camera, destIDs map[string]bool) {
	if !naming.ValidID(cam.ID) {
		ve.add(f("id"), "The id may contain lowercase letters, digits and hyphens, 1 to 48 characters.")
	}
	if cam.Name == "" {
		ve.add(f("name"), "Enter a name for the camera.")
	} else if len(cam.Name) > 100 {
		ve.add(f("name"), "The name is too long (100 characters at most).")
	}
	switch {
	case cam.Host == "":
		ve.add(f("host"), "Enter the camera's IP address or host name.")
	case strings.Contains(cam.Host, "://") || strings.ContainsAny(cam.Host, "/@ "):
		ve.add(f("host"), "Enter only the IP address or host name here, or paste the full URL into the URL box.")
	case net.ParseIP(cam.Host) == nil && !hostRe.MatchString(cam.Host):
		ve.add(f("host"), "%q is not a valid IP address or host name.", cam.Host)
	}
	if cam.Port < 1 || cam.Port > 65535 {
		ve.add(f("port"), "Port must be a number between 1 and 65535.")
	}
	if !strings.HasPrefix(cam.Path, "/") {
		ve.add(f("path"), "The stream path must start with /.")
	} else if strings.ContainsAny(cam.Path, " \t\r\n") {
		ve.add(f("path"), "The stream path cannot contain spaces.")
	}
	if cam.Transport != "tcp" && cam.Transport != "udp" {
		ve.add(f("transport"), `Transport must be "tcp" or "udp".`)
	}
	if cam.RecordingMode != "continuous" {
		ve.add(f("recordingMode"), `This version only supports the recording mode "continuous".`)
	}
	seen := map[string]bool{}
	for _, id := range cam.DestinationIDs {
		if !destIDs[id] {
			ve.add(f("destinationIds"), "Destination %q does not exist.", id)
		}
		if seen[id] {
			ve.add(f("destinationIds"), "Destination %q is listed twice.", id)
		}
		seen[id] = true
	}
}
