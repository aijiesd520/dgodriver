package dgodriver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/aijiesd520/dgodriver/cdp"
)

// Tab represents a CDP target, including pages, out-of-process frames and workers.
// Its connection is attached lazily. Closed targets cannot be reused.
type Tab struct {
	browser        *Browser
	mu             sync.Mutex
	target         cdp.TargetTargetInfo
	conn           *Connection
	destroyed      bool
	attachMu       sync.Mutex
	mouseMu        sync.Mutex
	mouseX, mouseY float64
	mouseButtons   int
	interceptMu    sync.Mutex
	interceptor    *Interceptor
}

// Browser returns the browser managing this target.
func (t *Tab) Browser() *Browser { return t.browser }

// Target returns the last known target metadata; UpdateTarget refreshes it.
func (t *Tab) Target() cdp.TargetTargetInfo {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.target
}

// TargetID returns the target's CDP identifier.
func (t *Tab) TargetID() string { return string(t.Target().TargetID) }

// URL returns the last known target URL.
func (t *Tab) URL() string { return t.Target().URL }

// Title returns the last known target title.
func (t *Tab) Title() string { return t.Target().Title }

// Connection attaches to the target when needed and returns its CDP session.
func (t *Tab) Connection(ctx context.Context) (*Connection, error) {
	t.attachMu.Lock()
	defer t.attachMu.Unlock()
	t.mu.Lock()
	conn := t.conn
	destroyed := t.destroyed
	t.mu.Unlock()
	if destroyed {
		return nil, ErrClosed
	}
	if conn != nil && !conn.Closed() {
		return conn, nil
	}
	result, err := cdp.Target.AttachToTarget(cdp.TargetAttachToTargetParams{TargetID: cdp.TargetTargetID(t.TargetID()), Flatten: cdp.Ptr(true)}).Execute(ctx, t.browser)
	if err != nil {
		return nil, err
	}
	conn = t.browser.transport.session(string(result.SessionID))
	t.mu.Lock()
	if t.destroyed {
		t.mu.Unlock()
		conn.finish()
		conn.transport.removeSession(conn.session)
		return nil, ErrClosed
	}
	t.conn = conn
	t.mu.Unlock()
	targetType := t.Target().Type
	if targetType != "page" && targetType != "iframe" {
		return conn, nil
	}
	if t.browser.Config.Headless {
		ua := t.browser.Config.UserAgent
		if ua == "" {
			value, err := cdp.Runtime.Evaluate(cdp.RuntimeEvaluateParams{Expression: "navigator.userAgent", ReturnByValue: cdp.Ptr(true)}).Execute(ctx, conn)
			if err != nil {
				return nil, err
			}
			if text, ok := value.Result.Value.(string); ok {
				ua = strings.ReplaceAll(text, "Headless", "")
			}
		}
		if ua != "" {
			if err = conn.Send(ctx, "Network.setUserAgentOverride", map[string]any{"userAgent": ua}, nil); err != nil {
				return nil, err
			}
		}
	}
	if t.browser.Config.Expert {
		if err = conn.Send(ctx, "Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": "(()=>{const original=Element.prototype.attachShadow;Element.prototype.attachShadow=function(init){return original.call(this,{...init,mode:'open'})}})()"}, nil); err != nil {
			return nil, err
		}
	}
	return conn, nil
}

// Send executes a CDP command in this target's session; see Connection.Send.
func (t *Tab) Send(ctx context.Context, method string, params, out any) error {
	c, err := t.Connection(ctx)
	if err != nil {
		return err
	}
	return c.Send(ctx, method, params, out)
}

// Call executes a target CDP command and returns its raw JSON result.
func (t *Tab) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	var result json.RawMessage
	err := t.Send(ctx, method, params, &result)
	return result, err
}

// On subscribes to target events, enabling supported domains. See Subscription
// for callback ordering, unbounded queue behavior and cleanup requirements.
func (t *Tab) On(ctx context.Context, method string, fn func(Event)) (*Subscription, error) {
	c, err := t.Connection(ctx)
	if err != nil {
		return nil, err
	}
	return c.On(ctx, method, fn)
}

// Enable enables a CDP domain in this target's session.
func (t *Tab) Enable(ctx context.Context, domain string) error {
	c, err := t.Connection(ctx)
	if err != nil {
		return err
	}
	return c.Enable(ctx, domain)
}

// DisableDOMAgent disables DOM notifications in this target's session.
func (t *Tab) DisableDOMAgent(ctx context.Context) error {
	c, err := t.Connection(ctx)
	if err != nil {
		return err
	}
	return c.Disable(ctx, "DOM")
}

// Wait waits for an idle interval in target CDP events (default 250 milliseconds).
func (t *Tab) Wait(ctx context.Context, idle ...time.Duration) error {
	c, err := t.Connection(ctx)
	if err != nil {
		return err
	}
	var d time.Duration
	if len(idle) > 0 {
		d = idle[0]
	}
	return c.Wait(ctx, d)
}

// Sleep waits for d or context cancellation.
func (t *Tab) Sleep(ctx context.Context, d time.Duration) error { return Sleep(ctx, d) }

// UpdateTarget refreshes this target's URL, title and other metadata from CDP.
func (t *Tab) UpdateTarget(ctx context.Context) error {
	r, err := cdp.Target.GetTargetInfo(cdp.TargetGetTargetInfoParams{TargetID: cdp.Ptr(cdp.TargetTargetID(t.TargetID()))}).Execute(ctx, t.browser)
	if err == nil {
		t.mu.Lock()
		t.target = r.TargetInfo
		t.mu.Unlock()
	}
	return err
}

// Navigate loads address and waits for DOMContentLoaded or load in the new document.
// Use a context deadline to bound the entire navigation, including event waits.
func (t *Tab) Navigate(ctx context.Context, address string) error {
	if err := t.Enable(ctx, "Page"); err != nil {
		return err
	}
	// Register before navigation. Matching loader IDs excludes a late load event
	// from the previous document; same-document navigation has no loader ID.
	events := make(chan cdp.PageLifecycleEvent, 64)
	lifetime, finish := context.WithCancel(ctx)
	defer finish()
	sub, err := t.On(lifetime, "Page.lifecycleEvent", func(e Event) {
		var value cdp.PageLifecycleEvent
		if e.Decode(&value) == nil {
			select {
			case events <- value:
			case <-lifetime.Done():
			}
		}
	})
	if err != nil {
		return err
	}
	defer sub.Close()
	if err = t.Send(ctx, "Page.setLifecycleEventsEnabled", map[string]any{"enabled": true}, nil); err != nil {
		return err
	}
	result, err := cdp.Page.Navigate(cdp.PageNavigateParams{URL: address}).Execute(ctx, t)
	if err != nil {
		return err
	}
	if result.ErrorText != nil && *result.ErrorText != "" {
		return fmt.Errorf("dgodriver: navigation: %s", *result.ErrorText)
	}
	if result.IsDownload != nil && *result.IsDownload {
		return nil
	}
	if result.LoaderID != nil {
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.browser.Done():
				return ErrClosed
			case <-sub.done:
				return ErrClosed
			case event := <-events:
				if event.FrameID == result.FrameID && event.LoaderID == *result.LoaderID && (event.Name == "DOMContentLoaded" || event.Name == "load") {
					return t.UpdateTarget(ctx)
				}
			}
		}
	}
	return t.WaitForReadyState(ctx, "interactive")
}

// Get navigates this tab, or creates a new tab/window when requested by options.
func (t *Tab) Get(ctx context.Context, address string, options ...NavigationOptions) (*Tab, error) {
	if len(options) > 0 && (options[0].NewTab || options[0].NewWindow) {
		return t.browser.NewTab(ctx, address, options[0].NewWindow)
	}
	return t, t.Navigate(ctx, address)
}

// Reload reloads the page and waits for completion; ignoreCache defaults to true.
func (t *Tab) Reload(ctx context.Context, ignoreCache ...bool) error {
	ignore := true
	if len(ignoreCache) > 0 {
		ignore = ignoreCache[0]
	}
	return t.waitForNavigationCommand(ctx, "complete", func() error {
		_, err := cdp.Page.Reload(cdp.PageReloadParams{IgnoreCache: &ignore}).Execute(ctx, t)
		return err
	})
}
func (t *Tab) history(ctx context.Context, delta int) error {
	r, err := cdp.Page.GetNavigationHistory().Execute(ctx, t)
	if err != nil {
		return err
	}
	index := r.CurrentIndex + delta
	if index < 0 || index >= len(r.Entries) {
		return fmt.Errorf("dgodriver: no navigation history entry")
	}
	return t.waitForNavigationCommand(ctx, "interactive", func() error {
		_, err := cdp.Page.NavigateToHistoryEntry(cdp.PageNavigateToHistoryEntryParams{EntryID: r.Entries[index].ID}).Execute(ctx, t)
		return err
	})
}
func (t *Tab) waitForNavigationCommand(ctx context.Context, state string, command func() error) error {
	root, err := cdp.Page.GetFrameTree().Execute(ctx, t)
	if err != nil {
		return err
	}
	lifetime, finish := context.WithCancel(ctx)
	defer finish()
	committed := make(chan struct{}, 1)
	sub, err := t.On(lifetime, "Page.*", func(e Event) {
		matched := false
		switch e.Method {
		case "Page.frameNavigated":
			var v cdp.PageFrameNavigated
			if e.Decode(&v) == nil {
				matched = v.Frame.ID == root.FrameTree.Frame.ID && v.Frame.LoaderID != root.FrameTree.Frame.LoaderID
			}
		case "Page.navigatedWithinDocument":
			var v cdp.PageNavigatedWithinDocument
			if e.Decode(&v) == nil {
				matched = v.FrameID == root.FrameTree.Frame.ID
			}
		}
		if matched {
			select {
			case committed <- struct{}{}:
			default:
			}
		}
	})
	if err != nil {
		return err
	}
	defer sub.Close()
	if err = command(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-sub.done:
		return ErrClosed
	case <-committed:
	}
	if err = t.WaitForReadyState(ctx, state); err != nil {
		return err
	}
	return t.UpdateTarget(ctx)
}

// Back navigates to the previous history entry.
func (t *Tab) Back(ctx context.Context) error { return t.history(ctx, -1) }

// Forward navigates to the next history entry.
func (t *Tab) Forward(ctx context.Context) error { return t.history(ctx, 1) }

// Close closes the target with an independent five-second context.
func (t *Tab) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return t.CloseContext(ctx)
}

// CloseContext closes the target and removes its local target/session state.
func (t *Tab) CloseContext(ctx context.Context) error {
	r, err := cdp.Target.CloseTarget(cdp.TargetCloseTargetParams{TargetID: cdp.TargetTargetID(t.TargetID())}).Execute(ctx, t.browser)
	if err != nil {
		return err
	}
	if !r.Success {
		return fmt.Errorf("dgodriver: target could not close")
	}
	t.browser.removeTarget(t.TargetID())
	return nil
}

// Activate activates this target using the browser's target API.
func (t *Tab) Activate(ctx context.Context) error {
	_, err := cdp.Target.ActivateTarget(cdp.TargetActivateTargetParams{TargetID: cdp.TargetTargetID(t.TargetID())}).Execute(ctx, t.browser)
	return err
}

// BringToFront brings the page to the foreground.
func (t *Tab) BringToFront(ctx context.Context) error {
	_, err := cdp.Page.BringToFront().Execute(ctx, t)
	return err
}

// JavaScriptError contains the exception raised by page JavaScript.
type JavaScriptError struct {
	Details cdp.RuntimeExceptionDetails
}

func (e *JavaScriptError) Error() string {
	if e.Details.Exception != nil && e.Details.Exception.Description != nil {
		return "dgodriver: JavaScript: " + *e.Details.Exception.Description
	}
	return "dgodriver: JavaScript: " + e.Details.Text
}

// EvaluateOptions controls promise waiting and the result returned by Evaluate or Apply.
type EvaluateOptions struct {
	AwaitPromise bool
	// RemoteObject returns CDP metadata into out instead of the JSON value.
	RemoteObject bool
	ContextID    *cdp.RuntimeExecutionContextID
}

// Evaluate runs JavaScript in the target and decodes the JSON result into out.
// out may be nil to discard the result. AwaitPromise must be set to await promises;
// RemoteObject expects out to receive a cdp.RuntimeRemoteObject.
func (t *Tab) Evaluate(ctx context.Context, expression string, out any, options ...EvaluateOptions) error {
	var opt EvaluateOptions
	if len(options) > 0 {
		opt = options[0]
	}
	r, err := cdp.Runtime.Evaluate(cdp.RuntimeEvaluateParams{Expression: expression, AwaitPromise: &opt.AwaitPromise, ReturnByValue: cdp.Ptr(!opt.RemoteObject), UserGesture: cdp.Ptr(true), ContextID: opt.ContextID}).Execute(ctx, t)
	if err != nil {
		return err
	}
	return decodeEvaluation(r.Result, r.ExceptionDetails, out, opt.RemoteObject)
}
func decodeEvaluation(object cdp.RuntimeRemoteObject, exception *cdp.RuntimeExceptionDetails, out any, remote bool) error {
	if exception != nil {
		return &JavaScriptError{*exception}
	}
	if out == nil {
		return nil
	}
	var value any = object.Value
	if remote {
		value = object
	} else if object.UnserializableValue != nil {
		value = string(*object.UnserializableValue)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

// JSDumps serializes an expression, stringifying big integers and marking cycles.
func (t *Tab) JSDumps(ctx context.Context, expression string) (json.RawMessage, error) {
	var out json.RawMessage
	err := t.Evaluate(ctx, "(()=>{const seen=new WeakSet();return JSON.parse(JSON.stringify(("+expression+"),(_,value)=>{if(typeof value==='bigint')return value.toString();if(value&&typeof value==='object'){if(seen.has(value))return '[Circular]';seen.add(value)}return value}))})()", &out)
	return out, err
}
func poll(ctx context.Context, fn func() (bool, error)) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ok, err := fn()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if err = Sleep(ctx, 50*time.Millisecond); err != nil {
			return err
		}
	}
}
func defaultTimeout(ctx context.Context, options []QueryOptions) (context.Context, context.CancelFunc, QueryOptions) {
	opt := QueryOptions{Timeout: 10 * time.Second}
	if len(options) > 0 {
		opt = options[0]
		if opt.Timeout == 0 {
			opt.Timeout = 10 * time.Second
		}
	}
	if opt.Timeout < 0 {
		return ctx, func() {}, opt
	}
	limited, cancel := context.WithTimeout(ctx, opt.Timeout)
	return limited, cancel, opt
}

// WaitForReadyState waits for loading, interactive or complete. Complete also
// satisfies interactive. Use a context deadline to bound polling.
func (t *Tab) WaitForReadyState(ctx context.Context, state string) error {
	if state != "loading" && state != "interactive" && state != "complete" {
		return fmt.Errorf("dgodriver: invalid ready state %q", state)
	}
	return poll(ctx, func() (bool, error) {
		var current string
		err := t.Evaluate(ctx, "document.readyState", &current)
		if err != nil {
			var p *ProtocolError
			if errors.As(err, &p) && (strings.Contains(p.Message, "context") || strings.Contains(p.Message, "frame")) {
				return false, nil
			}
			return false, err
		}
		return current == state || (state == "interactive" && current == "complete"), nil
	})
}

// GetContent returns the document element's outer HTML.
func (t *Tab) GetContent(ctx context.Context) (string, error) {
	var html string
	err := t.Evaluate(ctx, "document.documentElement.outerHTML", &html)
	return html, err
}

// GetLocalStorage reads localStorage for the current page's origin.
func (t *Tab) GetLocalStorage(ctx context.Context) (map[string]string, error) {
	var result map[string]string
	err := t.Evaluate(ctx, "(()=>{const out={};for(let i=0;i<localStorage.length;i++){const k=localStorage.key(i);Object.defineProperty(out,k,{value:localStorage.getItem(k),enumerable:true,configurable:true})}return out})()", &result)
	return result, err
}

// SetLocalStorage writes items into localStorage for the current page's origin.
func (t *Tab) SetLocalStorage(ctx context.Context, items map[string]string) error {
	raw, err := json.Marshal(items)
	if err != nil {
		return err
	}
	return t.Evaluate(ctx, "Object.entries("+string(raw)+").forEach(([k,v])=>localStorage.setItem(k,v))", nil)
}

// SetUserAgent overrides the target's user agent, optionally Accept-Language and platform.
func (t *Tab) SetUserAgent(ctx context.Context, userAgent string, languagePlatform ...string) error {
	p := cdp.NetworkSetUserAgentOverrideParams{UserAgent: userAgent}
	if len(languagePlatform) > 0 {
		p.AcceptLanguage = &languagePlatform[0]
	}
	if len(languagePlatform) > 1 {
		p.Platform = &languagePlatform[1]
	}
	_, err := cdp.Network.SetUserAgentOverride(p).Execute(ctx, t)
	return err
}

// QueryOptions controls element searches. Polling searches default to ten seconds;
// a negative Timeout uses only the caller's context. Immediate QuerySelector APIs
// do not poll. IncludeFrames also searches shadow roots and descendant frames.
type QueryOptions struct {
	Timeout       time.Duration
	IncludeFrames bool
	Exact         bool
}

func (t *Tab) document(ctx context.Context) (cdp.DOMNode, error) {
	r, err := cdp.DOM.GetDocument(cdp.DOMGetDocumentParams{Depth: cdp.Ptr(-1), Pierce: cdp.Ptr(true)}).Execute(ctx, t)
	return r.Root, err
}
func nodeChildren(n cdp.DOMNode) []cdp.DOMNode {
	children := append([]cdp.DOMNode(nil), n.Children...)
	children = append(children, n.ShadowRoots...)
	if n.ContentDocument != nil {
		children = append(children, *n.ContentDocument)
	}
	if n.TemplateContent != nil {
		children = append(children, *n.TemplateContent)
	}
	return children
}
func walkNodes(root cdp.DOMNode, fn func(cdp.DOMNode)) {
	fn(root)
	for _, child := range nodeChildren(root) {
		walkNodes(child, fn)
	}
}
func (t *Tab) describe(ctx context.Context, id cdp.DOMNodeID) (*Element, error) {
	r, err := cdp.DOM.DescribeNode(cdp.DOMDescribeNodeParams{NodeID: &id, Depth: cdp.Ptr(-1), Pierce: cdp.Ptr(true)}).Execute(ctx, t)
	if err != nil {
		return nil, err
	}
	return &Element{tab: t, node: r.Node}, nil
}

// QuerySelectorAll returns current CSS matches without waiting for elements to appear.
func (t *Tab) QuerySelectorAll(ctx context.Context, selector string, options ...QueryOptions) ([]*Element, error) {
	root, err := t.document(ctx)
	if err != nil {
		return nil, err
	}
	var opt QueryOptions
	if len(options) > 0 {
		opt = options[0]
	}
	roots := []cdp.DOMNode{root}
	if opt.IncludeFrames {
		walkNodes(root, func(n cdp.DOMNode) {
			if n.ContentDocument != nil {
				roots = append(roots, *n.ContentDocument)
			}
			roots = append(roots, n.ShadowRoots...)
		})
	}
	var elements []*Element
	seen := map[cdp.DOMNodeID]bool{}
	for _, r := range roots {
		result, err := cdp.DOM.QuerySelectorAll(cdp.DOMQuerySelectorAllParams{NodeID: r.NodeID, Selector: selector}).Execute(ctx, t)
		if err != nil {
			return nil, err
		}
		for _, id := range result.NodeIDs {
			if !seen[id] {
				e, err := t.describe(ctx, id)
				if err != nil {
					return nil, err
				}
				elements = append(elements, e)
				seen[id] = true
			}
		}
	}
	if opt.IncludeFrames {
		frames, err := t.GetFrames(ctx)
		if err != nil {
			return nil, err
		}
		for _, f := range frames {
			items, err := f.QuerySelectorAll(ctx, selector, QueryOptions{IncludeFrames: true})
			if err != nil {
				return nil, err
			}
			elements = append(elements, items...)
		}
	}
	return elements, nil
}

// QuerySelector returns the first current CSS match or ErrNotFound without polling.
func (t *Tab) QuerySelector(ctx context.Context, selector string, options ...QueryOptions) (*Element, error) {
	list, err := t.QuerySelectorAll(ctx, selector, options...)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, ErrNotFound
	}
	return list[0], nil
}

// SelectAll polls until at least one element matches the CSS selector.
func (t *Tab) SelectAll(ctx context.Context, selector string, options ...QueryOptions) (result []*Element, err error) {
	limited, cancel, opt := defaultTimeout(ctx, options)
	defer cancel()
	err = poll(limited, func() (bool, error) {
		var e error
		result, e = t.QuerySelectorAll(limited, selector, opt)
		return len(result) > 0, e
	})
	return
}

// Select polls for the first CSS match; see QueryOptions for timeout behavior.
func (t *Tab) Select(ctx context.Context, selector string, options ...QueryOptions) (*Element, error) {
	items, err := t.SelectAll(ctx, selector, options...)
	if err != nil {
		return nil, err
	}
	return items[0], nil
}
func (t *Tab) search(ctx context.Context, query string) ([]*Element, error) {
	if err := t.Enable(ctx, "DOM"); err != nil {
		return nil, err
	}
	result, err := cdp.DOM.PerformSearch(cdp.DOMPerformSearchParams{Query: query, IncludeUserAgentShadowDOM: cdp.Ptr(true)}).Execute(ctx, t)
	if err != nil {
		return nil, err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = cdp.DOM.DiscardSearchResults(cdp.DOMDiscardSearchResultsParams{SearchID: result.SearchID}).Execute(cleanup, t)
	}()
	if result.ResultCount == 0 {
		return nil, nil
	}
	r, err := cdp.DOM.GetSearchResults(cdp.DOMGetSearchResultsParams{SearchID: result.SearchID, FromIndex: 0, ToIndex: result.ResultCount}).Execute(ctx, t)
	if err != nil {
		return nil, err
	}
	var elements []*Element
	for _, id := range r.NodeIDs {
		if id == 0 {
			continue
		}
		e, err := t.describe(ctx, id)
		if err != nil {
			return nil, err
		}
		elements = append(elements, e)
	}
	return elements, nil
}
func (t *Tab) XPath(ctx context.Context, xpath string, options ...QueryOptions) ([]*Element, error) {
	limited, cancel, _ := defaultTimeout(ctx, options)
	defer cancel()
	var result []*Element
	err := poll(limited, func() (bool, error) { var e error; result, e = t.search(limited, xpath); return len(result) > 0, e })
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return result, nil
	}
	return result, err
}
func (t *Tab) FindElementsByText(ctx context.Context, text string, options ...QueryOptions) ([]*Element, error) {
	items, err := t.search(ctx, text)
	if err != nil {
		return nil, err
	}
	seen := map[cdp.DOMBackendNodeID]bool{}
	var result []*Element
	var opt QueryOptions
	if len(options) > 0 {
		opt = options[0]
	}
	for _, e := range items {
		if e.Node().NodeType == 3 {
			var parent cdp.RuntimeRemoteObject
			if err = e.Apply(ctx, "function(){return this.parentElement}", &parent, EvaluateOptions{RemoteObject: true}); err != nil {
				return nil, err
			}
			if parent.ObjectID == nil {
				continue
			}
			r, err := cdp.DOM.DescribeNode(cdp.DOMDescribeNodeParams{ObjectID: parent.ObjectID, Depth: cdp.Ptr(-1), Pierce: cdp.Ptr(true)}).Execute(ctx, t)
			cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
			_, _ = cdp.Runtime.ReleaseObject(cdp.RuntimeReleaseObjectParams{ObjectID: *parent.ObjectID}).Execute(cleanup, t)
			cancel()
			if err != nil {
				return nil, err
			}
			e = &Element{tab: t, node: r.Node}
		}
		if opt.Exact {
			s, err := e.TextAll(ctx)
			if err != nil {
				return nil, err
			}
			if strings.TrimSpace(s) != text {
				continue
			}
		}
		id := e.Node().BackendNodeID
		if !seen[id] {
			seen[id] = true
			result = append(result, e)
		}
	}
	if opt.IncludeFrames {
		frames, err := t.GetFrames(ctx)
		if err != nil {
			return nil, err
		}
		for _, f := range frames {
			items, err := f.FindElementsByText(ctx, text)
			if err != nil {
				return nil, err
			}
			result = append(result, items...)
		}
	}
	return result, nil
}

// FindAll polls until at least one element matches text.
func (t *Tab) FindAll(ctx context.Context, text string, options ...QueryOptions) (result []*Element, err error) {
	limited, cancel, opt := defaultTimeout(ctx, options)
	defer cancel()
	err = poll(limited, func() (bool, error) {
		var e error
		result, e = t.FindElementsByText(limited, text, opt)
		return len(result) > 0, e
	})
	return
}

// Find polls for text and returns the element with the closest text length.
func (t *Tab) Find(ctx context.Context, text string, options ...QueryOptions) (*Element, error) {
	items, err := t.FindAll(ctx, text, options...)
	if err != nil {
		return nil, err
	}
	return bestTextMatch(ctx, items, text)
}

// WaitFor polls for selector, or text when selector is empty.
func (t *Tab) WaitFor(ctx context.Context, selector, text string, options ...QueryOptions) (*Element, error) {
	if selector != "" {
		return t.Select(ctx, selector, options...)
	}
	if text != "" {
		return t.Find(ctx, text, options...)
	}
	return nil, fmt.Errorf("dgodriver: WaitFor needs a selector or text")
}

// GetFrames returns out-of-process iframe targets belonging to this page's frame tree.
func (t *Tab) GetFrames(ctx context.Context) ([]*Tab, error) {
	if err := t.browser.UpdateTargets(ctx); err != nil {
		return nil, err
	}
	result, err := cdp.Page.GetFrameTree().Execute(ctx, t)
	if err != nil {
		return nil, err
	}
	ids := map[cdp.PageFrameID]bool{}
	var visit func(cdp.PageFrameTree)
	visit = func(tree cdp.PageFrameTree) {
		ids[tree.Frame.ID] = true
		for _, child := range tree.ChildFrames {
			visit(child)
		}
	}
	visit(result.FrameTree)
	var frames []*Tab
	for _, target := range t.browser.Targets() {
		info := target.Target()
		if info.Type == "iframe" && info.ParentFrameID != nil && ids[*info.ParentFrameID] {
			frames = append(frames, target)
		}
	}
	return frames, nil
}
func (t *Tab) InspectorURL() string {
	u, _ := url.Parse(t.browser.WebSocketURL())
	return "http://" + u.Host + "/devtools/inspector.html?ws=" + url.QueryEscape(u.Host+"/devtools/page/"+t.TargetID())
}
func (t *Tab) OpenExternalInspector() error { return openExternal(t.InspectorURL()) }
func openExternal(address string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", address)
	case "darwin":
		cmd = exec.Command("open", address)
	default:
		cmd = exec.Command("xdg-open", address)
	}
	configureProcess(cmd)
	return cmd.Run()
}
func (t *Tab) BypassInsecureConnectionWarning(ctx context.Context) error {
	button, err := t.Select(ctx, "#details-button")
	if err != nil {
		return err
	}
	if err = button.Click(ctx); err != nil {
		return err
	}
	proceed, err := t.Select(ctx, "#proceed-link")
	if err != nil {
		return err
	}
	return proceed.Click(ctx)
}
