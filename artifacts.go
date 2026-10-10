package dgodriver

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aijiesd520/dgodriver/cdp"
)

type ScreenshotOptions struct {
	Format   string
	FullPage bool
	Quality  *int
	Scale    float64
}

func screenshotParams(options []ScreenshotOptions) (cdp.PageCaptureScreenshotParams, ScreenshotOptions, error) {
	opt := ScreenshotOptions{Format: "png", Scale: 1}
	if len(options) > 0 {
		opt = options[0]
		if opt.Format == "" {
			opt.Format = "png"
		}
		if opt.Scale == 0 {
			opt.Scale = 1
		}
	}
	if opt.Format != "png" && opt.Format != "jpeg" && opt.Format != "webp" {
		return cdp.PageCaptureScreenshotParams{}, opt, fmt.Errorf("dgodriver: invalid screenshot format")
	}
	if opt.Scale <= 0 {
		return cdp.PageCaptureScreenshotParams{}, opt, fmt.Errorf("dgodriver: screenshot scale must be positive")
	}
	if opt.Quality != nil && (*opt.Quality < 0 || *opt.Quality > 100) {
		return cdp.PageCaptureScreenshotParams{}, opt, fmt.Errorf("dgodriver: screenshot quality must be 0..100")
	}
	return cdp.PageCaptureScreenshotParams{Format: &opt.Format, Quality: opt.Quality, FromSurface: cdp.Ptr(true), CaptureBeyondViewport: cdp.Ptr(opt.FullPage)}, opt, nil
}
func (t *Tab) ScreenshotBase64(ctx context.Context, options ...ScreenshotOptions) (string, error) {
	p, opt, err := screenshotParams(options)
	if err != nil {
		return "", err
	}
	if opt.FullPage {
		r, err := cdp.Page.GetLayoutMetrics().Execute(ctx, t)
		if err != nil {
			return "", err
		}
		p.Clip = &cdp.PageViewport{X: r.CSSContentSize.X, Y: r.CSSContentSize.Y, Width: r.CSSContentSize.Width, Height: r.CSSContentSize.Height, Scale: opt.Scale}
	}
	r, err := cdp.Page.CaptureScreenshot(p).Execute(ctx, t)
	return r.Data, err
}
func (t *Tab) Screenshot(ctx context.Context, options ...ScreenshotOptions) ([]byte, error) {
	s, err := t.ScreenshotBase64(ctx, options...)
	if err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(s)
}
func (t *Tab) SaveScreenshot(ctx context.Context, path string, options ...ScreenshotOptions) error {
	data, err := t.Screenshot(ctx, options...)
	if err != nil {
		return err
	}
	if path == "" || path == "auto" {
		_, opt, _ := screenshotParams(options)
		path = "screenshot-" + time.Now().Format("20060102-150405.000") + "." + opt.Format
	}
	return writeAtomic(path, data, 0644)
}
func (e *Element) ScreenshotBase64(ctx context.Context, options ...ScreenshotOptions) (string, error) {
	if err := e.ScrollIntoView(ctx); err != nil {
		return "", err
	}
	p, opt, err := screenshotParams(options)
	if err != nil {
		return "", err
	}
	position, err := e.GetPosition(ctx, true)
	if err != nil {
		return "", err
	}
	if position.Width <= 0 || position.Height <= 0 {
		return "", fmt.Errorf("dgodriver: element has no visible box")
	}
	clip := position.ToViewport(opt.Scale)
	p.Clip = &clip
	p.CaptureBeyondViewport = cdp.Ptr(true)
	r, err := cdp.Page.CaptureScreenshot(p).Execute(ctx, e.tab)
	return r.Data, err
}
func (e *Element) Screenshot(ctx context.Context, options ...ScreenshotOptions) ([]byte, error) {
	s, err := e.ScreenshotBase64(ctx, options...)
	if err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(s)
}
func (e *Element) SaveScreenshot(ctx context.Context, path string, options ...ScreenshotOptions) error {
	data, err := e.Screenshot(ctx, options...)
	if err != nil {
		return err
	}
	if path == "" || path == "auto" {
		_, opt, _ := screenshotParams(options)
		path = "element-" + time.Now().Format("20060102-150405.000") + "." + opt.Format
	}
	return writeAtomic(path, data, 0644)
}
func (t *Tab) SaveSnapshot(ctx context.Context, path string) error {
	r, err := cdp.Page.CaptureSnapshot(cdp.PageCaptureSnapshotParams{Format: cdp.Ptr("mhtml")}).Execute(ctx, t)
	if err != nil {
		return err
	}
	if path == "" {
		path = "snapshot.mhtml"
	}
	return writeAtomic(path, []byte(r.Data), 0644)
}
func (t *Tab) PrintToPDF(ctx context.Context, path string, options ...cdp.PagePrintToPDFParams) error {
	p := cdp.PagePrintToPDFParams{PrintBackground: cdp.Ptr(true)}
	if len(options) > 0 {
		p = options[0]
	}
	r, err := cdp.Page.PrintToPDF(p).Execute(ctx, t)
	if err != nil {
		return err
	}
	var data []byte
	if r.Stream != nil {
		defer func() {
			cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, _ = cdp.IO.Close(cdp.IOCloseParams{Handle: *r.Stream}).Execute(cleanup, t)
		}()
		for {
			part, err := cdp.IO.Read(cdp.IOReadParams{Handle: *r.Stream}).Execute(ctx, t)
			if err != nil {
				return err
			}
			chunk := []byte(part.Data)
			if part.Base64Encoded != nil && *part.Base64Encoded {
				chunk, err = base64.StdEncoding.DecodeString(part.Data)
				if err != nil {
					return err
				}
			}
			data = append(data, chunk...)
			if part.Eof {
				break
			}
		}
	} else {
		data, err = base64.StdEncoding.DecodeString(r.Data)
		if err != nil {
			return err
		}
	}
	return writeAtomic(path, data, 0644)
}
func (t *Tab) GetAllLinkedSources(ctx context.Context) ([]*Element, error) {
	return t.QuerySelectorAll(ctx, "a,link,img,script,meta,video,audio,source")
}
func (t *Tab) GetAllURLs(ctx context.Context, absolute ...bool) ([]string, error) {
	expr := "Array.from(document.querySelectorAll('a,link,img,script,meta,video,audio,source')).flatMap(el=>['src','href'].filter(k=>el.hasAttribute(k)).map(k=>new URL(el.getAttribute(k),document.baseURI).href))"
	if len(absolute) > 0 && !absolute[0] {
		expr = "Array.from(document.querySelectorAll('a,link,img,script,meta,video,audio,source')).flatMap(el=>['src','href'].filter(k=>el.hasAttribute(k)).map(k=>el.getAttribute(k)))"
	}
	var result []string
	err := t.Evaluate(ctx, expr, &result)
	return result, err
}

type DownloadInfo struct {
	GUID, URL, SuggestedFilename, Path string
	ReceivedBytes, TotalBytes          float64
}
type DownloadExpectation struct {
	tab               *Tab
	sub               *Subscription
	ready             chan struct{}
	done              chan struct{}
	mu                sync.Mutex
	info              DownloadInfo
	started, finished bool
	err               error
}

func (t *Tab) SetDownloadPath(ctx context.Context, path string) error {
	t.browser.downloadMu.Lock()
	defer t.browser.downloadMu.Unlock()
	return t.setDownloadPath(ctx, path)
}
func (t *Tab) contextID() string {
	if id := t.Target().BrowserContextID; id != nil {
		return string(*id)
	}
	return ""
}
func (t *Tab) setDownloadPath(ctx context.Context, path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(abs, 0755); err != nil {
		return err
	}
	p := cdp.BrowserSetDownloadBehaviorParams{Behavior: "allow", DownloadPath: &abs, EventsEnabled: cdp.Ptr(true)}
	if id := t.Target().BrowserContextID; id != nil {
		p.BrowserContextID = id
	}
	_, err = cdp.Browser.SetDownloadBehavior(p).Execute(ctx, t.browser)
	if err == nil {
		t.browser.mu.Lock()
		t.browser.downloadPaths[t.contextID()] = abs
		t.browser.mu.Unlock()
	}
	return err
}
func (t *Tab) ExpectDownload(ctx context.Context) (*DownloadExpectation, error) {
	tree, err := cdp.Page.GetFrameTree().Execute(ctx, t)
	if err != nil {
		return nil, err
	}
	frames := map[cdp.PageFrameID]bool{}
	var visit func(cdp.PageFrameTree)
	visit = func(f cdp.PageFrameTree) {
		frames[f.Frame.ID] = true
		for _, child := range f.ChildFrames {
			visit(child)
		}
	}
	visit(tree.FrameTree)
	e := &DownloadExpectation{tab: t, ready: make(chan struct{}), done: make(chan struct{})}
	e.sub = t.browser.AddHandler(ctx, "Browser.*", func(event Event) {
		e.mu.Lock()
		defer e.mu.Unlock()
		switch event.Method {
		case "Browser.downloadWillBegin":
			var v cdp.BrowserDownloadWillBegin
			if event.Decode(&v) != nil || !frames[v.FrameID] || e.started {
				return
			}
			e.started = true
			e.info.GUID = v.GUID
			e.info.URL = v.URL
			e.info.SuggestedFilename = v.SuggestedFilename
			t.browser.mu.Lock()
			dir := t.browser.downloadPaths[t.contextID()]
			t.browser.mu.Unlock()
			if dir != "" {
				e.info.Path = filepath.Join(dir, filepath.Base(v.SuggestedFilename))
			}
			close(e.ready)
		case "Browser.downloadProgress":
			var v cdp.BrowserDownloadProgress
			if event.Decode(&v) != nil || !e.started || e.info.GUID != v.GUID || e.finished {
				return
			}
			e.info.ReceivedBytes = v.ReceivedBytes
			e.info.TotalBytes = v.TotalBytes
			if v.FilePath != nil {
				e.info.Path = *v.FilePath
			}
			if v.State == "completed" || v.State == "canceled" {
				if v.State == "canceled" {
					e.err = fmt.Errorf("dgodriver: download canceled")
				}
				e.finished = true
				close(e.done)
			}
		}
	})
	p := cdp.BrowserSetDownloadBehaviorParams{Behavior: "default", EventsEnabled: cdp.Ptr(true), BrowserContextID: t.Target().BrowserContextID}
	t.browser.mu.Lock()
	path := t.browser.downloadPaths[t.contextID()]
	t.browser.mu.Unlock()
	if path != "" {
		p.Behavior = "allow"
		p.DownloadPath = &path
	}
	if _, err = cdp.Browser.SetDownloadBehavior(p).Execute(ctx, t.browser); err != nil {
		e.sub.Close()
		return nil, err
	}
	return e, nil
}
func (e *DownloadExpectation) Close() error { return e.sub.Close() }
func (e *DownloadExpectation) WaitBegin(ctx context.Context) (DownloadInfo, error) {
	select {
	case <-ctx.Done():
		return DownloadInfo{}, ctx.Err()
	case <-e.tab.browser.Done():
		return DownloadInfo{}, ErrClosed
	case <-e.sub.done:
		return DownloadInfo{}, ErrClosed
	case <-e.ready:
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.info, nil
	}
}
func (e *DownloadExpectation) Wait(ctx context.Context) (DownloadInfo, error) {
	select {
	case <-ctx.Done():
		return DownloadInfo{}, ctx.Err()
	case <-e.tab.browser.Done():
		return DownloadInfo{}, ErrClosed
	case <-e.sub.done:
		return DownloadInfo{}, ErrClosed
	case <-e.done:
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.info, e.err
	}
}

// DownloadFile fetches within the page, preserving browser cookies and proxy.
// The browser's same-origin/CORS rules apply to the requested URL.
func (t *Tab) DownloadFile(ctx context.Context, address, path string) error {
	t.browser.downloadMu.Lock()
	defer t.browser.downloadMu.Unlock()
	if path == "" {
		path = "download"
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp(filepath.Dir(abs), ".dgodriver-download-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	t.browser.mu.Lock()
	previous := t.browser.downloadPaths[t.contextID()]
	t.browser.mu.Unlock()
	if err = t.setDownloadPath(ctx, dir); err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if previous != "" {
			_ = t.setDownloadPath(cleanup, previous)
		} else {
			p := cdp.BrowserSetDownloadBehaviorParams{Behavior: "default", EventsEnabled: cdp.Ptr(true), BrowserContextID: t.Target().BrowserContextID}
			_, _ = cdp.Browser.SetDownloadBehavior(p).Execute(cleanup, t.browser)
			t.browser.mu.Lock()
			delete(t.browser.downloadPaths, t.contextID())
			t.browser.mu.Unlock()
		}
	}()
	expectation, err := t.ExpectDownload(ctx)
	if err != nil {
		return err
	}
	defer expectation.Close()
	code := "(async()=>{const response=await fetch(" + jsString(address) + ",{credentials:'include'});if(!response.ok)throw new Error('download HTTP '+response.status);const blob=await response.blob();const u=URL.createObjectURL(blob);const a=document.createElement('a');a.href=u;a.download=" + jsString(filepath.Base(abs)) + ";document.body.appendChild(a);a.click();a.remove();setTimeout(()=>URL.revokeObjectURL(u),30000)})()"
	if err = t.Evaluate(ctx, code, nil, EvaluateOptions{AwaitPromise: true}); err != nil {
		return err
	}
	info, err := expectation.Wait(ctx)
	if err != nil {
		return err
	}
	if info.Path == "" {
		info.Path = filepath.Join(dir, filepath.Base(abs))
	}
	return os.Rename(info.Path, abs)
}

// VideoRecording uses the browser's MediaRecorder. The resulting file is WebM.
// User activation and Chromium capture support are required, as in zendriver.
type VideoRecording struct {
	element *Element
	path    string
	once    sync.Once
	err     error
}

func (e *Element) RecordVideo(ctx context.Context, path string) (*VideoRecording, error) {
	if !strings.HasSuffix(strings.ToLower(path), ".webm") {
		return nil, fmt.Errorf("dgodriver: recording path must end in .webm")
	}
	if err := e.tab.BringToFront(ctx); err != nil {
		return nil, err
	}
	if err := e.ScrollIntoView(ctx); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.recording != nil {
		return nil, fmt.Errorf("dgodriver: element is already recording")
	}
	// Resolve directly while holding the recording lock, without calling Node().
	n := e.node
	r, err := cdp.DOM.ResolveNode(cdp.DOMResolveNodeParams{BackendNodeID: &n.BackendNodeID}).Execute(ctx, e.tab)
	if err != nil {
		return nil, err
	}
	if r.Object.ObjectID == nil {
		return nil, fmt.Errorf("dgodriver: node has no remote object")
	}
	id := *r.Object.ObjectID
	defer releaseObject(e.tab, id)
	code := `async function(){const el=this;const stream=el.captureStream?el.captureStream():await navigator.mediaDevices.getDisplayMedia({video:true,audio:false});const chunks=[];const recorder=new MediaRecorder(stream,{mimeType:'video/webm'});const done=new Promise(resolve=>{recorder.ondataavailable=e=>{if(e.data.size)chunks.push(e.data)};recorder.onstop=resolve});el.__dgodriverRecording={recorder,stream,chunks,done};recorder.start(100);stream.getVideoTracks().forEach(t=>{if(t.requestFrame)t.requestFrame()})}`
	value, err := cdp.Runtime.CallFunctionOn(cdp.RuntimeCallFunctionOnParams{ObjectID: &id, FunctionDeclaration: code, AwaitPromise: cdp.Ptr(true), UserGesture: cdp.Ptr(true)}).Execute(ctx, e.tab)
	if err != nil {
		return nil, err
	}
	if value.ExceptionDetails != nil {
		return nil, &JavaScriptError{*value.ExceptionDetails}
	}
	recording := &VideoRecording{element: e, path: path}
	e.recording = recording
	return recording, nil
}
func (e *Element) IsRecording() bool { e.mu.Lock(); defer e.mu.Unlock(); return e.recording != nil }
func (v *VideoRecording) Stop(ctx context.Context) error {
	v.once.Do(func() {
		var encoded string
		v.err = v.element.Apply(ctx, `async el=>{const r=el.__dgodriverRecording;if(!r)throw new Error('recording not found');r.recorder.stop();await r.done;r.stream.getTracks().forEach(t=>t.stop());const blob=new Blob(r.chunks,{type:'video/webm'});delete el.__dgodriverRecording;if(!blob.size)throw new Error('no video frames captured');return await new Promise((resolve,reject)=>{const reader=new FileReader();reader.onload=()=>resolve(reader.result.split(',')[1]);reader.onerror=reject;reader.readAsDataURL(blob)})}`, &encoded, EvaluateOptions{AwaitPromise: true})
		if v.err == nil {
			var data []byte
			data, v.err = base64.StdEncoding.DecodeString(encoded)
			if v.err == nil {
				v.err = writeAtomic(v.path, data, 0644)
			}
		}
		v.element.mu.Lock()
		v.element.recording = nil
		v.element.mu.Unlock()
	})
	return v.err
}
func (v *VideoRecording) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return v.Stop(ctx)
}
