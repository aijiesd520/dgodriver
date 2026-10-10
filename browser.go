package dgodriver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aijiesd520/dgodriver/cdp"
)

type BrowserInfo struct {
	Browser              string `json:"Browser"`
	ProtocolVersion      string `json:"Protocol-Version"`
	UserAgent            string `json:"User-Agent"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

type Browser struct {
	*Connection
	Config        Config
	Info          BrowserInfo
	mu            sync.Mutex
	targets       map[string]*Tab
	order         []string
	contexts      map[string]*BrowserContext
	downloadMu    sync.Mutex
	downloadPaths map[string]string
	process       *exec.Cmd
	processDone   chan struct{}
	stderr        lockedBuffer
	ownedProfile  bool
	extensionDirs []string
	client        *http.Client
	stopOnce      sync.Once
	stopErr       error
	stopped       chan struct{}
}
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}
func (b *lockedBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.b.String() }

// Start launches Chromium, or attaches when Config has Host+Port or WebSocketURL.
// ctx bounds startup; browser lifetime is explicitly managed by Close/Stop.
func Start(ctx context.Context, configs ...Config) (_ *Browser, err error) {
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	var cfg Config
	if len(configs) > 1 {
		return nil, fmt.Errorf("dgodriver: expected at most one Config")
	}
	if len(configs) == 1 {
		cfg = configs[0]
	}
	cfg.BrowserArgs = append([]string(nil), cfg.BrowserArgs...)
	cfg.Extensions = append([]string(nil), cfg.Extensions...)
	if cfg.StartupTimeout <= 0 {
		cfg.StartupTimeout = 15 * time.Second
	}
	if cfg.HTTPTimeout <= 0 {
		cfg.HTTPTimeout = 10 * time.Second
	}
	startup, cancel := context.WithTimeout(ctx, cfg.StartupTimeout)
	defer cancel()
	b := &Browser{Config: cfg, targets: map[string]*Tab{}, contexts: map[string]*BrowserContext{}, downloadPaths: map[string]string{}, stopped: make(chan struct{}), client: &http.Client{Timeout: cfg.HTTPTimeout}}
	defer func() {
		if err != nil {
			_ = b.Close()
		}
	}()
	attach := cfg.WebSocketURL != "" || (cfg.Host != "" && cfg.Port > 0)
	if !attach {
		if b.Config.Host == "" {
			b.Config.Host = "127.0.0.1"
		}
		if b.Config.Port == 0 {
			// Use a concrete port as zendriver does; Chromium treats the literal
			// --remote-debugging-port=0 as an automation signal.
			listener, e := net.Listen("tcp", net.JoinHostPort(b.Config.Host, "0"))
			if e != nil {
				return nil, e
			}
			b.Config.Port = listener.Addr().(*net.TCPAddr).Port
			_ = listener.Close()
		}
		if cfg.BrowserExecutablePath == "" {
			b.Config.BrowserExecutablePath, err = FindExecutable(cfg.Browser)
			if err != nil {
				return nil, err
			}
		}
		if cfg.UserDataDir == "" {
			b.Config.UserDataDir, err = os.MkdirTemp("", "dgodriver-profile-")
			if err != nil {
				return nil, err
			}
			b.ownedProfile = true
		} else {
			b.Config.UserDataDir, err = filepath.Abs(cfg.UserDataDir)
			if err != nil {
				return nil, err
			}
			if err = os.MkdirAll(b.Config.UserDataDir, 0700); err != nil {
				return nil, err
			}
		}
		for i, p := range cfg.Extensions {
			var dir string
			dir, err = unpackExtension(p)
			if err != nil {
				return nil, err
			}
			b.Config.Extensions[i] = dir
			if dir != p {
				b.extensionDirs = append(b.extensionDirs, dir)
			}
		}
		args := append(b.Config.Arguments(), "about:blank")
		b.process = exec.Command(b.Config.BrowserExecutablePath, args...)
		configureProcess(b.process)
		b.process.Stderr = &b.stderr
		if err = b.process.Start(); err != nil {
			b.process = nil
			return nil, err
		}
		b.processDone = make(chan struct{})
		go func() { _ = b.process.Wait(); close(b.processDone) }()
	}
	websocketURL := cfg.WebSocketURL
	for websocketURL == "" {
		if b.process != nil {
			select {
			case <-b.processDone:
				return nil, fmt.Errorf("dgodriver: browser exited: %s", b.stderr.String())
			default:
			}
		}
		if b.Config.Port == 0 && b.Config.UserDataDir != "" {
			if data, e := os.ReadFile(filepath.Join(b.Config.UserDataDir, "DevToolsActivePort")); e == nil {
				lines := strings.Split(strings.TrimSpace(string(data)), "\n")
				if len(lines) > 0 {
					b.Config.Port, _ = strconv.Atoi(strings.TrimSpace(lines[0]))
				}
			}
		}
		if b.Config.Port > 0 {
			if e := b.HTTPGet(startup, "version", &b.Info); e == nil {
				websocketURL = b.Info.WebSocketDebuggerURL
			}
		}
		if websocketURL == "" {
			if e := Sleep(startup, 50*time.Millisecond); e != nil {
				return nil, fmt.Errorf("dgodriver: browser startup: %w; %s", e, b.stderr.String())
			}
		}
	}
	b.Info.WebSocketDebuggerURL = websocketURL
	b.Connection, err = Dial(startup, websocketURL)
	if err != nil {
		return nil, err
	}
	b.Connection.AddHandler(context.Background(), "Target.targetCreated", b.handleTarget)
	b.Connection.AddHandler(context.Background(), "Target.targetInfoChanged", b.handleTarget)
	b.Connection.AddHandler(context.Background(), "Target.targetDestroyed", b.handleTarget)
	if err = b.Send(startup, "Target.setDiscoverTargets", map[string]any{"discover": true}, nil); err != nil {
		return nil, err
	}
	if err = b.UpdateTargets(startup); err != nil {
		return nil, err
	}
	return b, nil
}
func Connect(ctx context.Context, websocketURL string) (*Browser, error) {
	return Start(ctx, Config{WebSocketURL: websocketURL})
}
func (b *Browser) WebSocketURL() string { return b.Info.WebSocketDebuggerURL }
func (b *Browser) HTTPGet(ctx context.Context, endpoint string, out any) error {
	address := net.JoinHostPort(b.Config.Host, strconv.Itoa(b.Config.Port))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/json/"+strings.TrimPrefix(endpoint, "/"), nil)
	if err != nil {
		return err
	}
	response, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, response.Body)
		return fmt.Errorf("dgodriver: discovery HTTP %d", response.StatusCode)
	}
	return json.NewDecoder(response.Body).Decode(out)
}
func (b *Browser) handleTarget(e Event) {
	if e.Method == "Target.targetDestroyed" {
		var p cdp.TargetTargetDestroyed
		if e.Decode(&p) != nil {
			return
		}
		id := string(p.TargetID)
		b.mu.Lock()
		tab := b.targets[id]
		delete(b.targets, id)
		b.mu.Unlock()
		if tab != nil {
			tab.mu.Lock()
			conn := tab.conn
			tab.destroyed = true
			tab.mu.Unlock()
			if conn != nil {
				conn.finish()
			}
		}
		return
	}
	var p struct {
		TargetInfo cdp.TargetTargetInfo `json:"targetInfo"`
	}
	if e.Decode(&p) == nil {
		b.upsert(p.TargetInfo)
	}
}
func (b *Browser) upsert(info cdp.TargetTargetInfo) *Tab {
	id := string(info.TargetID)
	b.mu.Lock()
	defer b.mu.Unlock()
	if t := b.targets[id]; t != nil {
		t.mu.Lock()
		t.target = info
		t.mu.Unlock()
		return t
	}
	t := &Tab{browser: b, target: info}
	b.targets[id] = t
	b.order = append(b.order, id)
	return t
}
func (b *Browser) UpdateTargets(ctx context.Context) error {
	result, err := cdp.Target.GetTargets(cdp.TargetGetTargetsParams{}).Execute(ctx, b)
	if err != nil {
		return err
	}
	for _, info := range result.TargetInfos {
		b.upsert(info)
	}
	return nil
}
func (b *Browser) Targets() []*Tab {
	b.mu.Lock()
	defer b.mu.Unlock()
	list := make([]*Tab, 0, len(b.targets))
	seen := map[string]bool{}
	for _, id := range b.order {
		if t := b.targets[id]; t != nil && !seen[id] {
			list = append(list, t)
			seen[id] = true
		}
	}
	return list
}
func (b *Browser) Tabs() []*Tab {
	var list []*Tab
	for _, t := range b.Targets() {
		if t.Target().Type == "page" {
			list = append(list, t)
		}
	}
	return list
}
func (b *Browser) MainTab() *Tab {
	tabs := b.Tabs()
	if len(tabs) == 0 {
		return nil
	}
	return tabs[0]
}
func (b *Browser) GetTab(ctx context.Context, text string) (*Tab, error) {
	if err := b.UpdateTargets(ctx); err != nil {
		return nil, err
	}
	text = strings.ToLower(text)
	for _, t := range b.Tabs() {
		info := t.Target()
		if strings.Contains(strings.ToLower(info.URL), text) || strings.Contains(strings.ToLower(info.Title), text) {
			return t, nil
		}
	}
	return nil, ErrNotFound
}
func (b *Browser) Tab(ctx context.Context, id string) (*Tab, error) {
	b.mu.Lock()
	t := b.targets[id]
	b.mu.Unlock()
	if t != nil {
		return t, nil
	}
	result, err := cdp.Target.GetTargetInfo(cdp.TargetGetTargetInfoParams{TargetID: cdp.Ptr(cdp.TargetTargetID(id))}).Execute(ctx, b)
	if err != nil {
		return nil, err
	}
	return b.upsert(result.TargetInfo), nil
}

type NavigationOptions struct{ NewTab, NewWindow bool }

func (b *Browser) Get(ctx context.Context, url string, options ...NavigationOptions) (*Tab, error) {
	if url == "" {
		url = "about:blank"
	}
	var opt NavigationOptions
	if len(options) > 0 {
		opt = options[0]
	}
	t := b.MainTab()
	if opt.NewTab || opt.NewWindow || t == nil {
		return b.NewTab(ctx, url, opt.NewWindow)
	}
	if err := t.Navigate(ctx, url); err != nil {
		return nil, err
	}
	return t, nil
}
func (b *Browser) NewTab(ctx context.Context, url string, newWindow ...bool) (*Tab, error) {
	p := cdp.TargetCreateTargetParams{URL: url}
	if p.URL == "" {
		p.URL = "about:blank"
	}
	if len(newWindow) > 0 {
		p.NewWindow = cdp.Ptr(newWindow[0])
	}
	result, err := cdp.Target.CreateTarget(p).Execute(ctx, b)
	if err != nil {
		return nil, err
	}
	t, err := b.Tab(ctx, string(result.TargetID))
	if err != nil {
		return nil, err
	}
	if err = t.WaitForReadyState(ctx, "interactive"); err != nil {
		return nil, err
	}
	return t, nil
}
func (b *Browser) Stopped() bool {
	select {
	case <-b.stopped:
		return true
	default:
	}
	if b.Connection != nil && b.Connection.Closed() {
		return true
	}
	if b.processDone != nil {
		select {
		case <-b.processDone:
			return true
		default:
		}
	}
	return false
}
func (b *Browser) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	return b.Stop(ctx)
}
func (b *Browser) Stop(ctx context.Context) error {
	b.stopOnce.Do(func() {
		defer close(b.stopped)
		for _, bc := range b.Contexts() {
			if err := bc.Dispose(ctx); err != nil && b.stopErr == nil {
				b.stopErr = err
			}
		}
		if b.process != nil && b.Connection != nil {
			_ = b.Send(ctx, "Browser.close", nil, nil)
		}
		if b.Connection != nil {
			_ = b.Connection.Close()
		}
		if b.process != nil {
			select {
			case <-b.processDone:
			case <-ctx.Done():
				_ = b.process.Process.Kill()
			case <-time.After(3 * time.Second):
				_ = b.process.Process.Kill()
			}
			select {
			case <-b.processDone:
			case <-time.After(3 * time.Second):
				b.stopErr = fmt.Errorf("dgodriver: browser did not exit")
				return
			}
		}
		if b.ownedProfile {
			if err := os.RemoveAll(b.Config.UserDataDir); err != nil && b.stopErr == nil {
				b.stopErr = err
			}
		}
		for _, p := range b.extensionDirs {
			if err := os.RemoveAll(p); err != nil && b.stopErr == nil {
				b.stopErr = err
			}
		}
		b.client.CloseIdleConnections()
	})
	return b.stopErr
}
func (b *Browser) GrantPermissions(ctx context.Context, permissions []cdp.BrowserPermissionType, origin string, contextID string) error {
	p := cdp.BrowserGrantPermissionsParams{Permissions: permissions}
	if origin != "" {
		p.Origin = &origin
	}
	if contextID != "" {
		p.BrowserContextID = cdp.Ptr(cdp.BrowserBrowserContextID(contextID))
	}
	_, err := cdp.Browser.GrantPermissions(p).Execute(ctx, b)
	return err
}
func (b *Browser) GrantAllPermissions(ctx context.Context) error {
	names := []string{"audioCapture", "backgroundSync", "backgroundFetch", "clipboardReadWrite", "clipboardSanitizedWrite", "displayCapture", "durableStorage", "geolocation", "idleDetection", "localFonts", "midi", "midiSysex", "nfc", "notifications", "paymentHandler", "periodicBackgroundSync", "protectedMediaIdentifier", "sensors", "storageAccess", "topLevelStorageAccess", "videoCapture", "videoCapturePanTiltZoom", "wakeLockScreen", "wakeLockSystem", "windowManagement"}
	permissions := make([]cdp.BrowserPermissionType, len(names))
	for i, n := range names {
		permissions[i] = cdp.BrowserPermissionType(n)
	}
	return b.GrantPermissions(ctx, permissions, "", "")
}

// TileWindows uses explicit screen bounds so it also works on remote/headless hosts.
func (b *Browser) TileWindows(ctx context.Context, width, height, columns int) ([]WindowBounds, error) {
	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("dgodriver: screen dimensions must be positive")
	}
	windows := map[int]*Tab{}
	for _, t := range b.Tabs() {
		id, _, err := t.GetWindow(ctx)
		if err != nil {
			return nil, err
		}
		windows[id] = t
	}
	ids := make([]int, 0, len(windows))
	for id := range windows {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	if len(ids) == 0 {
		return nil, nil
	}
	if columns <= 0 {
		columns = 1
		for columns*columns < len(ids) {
			columns++
		}
	}
	rows := (len(ids) + columns - 1) / columns
	var result []WindowBounds
	for i, id := range ids {
		r := WindowBounds{Left: (i % columns) * width / columns, Top: (i / columns) * height / rows, Width: width / columns, Height: height / rows, State: "normal"}
		if err := windows[id].SetWindowSize(ctx, r.Left, r.Top, r.Width, r.Height); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, nil
}
