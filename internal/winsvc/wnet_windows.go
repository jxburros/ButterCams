//go:build windows

package winsvc

import (
	"fmt"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modMpr                    = windows.NewLazySystemDLL("mpr.dll")
	procWNetAddConnection2    = modMpr.NewProc("WNetAddConnection2W")
	procWNetCancelConnection2 = modMpr.NewProc("WNetCancelConnection2W")

	modAdvapi32               = windows.NewLazySystemDLL("advapi32.dll")
	procLsaOpenPolicy         = modAdvapi32.NewProc("LsaOpenPolicy")
	procLsaAddAccountRights   = modAdvapi32.NewProc("LsaAddAccountRights")
	procLsaClose              = modAdvapi32.NewProc("LsaClose")
	procLsaNtStatusToWinError = modAdvapi32.NewProc("LsaNtStatusToWinError")
)

type netResource struct {
	scope       uint32
	resType     uint32
	displayType uint32
	usage       uint32
	localName   *uint16
	remoteName  *uint16
	comment     *uint16
	provider    *uint16
}

const (
	resourceTypeDisk               = 1
	errorSessionCredentialConflict = 1219
	errorAlreadyAssigned           = 85
)

// ShareRoot returns the \\server\share part of a UNC path.
func ShareRoot(unc string) string {
	parts := strings.Split(strings.TrimLeft(unc, `\`), `\`)
	if len(parts) < 2 {
		return unc
	}
	return `\\` + parts[0] + `\` + parts[1]
}

// ConnectShare authenticates to the share that holds uncPath with the given
// credentials (WNetAddConnection2, no drive letter). With an empty user it
// does nothing and the service account's identity is used (spec 7.3).
func ConnectShare(uncPath, user, password string) error {
	if user == "" {
		return nil
	}
	root := ShareRoot(uncPath)
	remote, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return err
	}
	u, err := windows.UTF16PtrFromString(user)
	if err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(password)
	if err != nil {
		return err
	}
	nr := netResource{resType: resourceTypeDisk, remoteName: remote}
	call := func() syscall.Errno {
		r, _, _ := procWNetAddConnection2.Call(uintptr(unsafe.Pointer(&nr)), uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(u)), 0)
		return syscall.Errno(r)
	}
	code := call()
	if code == errorSessionCredentialConflict {
		// Connected with other credentials: drop that connection and retry.
		procWNetCancelConnection2.Call(uintptr(unsafe.Pointer(remote)), 0, 1)
		code = call()
	}
	if code != 0 && code != errorAlreadyAssigned {
		return fmt.Errorf("connecting to %s as %s: %w", root, user, code)
	}
	return nil
}

type lsaUnicodeString struct {
	length        uint16
	maximumLength uint16
	buffer        *uint16
}

type lsaObjectAttributes struct {
	length                   uint32
	rootDirectory            windows.Handle
	objectName               *lsaUnicodeString
	attributes               uint32
	securityDescriptor       uintptr
	securityQualityOfService uintptr
}

// GrantLogonAsService gives account the "Log on as a service" right
// (SeServiceLogonRight), which a service account needs (spec 13).
func GrantLogonAsService(account string) error {
	sid, _, _, err := windows.LookupSID("", account)
	if err != nil {
		return fmt.Errorf("looking up account %q: %w", account, err)
	}
	var attrs lsaObjectAttributes
	attrs.length = uint32(unsafe.Sizeof(attrs))
	var policy windows.Handle
	const policyCreateAccount, policyLookupNames = 0x10, 0x800
	st, _, _ := procLsaOpenPolicy.Call(0, uintptr(unsafe.Pointer(&attrs)), policyCreateAccount|policyLookupNames, uintptr(unsafe.Pointer(&policy)))
	if st != 0 {
		return fmt.Errorf("LsaOpenPolicy: %w", ntStatusErr(st))
	}
	defer procLsaClose.Call(uintptr(policy))

	right, err := windows.UTF16FromString("SeServiceLogonRight")
	if err != nil {
		return err
	}
	us := lsaUnicodeString{
		length:        uint16((len(right) - 1) * 2),
		maximumLength: uint16(len(right) * 2),
		buffer:        &right[0],
	}
	st, _, _ = procLsaAddAccountRights.Call(uintptr(policy), uintptr(unsafe.Pointer(sid)), uintptr(unsafe.Pointer(&us)), 1)
	if st != 0 {
		return fmt.Errorf("LsaAddAccountRights: %w", ntStatusErr(st))
	}
	return nil
}

func ntStatusErr(st uintptr) error {
	code, _, _ := procLsaNtStatusToWinError.Call(st)
	return syscall.Errno(code)
}
