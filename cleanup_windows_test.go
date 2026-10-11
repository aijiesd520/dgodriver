package dgodriver

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestStopRetriesLockedTemporaryProfile(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "owned-profile")
	if err := os.Mkdir(profile, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(profile, "locked-file")
	if err := os.WriteFile(path, []byte("locked"), 0600); err != nil {
		t.Fatal(err)
	}
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	// Omit FILE_SHARE_DELETE so RemoveAll fails until the handle is released.
	handle, err := syscall.CreateFile(name, syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, nil,
		syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() { once.Do(func() { _ = syscall.CloseHandle(handle) }) }
	t.Cleanup(release)
	if err := os.Remove(path); err == nil {
		t.Fatal("test file did not prevent deletion")
	}
	go func() {
		time.Sleep(250 * time.Millisecond)
		release()
	}()
	b := &Browser{
		Config:       Config{UserDataDir: profile},
		ownedProfile: true,
		stopped:      make(chan struct{}),
		client:       &http.Client{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(profile); !os.IsNotExist(err) {
		t.Fatalf("temporary profile survived released file lock: %v", err)
	}
}
