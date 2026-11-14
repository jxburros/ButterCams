//go:build windows

package winsvc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

// IsWindowsService reports whether the process was started by the service
// control manager.
func IsWindowsService() bool {
	ok, err := svc.IsWindowsService()
	return err == nil && ok
}

type handler struct {
	run func(ctx context.Context) error
}

// StopTimeout is how long the service waits for the guardian to finalize
// files after a stop or (pre)shutdown request. The guardian itself gives its
// workers 15 s (spec 4.1).
const StopTimeout = 25 * time.Second

func (h *handler) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	const accepts = svc.AcceptStop | svc.AcceptShutdown | svc.AcceptPreShutdown
	status <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.run(ctx) }()

	status <- svc.Status{State: svc.Running, Accepts: accepts}
	for {
		select {
		case err := <-done:
			// The guardian stopped without being asked to. Exit without
			// reporting SERVICE_STOPPED so the SCM counts it as a failure and
			// runs the recovery actions (restart after 5 s).
			if err != nil {
				fmt.Fprintln(os.Stderr, "buttercams: guardian failed:", err)
			}
			os.Exit(1)
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown, svc.PreShutdown:
				status <- svc.Status{State: svc.StopPending, WaitHint: uint32(StopTimeout / time.Millisecond)}
				cancel()
				select {
				case <-done:
				case <-time.After(StopTimeout):
				}
				return false, 0
			}
		}
	}
}

// RunService runs fn as the ButterCams Windows service. fn must return soon
// after its context is cancelled.
func RunService(fn func(ctx context.Context) error) error {
	return svc.Run(ServiceName, &handler{run: fn})
}

// InstallService registers (or updates) the service: Automatic (Delayed
// Start), restart after 5 s on every failure, failure count reset after one
// day (spec 9 and 13).
func InstallService(cfg ServiceConfig) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connecting to the service manager: %w", err)
	}
	defer m.Disconnect()

	mc := mgr.Config{
		ServiceType:      windows.SERVICE_WIN32_OWN_PROCESS,
		StartType:        mgr.StartAutomatic,
		ErrorControl:     mgr.ErrorNormal,
		DisplayName:      ServiceDisplayName,
		Description:      ServiceDescription,
		DelayedAutoStart: true,
	}
	if cfg.Account != "" {
		mc.ServiceStartName = cfg.Account
		mc.Password = cfg.Password
	}

	s, err := m.OpenService(ServiceName)
	if err == nil {
		// Upgrade: keep the registration, refresh its settings.
		cur, cerr := s.Config()
		if cerr == nil {
			cur.BinaryPathName = quoteArg(cfg.ExePath) + " service"
			cur.StartType = mc.StartType
			cur.DelayedAutoStart = true
			cur.DisplayName = mc.DisplayName
			cur.Description = mc.Description
			if cfg.Account != "" {
				cur.ServiceStartName = cfg.Account
				cur.Password = cfg.Password
			}
			if uerr := s.UpdateConfig(cur); uerr != nil {
				s.Close()
				return fmt.Errorf("updating the service: %w", uerr)
			}
		}
	} else {
		s, err = m.CreateService(ServiceName, cfg.ExePath, mc, "service")
		if err != nil {
			return fmt.Errorf("creating the service: %w", err)
		}
	}
	defer s.Close()

	restart := mgr.RecoveryAction{Type: mgr.ServiceRestart, Delay: 5 * time.Second}
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{restart, restart, restart}, 86400); err != nil {
		return fmt.Errorf("setting recovery actions: %w", err)
	}
	if err := s.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return fmt.Errorf("setting recovery flag: %w", err)
	}
	// Give the guardian time to finalize files at OS shutdown.
	setPreshutdownTimeout(s.Handle, StopTimeout+5*time.Second)

	if err := eventlog.InstallAsEventCreate(EventSource, eventlog.Error|eventlog.Warning|eventlog.Info); err != nil {
		// Already registered is fine.
		if _, kerr := registry.OpenKey(registry.LOCAL_MACHINE,
			`SYSTEM\CurrentControlSet\Services\EventLog\Application\`+EventSource, registry.QUERY_VALUE); kerr != nil {
			return fmt.Errorf("registering the event source: %w", err)
		}
	}
	return nil
}

type servicePreshutdownInfo struct {
	timeoutMs uint32
}

func setPreshutdownTimeout(h windows.Handle, d time.Duration) {
	info := servicePreshutdownInfo{timeoutMs: uint32(d / time.Millisecond)}
	windows.ChangeServiceConfig2(h, windows.SERVICE_CONFIG_PRESHUTDOWN_INFO, (*byte)(unsafe.Pointer(&info)))
}

func quoteArg(s string) string {
	return `"` + s + `"`
}

// StartService starts the service.
func StartService() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(ServiceName)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		return err
	}
	return nil
}

// StopService stops the service and waits for it to stop.
func StopService() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(ServiceName)
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return nil
		}
		return err
	}
	defer s.Close()
	st, err := s.Query()
	if err != nil {
		return err
	}
	if st.State == svc.Stopped {
		return nil
	}
	if _, err := s.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
		return err
	}
	deadline := time.Now().Add(StopTimeout + 15*time.Second)
	for time.Now().Before(deadline) {
		st, err = s.Query()
		if err != nil {
			return err
		}
		if st.State == svc.Stopped {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("the service did not stop in time")
}

// UninstallService stops and removes the service and its event source.
func UninstallService() error {
	if err := StopService(); err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(ServiceName)
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return nil
		}
		return err
	}
	defer s.Close()
	if err := s.Delete(); err != nil {
		return err
	}
	eventlog.Remove(EventSource)
	return nil
}

type winEventLog struct{ l *eventlog.Log }

func (w winEventLog) Info(msg string)    { w.l.Info(1, msg) }
func (w winEventLog) Warning(msg string) { w.l.Warning(2, msg) }
func (w winEventLog) Error(msg string)   { w.l.Error(3, msg) }
func (w winEventLog) Close()             { w.l.Close() }

type nopEventLog struct{}

func (nopEventLog) Info(string)    {}
func (nopEventLog) Warning(string) {}
func (nopEventLog) Error(string)   {}
func (nopEventLog) Close()         {}

// OpenEventLog opens the ButterCams event source. It never fails: when the
// source is not registered (development runs) the returned log discards.
func OpenEventLog() EventLog {
	l, err := eventlog.Open(EventSource)
	if err != nil {
		return nopEventLog{}
	}
	return winEventLog{l}
}

// ReadDataRoot returns the data root stored by the installer.
func ReadDataRoot() (string, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, RegistryKey, registry.QUERY_VALUE)
	if err != nil {
		return "", err
	}
	defer k.Close()
	v, _, err := k.GetStringValue("DataRoot")
	return v, err
}

// WriteDataRoot stores the data root at HKLM\SOFTWARE\ButterCams\DataRoot.
func WriteDataRoot(path string) error {
	k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, RegistryKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue("DataRoot", path)
}

// DeleteRegistry removes HKLM\SOFTWARE\ButterCams.
func DeleteRegistry() error {
	err := registry.DeleteKey(registry.LOCAL_MACHINE, RegistryKey)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	return err
}

// AddFirewallRule allows inbound TCP on port for the Domain and Private
// profiles only (spec 13).
func AddFirewallRule(port int) error {
	RemoveFirewallRule()
	out, err := exec.Command("netsh", "advfirewall", "firewall", "add", "rule",
		"name="+FirewallRuleName, "dir=in", "action=allow", "protocol=TCP",
		"localport="+strconv.Itoa(port), "profile=domain,private").CombinedOutput()
	if err != nil {
		return fmt.Errorf("netsh: %v: %s", err, out)
	}
	return nil
}

// RemoveFirewallRule deletes the inbound rule.
func RemoveFirewallRule() error {
	return exec.Command("netsh", "advfirewall", "firewall", "delete", "rule", "name="+FirewallRuleName).Run()
}
