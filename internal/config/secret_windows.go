//go:build windows

package config

import (
	"encoding/base64"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// SecretsProtected reports whether Protect really encrypts on this platform.
const SecretsProtected = true

// Protect encrypts a secret with Windows DPAPI at machine scope, so the
// service account, the workers and the mover can all decrypt it but the value
// is useless on any other computer. The result is base64.
func Protect(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	data := []byte(plain)
	in := windows.DataBlob{Size: uint32(len(data)), Data: &data[0]}
	var out windows.DataBlob
	err := windows.CryptProtectData(&in, nil, nil, 0, nil,
		windows.CRYPTPROTECT_LOCAL_MACHINE|windows.CRYPTPROTECT_UI_FORBIDDEN, &out)
	if err != nil {
		return "", fmt.Errorf("protecting secret: %w", err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return base64.StdEncoding.EncodeToString(unsafe.Slice(out.Data, out.Size)), nil
}

// Unprotect reverses Protect.
func Unprotect(enc string) (string, error) {
	if enc == "" {
		return "", nil
	}
	data, err := base64.StdEncoding.DecodeString(enc)
	if err != nil || len(data) == 0 {
		return "", fmt.Errorf("stored secret is not valid base64")
	}
	in := windows.DataBlob{Size: uint32(len(data)), Data: &data[0]}
	var out windows.DataBlob
	err = windows.CryptUnprotectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out)
	if err != nil {
		return "", fmt.Errorf("stored secret cannot be decrypted on this computer: %w", err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return string(unsafe.Slice(out.Data, out.Size)), nil
}
