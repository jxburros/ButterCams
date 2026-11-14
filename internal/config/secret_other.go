//go:build !windows

package config

import (
	"encoding/base64"
	"fmt"
	"strings"
)

// SecretsProtected reports whether Protect really encrypts on this platform.
// It does not outside Windows: DPAPI does not exist there, and non-Windows
// builds are for development and tests only. The guardian logs a warning.
const SecretsProtected = false

const devPrefix = "dev-unprotected:"

// Protect encodes a secret. Outside Windows this is NOT encryption.
func Protect(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	return devPrefix + base64.StdEncoding.EncodeToString([]byte(plain)), nil
}

// Unprotect reverses Protect.
func Unprotect(enc string) (string, error) {
	if enc == "" {
		return "", nil
	}
	if !strings.HasPrefix(enc, devPrefix) {
		return "", fmt.Errorf("stored secret was protected on another platform")
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(enc, devPrefix))
	if err != nil {
		return "", fmt.Errorf("stored secret is not valid base64")
	}
	return string(b), nil
}
