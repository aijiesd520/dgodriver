package dgodriver

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestBrowserExecutableOverride(t *testing.T) {
	executable, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DGODRIVER_BROWSER", executable)
	path, err := FindExecutable("auto")
	if err != nil || path != executable {
		t.Fatalf("executable override: %q, %v", path, err)
	}
	t.Setenv("PATH", filepath.Dir(executable))
	t.Setenv("DGODRIVER_BROWSER", filepath.Base(executable))
	path, err = FindExecutable("chrome")
	if err != nil || path != executable {
		t.Fatalf("override from PATH: %q, %v", path, err)
	}
	t.Setenv("DGODRIVER_BROWSER", filepath.Join(t.TempDir(), "missing-env-browser"))
	if _, err := FindExecutable("auto"); err == nil || !strings.Contains(err.Error(), "DGODRIVER_BROWSER") {
		t.Fatalf("invalid override silently fell back: %v", err)
	}
	// An explicit Config path must take precedence, even over an invalid override.
	explicit := filepath.Join(t.TempDir(), "missing-explicit-browser")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = Start(ctx, Config{BrowserExecutablePath: explicit, NoSandbox: true})
	var commandErr *exec.Error
	var pathErr *os.PathError
	explicitError := errors.As(err, &commandErr) && commandErr.Name == explicit || errors.As(err, &pathErr) && pathErr.Path == explicit
	if !explicitError {
		t.Fatalf("explicit executable did not take precedence: %v", err)
	}
	_, err = Start(ctx, Config{NoSandbox: true})
	if err == nil || !strings.Contains(err.Error(), "DGODRIVER_BROWSER") {
		t.Fatalf("Start ignored an invalid environment override: %v", err)
	}
}

func TestLinuxRootLaunchRequiresExplicitNoSandbox(t *testing.T) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Skip("requires Linux root")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := Start(ctx, Config{BrowserExecutablePath: filepath.Join(t.TempDir(), "missing-browser")})
	if err == nil || !strings.Contains(err.Error(), "Config.NoSandbox = true") || !strings.Contains(err.Error(), "non-root") {
		t.Fatalf("root launch did not explain the required action: %v", err)
	}
}
