// Package winsvc wraps everything ButterCams needs from the operating system:
// the Windows service control manager, Job Objects, the Event Log, WNet share
// connections, the registry, atomic file replacement and disk information.
//
// Files ending in _windows.go hold the real implementations. Files ending in
// _other.go hold small stand-ins so that the recorder, mover, guardian and
// their tests also build and run on Linux and macOS during development.
package winsvc

// ServiceName is the Windows service name.
const ServiceName = "ButterCams"

// ServiceDisplayName is shown in services.msc.
const ServiceDisplayName = "ButterCams Recorder"

// ServiceDescription is shown in services.msc.
const ServiceDescription = "Records RTSP cameras continuously into one-hour MP4 files and copies them to storage."

// EventSource is the Windows Event Log source.
const EventSource = "ButterCams"

// FirewallRuleName is the inbound rule the installer adds for the web port.
const FirewallRuleName = "ButterCams Web"

// RegistryKey is the HKLM key that stores the data root.
const RegistryKey = `SOFTWARE\ButterCams`

// DefaultDataRoot is used when the registry has no data root.
const DefaultDataRoot = `C:\ProgramData\ButterCams`

// EventLog writes to the Windows Event Log. It is a no-op elsewhere.
type EventLog interface {
	Info(msg string)
	Warning(msg string)
	Error(msg string)
	Close()
}

// ServiceConfig describes how the service is registered.
type ServiceConfig struct {
	ExePath  string
	Account  string // empty = LocalSystem
	Password string
}
