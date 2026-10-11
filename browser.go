package dgodriver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aijiesd520/dgodriver/cdp"
)

// BrowserInfo describes the browser's /json/version discovery response.
type BrowserInfo struct {
	Browser              string `json:"Browser"`
	ProtocolVersion      string `json:"Protocol-Version"`
	UserAgent            string `json:"User-Agent"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// Browser manages a CDP connection, targets and any locally launched process.
// Use Close or Stop to release resources; canceling Start's context does not stop it.
type Browser struct {
	*Connection
	Config        Config
	Info          BrowserInfo
	mu            sync.Mutex
	targets       map[string]*Tab
	order         []string
	targetQueries map[*targetQuery]struct{}
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

// targetQuery records only targets destroyed while a CDP target query is in flight.
// Removing completed queries avoids retaining every closed target for the browser's lifetime.
type targetQuery struct{ destroyed map[string]struct{} }

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}
func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

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
	b := &Browser{
		Config:        cfg,
		targets:       make(map[string]*Tab),
		contexts:      make(map[string]*BrowserContext),
		downloadPaths: make(map[string]string),
		stopped:       make(chan struct{}),
		client:        &http.Client{Timeout: cfg.HTTPTimeout},
	}
	defer func() {
		if err != nil {
			_ = b.Close()
		}
	}()
	attach := cfg.WebSocketURL != "" || (cfg.Host != "" && cfg.Port > 0)
	if !attach {
		if runtime.GOOS == "linux" && os.Geteuid() == 0 && !cfg.NoSandbox {
			return nil, fmt.Errorf("dgodriver: Chromium cannot run as root on Linux with the sandbox enabled; run as a non-root user or explicitly set Config.NoSandbox = true in a trusted environment")
		}
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
	// One subscription preserves wire order across creation, updates and destruction.
	b.Connection.AddHandler(context.Background(), "Target.*", b.handleTarget)
	if err = b.Send(startup, "Target.setDiscoverTargets", map[string]any{"discover": true}, nil); err != nil {
		return nil, err
	}
	if err = b.UpdateTargets(startup); err != nil {
		return nil, err
	}
	return b, nil
}

// Connect attaches to an existing browser without taking ownership of its process.
func Connect(ctx context.Context, websocketURL string) (*Browser, error) {
	return Start(ctx, Config{WebSocketURL: websocketURL})
}

// WebSocketURL returns the browser's CDP WebSocket endpoint.
func (b *Browser) WebSocketURL() string { return b.Info.WebSocketDebuggerURL }

// HTTPGet decodes a /json discovery response. It requires Config.Host and Port.
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
	switch e.Method {
	case "Target.targetDestroyed":
		var p cdp.TargetTargetDestroyed
		if e.Decode(&p) == nil && p.TargetID != "" {
			b.removeTarget(string(p.TargetID))
		}
	case "Target.targetCreated":
		var p cdp.TargetTargetCreated
		if e.Decode(&p) == nil {
			b.upsert(p.TargetInfo, nil)
		}
	case "Target.targetInfoChanged":
		var p cdp.TargetTargetInfoChanged
		if e.Decode(&p) != nil {
			return
		}
		b.mu.Lock()
		if tab := b.targets[string(p.TargetInfo.TargetID)]; tab != nil {
			tab.mu.Lock()
			tab.target = p.TargetInfo
			tab.mu.Unlock()
		}
		b.mu.Unlock()
	}
}

func (b *Browser) removeTarget(id string) {
	if id == "" {
		return
	}
	b.mu.Lock()
	for query := range b.targetQueries {
		query.destroyed[id] = struct{}{}
	}
	tab := b.targets[id]
	delete(b.targets, id)
	for i, targetID := range b.order {
		if targetID == id {
			copy(b.order[i:], b.order[i+1:])
			b.order[len(b.order)-1] = ""
			b.order = b.order[:len(b.order)-1]
			break
		}
	}
	var conn *Connection
	if tab != nil {
		tab.mu.Lock()
		tab.destroyed = true
		conn = tab.conn
		tab.mu.Unlock()
	}
	b.mu.Unlock()
	if conn != nil {
		conn.finish()
		conn.transport.removeSession(conn.session)
	}
}

func (b *Browser) beginTargetQuery() *targetQuery {
	b.mu.Lock()
	defer b.mu.Unlock()
	query := &targetQuery{destroyed: make(map[string]struct{})}
	if b.targetQueries == nil {
		b.targetQueries = make(map[*targetQuery]struct{})
	}
	b.targetQueries[query] = struct{}{}
	return query
}

func (b *Browser) endTargetQuery(query *targetQuery) {
	b.mu.Lock()
	delete(b.targetQueries, query)
	b.mu.Unlock()
}

func (b *Browser) upsert(info cdp.TargetTargetInfo, query *targetQuery) *Tab {
	id := string(info.TargetID)
	if id == "" {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if query != nil {
		if _, destroyed := query.destroyed[id]; destroyed {
			return nil
		}
	}
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

// UpdateTargets refreshes known targets from the browser's current target list.
func (b *Browser) UpdateTargets(ctx context.Context) error {
	query := b.beginTargetQuery()
	defer b.endTargetQuery(query)
	result, err := cdp.Target.GetTargets(cdp.TargetGetTargetsParams{}).Execute(ctx, b)
	if err != nil {
		return err
	}
	for _, info := range result.TargetInfos {
		b.upsert(info, query)
	}
	return nil
}

// Targets returns a snapshot of all known targets in discovery order.
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

// Tabs returns the known page targets, excluding workers and iframe targets.
func (b *Browser) Tabs() []*Tab {
	var list []*Tab
	for _, t := range b.Targets() {
		if t.Target().Type == "page" {
			list = append(list, t)
		}
	}
	return list
}

// MainTab returns the first page target, or nil when no page is open.
func (b *Browser) MainTab() *Tab {
	tabs := b.Tabs()
	if len(tabs) == 0 {
		return nil
	}
	return tabs[0]
}

// GetTab refreshes targets and finds a page by a case-insensitive title/URL substring.
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

// Tab returns the target with id, querying CDP if it is not already known.
func (b *Browser) Tab(ctx context.Context, id string) (*Tab, error) {
	b.mu.Lock()
	t := b.targets[id]
	b.mu.Unlock()
	if t != nil {
		return t, nil
	}
	query := b.beginTargetQuery()
	defer b.endTargetQuery(query)
	result, err := cdp.Target.GetTargetInfo(cdp.TargetGetTargetInfoParams{TargetID: cdp.Ptr(cdp.TargetTargetID(id))}).Execute(ctx, b)
	if err != nil {
		return nil, err
	}
	if tab := b.upsert(result.TargetInfo, query); tab != nil {
		return tab, nil
	}
	return nil, ErrClosed
}

// NavigationOptions selects a new tab or window instead of reusing the main tab.
type NavigationOptions struct {
	NewTab    bool
	NewWindow bool
}

// Get navigates the main tab, creating a tab if needed or requested by options.
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

// NewTab opens url and waits for an interactive document. Empty url opens about:blank.
// Set newWindow to true to open a separate window.
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

// Stopped reports whether shutdown, connection loss or process exit has occurred.
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

// Close calls Stop with an independent eight-second context.
func (b *Browser) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	return b.Stop(ctx)
}

// Stop disposes managed contexts and closes CDP. For a launched browser it also
// requests graceful exit, then forces process-tree termination if needed.
// Cleanup still runs with a canceled context; only library-owned directories are
// removed. Repeated calls return the first shutdown's result.
func (b *Browser) Stop(ctx context.Context) error {
	b.stopOnce.Do(func() {
		defer close(b.stopped)
		for _, bc := range b.Contexts() {
			if err := bc.Dispose(ctx); err != nil && b.stopErr == nil {
				b.stopErr = err
			}
		}
		if b.process != nil && b.Connection != nil {
			closeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			_ = b.Send(closeCtx, "Browser.close", nil, nil)
			cancel()
		}
		if b.Connection != nil {
			_ = b.Connection.Close()
		}
		if b.process != nil {
			select {
			case <-b.processDone:
			case <-ctx.Done():
				b.stopErr = errors.Join(b.stopErr, killProcessTree(b.process))
			case <-time.After(3 * time.Second):
				b.stopErr = errors.Join(b.stopErr, killProcessTree(b.process))
			}
			select {
			case <-b.processDone:
			case <-time.After(3 * time.Second):
				b.stopErr = errors.Join(b.stopErr, fmt.Errorf("dgodriver: browser did not exit"))
			}
		}
		if b.ownedProfile {
			b.stopErr = errors.Join(b.stopErr, removePathWithRetry(b.Config.UserDataDir))
		}
		for _, p := range b.extensionDirs {
			b.stopErr = errors.Join(b.stopErr, removePathWithRetry(p))
		}
		b.client.CloseIdleConnections()
	})
	return b.stopErr
}

// Windows may retain file handles briefly after the browser has exited.
// Cleanup uses its own bounded retry window even if Stop's context is cancelled.
func removePathWithRetry(path string) error {
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := os.RemoveAll(path)
		if err == nil {
			return nil
		}
		if runtime.GOOS != "windows" || time.Now().After(deadline) {
			return fmt.Errorf("dgodriver: remove temporary directory %q: %w", path, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// GrantPermissions overrides permissions for an origin and browser context.
// Empty origin applies to all origins; empty contextID selects the default context.
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

// GrantAllPermissions grants the library's permission list to the default context.
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
