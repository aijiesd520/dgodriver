//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package dgodriver

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func configureProcess(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

func killProcessTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	// The browser starts in its own process group, inherited by its children.
	groupErr := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(groupErr, syscall.ESRCH) {
		groupErr = nil
	}
	rootErr := cmd.Process.Kill()
	if errors.Is(rootErr, os.ErrProcessDone) {
		rootErr = nil
	}
	return errors.Join(groupErr, rootErr)
}
