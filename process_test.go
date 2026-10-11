package dgodriver

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

// The parent creates an ordinary child which inherits its process group on Unix.
// The child serves a TCP probe so the test can check liveness without PID reuse
// or zombie-process false positives, and without scanning unrelated browsers.
func TestProcessTreeHelper(t *testing.T) {
	switch os.Getenv("DGODRIVER_PROCESS_HELPER") {
	case "parent":
		child := exec.Command(os.Args[0], "-test.run=^TestProcessTreeHelper$")
		child.Env = append(os.Environ(), "DGODRIVER_PROCESS_HELPER=child")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Run(); err != nil {
			t.Fatal(err)
		}
	case "child":
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		if err := json.NewEncoder(os.Stdout).Encode(listener.Addr().String()); err != nil {
			t.Fatal(err)
		}
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_, _ = io.WriteString(conn, "alive")
			_ = conn.Close()
		}
	}
}

func startProcessHelper(t *testing.T, role string) (*exec.Cmd, chan struct{}, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessTreeHelper$")
	cmd.Env = append(os.Environ(), "DGODRIVER_PROCESS_HELPER="+role)
	configureProcess(cmd)
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			_ = killProcessTree(cmd)
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("process helper did not exit")
		}
	})
	ready := make(chan string, 1)
	go func() {
		var address string
		_ = json.NewDecoder(output).Decode(&address)
		ready <- address
	}()
	select {
	case address := <-ready:
		if address == "" {
			t.Fatal("process helper did not report its probe address")
		}
		return cmd, done, address
	case <-time.After(10 * time.Second):
		t.Fatal("timed out starting process helper")
		return nil, nil, ""
	}
}

func processProbeAlive(address string) bool {
	conn, err := net.DialTimeout("tcp", address, 200*time.Millisecond)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	data, err := io.ReadAll(conn)
	return err == nil && string(data) == "alive"
}

func TestBrowserStopKillsOnlyOwnedProcessTree(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("process tree regression runs on Windows, Linux and macOS")
	}
	cmd, done, childAddress := startProcessHelper(t, "parent")
	_, _, unrelatedAddress := startProcessHelper(t, "child")
	if !processProbeAlive(childAddress) || !processProbeAlive(unrelatedAddress) {
		t.Fatal("process helpers are not alive before Stop")
	}
	b := &Browser{process: cmd, processDone: done, stopped: make(chan struct{}), client: &http.Client{}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for processProbeAlive(childAddress) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if processProbeAlive(childAddress) {
		t.Fatal("child survived forced browser shutdown")
	}
	if !processProbeAlive(unrelatedAddress) {
		t.Fatal("forced shutdown killed an unrelated process")
	}
	if err := b.Close(); err != nil {
		t.Fatalf("repeated Stop failed: %v", err)
	}
}
