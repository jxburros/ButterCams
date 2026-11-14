//go:build !windows && !linux

package winsvc

import "os/exec"

func prepareChild(*exec.Cmd) {}
