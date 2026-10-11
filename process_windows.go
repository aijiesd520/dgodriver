//go:build windows

package dgodriver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func configureProcess(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true} }

// taskkill targets only this browser's PID and descendants, never other browsers.
func killProcessTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	// Teardown must still work when the caller's context is already cancelled.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	kill := exec.CommandContext(ctx, "taskkill", "/F", "/T", "/PID", strconv.Itoa(cmd.Process.Pid))
	configureProcess(kill)
	output, err := kill.CombinedOutput()
	if err == nil {
		return nil
	}
	rootErr := cmd.Process.Kill()
	if errors.Is(rootErr, os.ErrProcessDone) {
		return nil
	}
	return errors.Join(fmt.Errorf("dgodriver: kill process tree: %w: %s", err, strings.TrimSpace(string(output))), rootErr)
}
