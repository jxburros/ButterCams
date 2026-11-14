//go:build !windows

package winsvc

import (
	"context"
	"errors"
	"strings"
)

var errWindowsOnly = errors.New("this command only works on Windows")

// IsWindowsService is always false outside Windows.
func IsWindowsService() bool { return false }

// RunService is only available on Windows.
func RunService(func(ctx context.Context) error) error { return errWindowsOnly }

// InstallService is only available on Windows.
func InstallService(ServiceConfig) error { return errWindowsOnly }

// StartService is only available on Windows.
func StartService() error { return errWindowsOnly }

// StopService is only available on Windows.
func StopService() error { return errWindowsOnly }

// UninstallService is only available on Windows.
func UninstallService() error { return errWindowsOnly }

type nopEventLog struct{}

func (nopEventLog) Info(string)    {}
func (nopEventLog) Warning(string) {}
func (nopEventLog) Error(string)   {}
func (nopEventLog) Close()         {}

// OpenEventLog returns a log that discards everything.
func OpenEventLog() EventLog { return nopEventLog{} }

// ReadDataRoot has no registry to read outside Windows.
func ReadDataRoot() (string, error) { return "", errWindowsOnly }

// WriteDataRoot is a no-op outside Windows.
func WriteDataRoot(string) error { return nil }

// DeleteRegistry is a no-op outside Windows.
func DeleteRegistry() error { return nil }

// AddFirewallRule is a no-op outside Windows.
func AddFirewallRule(int) error { return nil }

// RemoveFirewallRule is a no-op outside Windows.
func RemoveFirewallRule() error { return nil }

// ShareRoot returns the \\server\share part of a UNC path.
func ShareRoot(unc string) string {
	parts := strings.Split(strings.TrimLeft(unc, `\`), `\`)
	if len(parts) < 2 {
		return unc
	}
	return `\\` + parts[0] + `\` + parts[1]
}

// ConnectShare is a no-op outside Windows.
func ConnectShare(string, string, string) error { return nil }

// GrantLogonAsService is only available on Windows.
func GrantLogonAsService(string) error { return errWindowsOnly }
