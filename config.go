package dgodriver

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Config's zero value launches a visible Chromium browser with a temporary profile.
// Host and Port together attach to an existing browser; WebSocketURL attaches directly.
type Config struct {
	BrowserExecutablePath string
	Browser               string // auto, chrome, brave, msedge, chromium
	UserDataDir           string
	Headless              bool
	NoSandbox             bool
	Expert                bool
	EnableWebRTC          bool
	DisableWebGL          bool
	UserAgent             string
	Lang                  string
	BrowserArgs           []string
	Extensions            []string
	Host                  string
	Port                  int
	WebSocketURL          string
	StartupTimeout        time.Duration
	HTTPTimeout           time.Duration
}

func (c *Config) AddArgument(arg string) error {
	for _, prefix := range []string{"--headless", "--user-data-dir", "--no-sandbox", "--lang=", "--remote-debugging-"} {
		if strings.HasPrefix(arg, prefix) {
			return fmt.Errorf("dgodriver: use Config fields for %s", arg)
		}
	}
	c.BrowserArgs = append(c.BrowserArgs, arg)
	return nil
}
func (c *Config) AddExtension(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if _, err = os.Stat(abs); err != nil {
		return err
	}
	c.Extensions = append(c.Extensions, abs)
	return nil
}
func MergeDisableFeaturesArgs(args []string) []string {
	var result, features []string
	seen := map[string]bool{}
	for _, a := range args {
		if strings.HasPrefix(a, "--disable-features=") {
			for _, f := range strings.Split(strings.TrimPrefix(a, "--disable-features="), ",") {
				if f != "" && !seen[f] {
					features = append(features, f)
					seen[f] = true
				}
			}
		} else {
			result = append(result, a)
		}
	}
	if len(features) > 0 {
		result = append(result, "--disable-features="+strings.Join(features, ","))
	}
	return result
}
func (c Config) Arguments() []string {
	args := []string{"--no-first-run", "--no-default-browser-check", "--no-service-autorun", "--homepage=about:blank", "--no-pings", "--password-store=basic", "--disable-infobars", "--disable-breakpad", "--disable-dev-shm-usage", "--disable-session-crashed-bubble", "--disable-search-engine-choice-screen", "--disable-features=IsolateOrigins,DisableLoadExtensionCommandLineSwitch,site-per-process"}
	if c.UserDataDir != "" {
		args = append(args, "--user-data-dir="+c.UserDataDir)
	}
	if c.Headless {
		args = append(args, "--headless=new")
	}
	if c.NoSandbox {
		args = append(args, "--no-sandbox")
	}
	if c.Expert {
		args = append(args, "--disable-site-isolation-trials")
	}
	if !c.EnableWebRTC {
		args = append(args, "--webrtc-ip-handling-policy=disable_non_proxied_udp", "--force-webrtc-ip-handling-policy")
	}
	if c.DisableWebGL {
		args = append(args, "--disable-webgl", "--disable-webgl2")
	}
	if c.UserAgent != "" {
		args = append(args, "--user-agent="+c.UserAgent)
	}
	if c.Lang != "" {
		args = append(args, "--lang="+c.Lang)
	}
	args = append(args, "--remote-debugging-address="+c.Host, "--remote-debugging-port="+strconv.Itoa(c.Port))
	if len(c.Extensions) > 0 {
		args = append(args, "--enable-unsafe-extension-debugging", "--load-extension="+strings.Join(c.Extensions, ","))
	}
	args = append(args, c.BrowserArgs...)
	return MergeDisableFeaturesArgs(args)
}
func FindExecutable(browser string) (string, error) {
	if browser == "" || browser == "auto" {
		for _, name := range []string{"chrome", "chromium", "msedge", "brave"} {
			if path, err := FindExecutable(name); err == nil {
				return path, nil
			}
		}
		return "", fmt.Errorf("dgodriver: no Chromium browser found; set BrowserExecutablePath")
	}
	names := map[string][]string{"chrome": {"google-chrome", "google-chrome-stable", "chrome"}, "chromium": {"chromium", "chromium-browser"}, "msedge": {"microsoft-edge", "microsoft-edge-stable", "msedge"}, "brave": {"brave-browser", "brave"}}
	list, ok := names[browser]
	if !ok {
		return "", fmt.Errorf("dgodriver: unknown browser %q", browser)
	}
	for _, n := range list {
		if path, err := exec.LookPath(n); err == nil {
			return path, nil
		}
	}
	var candidates []string
	if runtime.GOOS == "windows" {
		relative := map[string]string{"chrome": "Google/Chrome/Application/chrome.exe", "chromium": "Chromium/Application/chrome.exe", "msedge": "Microsoft/Edge/Application/msedge.exe", "brave": "BraveSoftware/Brave-Browser/Application/brave.exe"}[browser]
		for _, base := range []string{os.Getenv("PROGRAMFILES"), os.Getenv("PROGRAMFILES(X86)"), os.Getenv("LOCALAPPDATA")} {
			if base != "" {
				candidates = append(candidates, filepath.Join(base, relative))
			}
		}
	} else if runtime.GOOS == "darwin" {
		app := map[string]string{"chrome": "Google Chrome", "chromium": "Chromium", "msedge": "Microsoft Edge", "brave": "Brave Browser"}[browser]
		candidates = append(candidates, "/Applications/"+app+".app/Contents/MacOS/"+app)
	}
	for _, p := range candidates {
		if s, err := os.Stat(p); err == nil && !s.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("dgodriver: %s executable not found", browser)
}

// unpackExtension accepts ZIP and CRX (ZIP payload) without path traversal.
func unpackExtension(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return path, nil
	}
	z, err := zip.OpenReader(path)
	if err != nil {
		return "", err
	}
	defer z.Close()
	dir, err := os.MkdirTemp("", "dgodriver-extension-")
	if err != nil {
		return "", err
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(dir)
		}
	}()
	for _, f := range z.File {
		name := filepath.FromSlash(f.Name)
		if !filepath.IsLocal(name) {
			return "", fmt.Errorf("dgodriver: unsafe extension path %q", f.Name)
		}
		target := filepath.Join(dir, name)
		if f.FileInfo().Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("dgodriver: extension symlink %q", name)
		}
		if f.FileInfo().IsDir() {
			if err = os.MkdirAll(target, 0755); err != nil {
				return "", err
			}
			continue
		}
		if err = os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return "", err
		}
		src, e := f.Open()
		if e != nil {
			return "", e
		}
		dst, e := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
		if e != nil {
			src.Close()
			return "", e
		}
		_, e = io.Copy(dst, src)
		src.Close()
		closeErr := dst.Close()
		if e != nil {
			return "", e
		}
		if closeErr != nil {
			return "", closeErr
		}
	}
	if _, err = os.Stat(filepath.Join(dir, "manifest.json")); err != nil {
		return "", fmt.Errorf("dgodriver: extension missing manifest.json: %w", err)
	}
	ok = true
	return dir, nil
}
