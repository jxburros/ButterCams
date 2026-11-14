package winsvc

import (
	"os"
	"path/filepath"
	"runtime"
)

// EnvDataRoot overrides the data root. The guardian sets it for its children.
const EnvDataRoot = "BUTTERCAMS_DATA_ROOT"

// ResolveDataRoot returns the data root: an explicit value, then the
// BUTTERCAMS_DATA_ROOT environment variable, then the registry (Windows), then
// the platform default.
func ResolveDataRoot(explicit string) string {
	if explicit != "" {
		return filepath.Clean(explicit)
	}
	if v := os.Getenv(EnvDataRoot); v != "" {
		return filepath.Clean(v)
	}
	if v, err := ReadDataRoot(); err == nil && v != "" {
		return filepath.Clean(v)
	}
	if runtime.GOOS == "windows" {
		return DefaultDataRoot
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".buttercams")
	}
	return "buttercams-data"
}
