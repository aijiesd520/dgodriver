//go:build !windows

package dgodriver

import "os/exec"

func configureProcess(cmd *exec.Cmd) {}
