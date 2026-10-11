package dgodriver

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestStopPreservesUserProfileAndRemovesOwnedExtensions(t *testing.T) {
	profile := t.TempDir()
	marker := filepath.Join(profile, "user-data")
	if err := os.WriteFile(marker, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	extension := filepath.Join(t.TempDir(), "unpacked-extension")
	if err := os.Mkdir(extension, 0700); err != nil {
		t.Fatal(err)
	}
	b := &Browser{
		Config:        Config{UserDataDir: profile},
		extensionDirs: []string{extension},
		stopped:       make(chan struct{}),
		client:        &http.Client{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "keep" {
		t.Fatalf("user profile was removed: %q, %v", data, err)
	}
	if _, err := os.Stat(extension); !os.IsNotExist(err) {
		t.Fatalf("owned extension directory was retained: %v", err)
	}
}
