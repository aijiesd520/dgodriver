package dgodriver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aijiesd520/dgodriver/cdp"
)

// Element retains a backend node ID across DOM agent refreshes. Node() exposes
// all protocol metadata, including closed shadow roots and content documents.
type Element struct {
	tab       *Tab
	mu        sync.Mutex
	node      cdp.DOMNode
	recording *VideoRecording
}

// Tab returns the target containing this element.
func (e *Element) Tab() *Tab { return e.tab }

// Node returns the cached DOM metadata; Update refreshes it.
func (e *Element) Node() cdp.DOMNode {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.node
}

// TagName returns the cached node name in lowercase.
func (e *Element) TagName() string { return strings.ToLower(e.Node().NodeName) }

// Attributes returns the cached HTML attributes as a new map.
func (e *Element) Attributes() map[string]string {
	attrs := e.Node().Attributes
	result := map[string]string{}
	for i := 0; i+1 < len(attrs); i += 2 {
		result[attrs[i]] = attrs[i+1]
	}
	return result
}

// Get returns a cached attribute value, or an empty string if absent.
func (e *Element) Get(name string) string { return e.Attributes()[name] }

// Children returns the cached child nodes.
func (e *Element) Children() []*Element {
	var result []*Element
	for _, n := range e.Node().Children {
		result = append(result, &Element{tab: e.tab, node: n})
	}
	return result
}

// ShadowRoots returns the cached shadow roots, including closed roots exposed by CDP.
func (e *Element) ShadowRoots() []*Element {
	var result []*Element
	for _, n := range e.Node().ShadowRoots {
		result = append(result, &Element{tab: e.tab, node: n})
	}
	return result
}

// ShadowChildren returns the cached children of this element's shadow roots.
func (e *Element) ShadowChildren() []*Element {
	var result []*Element
	for _, root := range e.ShadowRoots() {
		result = append(result, root.Children()...)
	}
	return result
}

// ContentDocument returns a cached in-process frame document, or nil if absent.
func (e *Element) ContentDocument() *Element {
	n := e.Node().ContentDocument
	if n == nil {
		return nil
	}
	return &Element{tab: e.tab, node: *n}
}

// Update refreshes the element's metadata using its backend node ID.
func (e *Element) Update(ctx context.Context) error {
	n := e.Node()
	r, err := cdp.DOM.DescribeNode(cdp.DOMDescribeNodeParams{BackendNodeID: &n.BackendNodeID, Depth: cdp.Ptr(-1), Pierce: cdp.Ptr(true)}).Execute(ctx, e.tab)
	if err == nil {
		e.mu.Lock()
		e.node = r.Node
		e.mu.Unlock()
	}
	return err
}

// RemoteObject resolves this node into a JavaScript object; release it via CDP when done.
func (e *Element) RemoteObject(ctx context.Context) (cdp.RuntimeRemoteObject, error) {
	n := e.Node()
	r, err := cdp.DOM.ResolveNode(cdp.DOMResolveNodeParams{BackendNodeID: &n.BackendNodeID}).Execute(ctx, e.tab)
	return r.Object, err
}

// ObjectID returns a JavaScript object ID; release it via Runtime.releaseObject when done.
func (e *Element) ObjectID(ctx context.Context) (cdp.RuntimeRemoteObjectID, error) {
	object, err := e.RemoteObject(ctx)
	if err != nil {
		return "", err
	}
	if object.ObjectID == nil {
		return "", fmt.Errorf("dgodriver: node has no remote object")
	}
	return *object.ObjectID, nil
}
func releaseObject(t *Tab, id cdp.RuntimeRemoteObjectID) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _ = cdp.Runtime.ReleaseObject(cdp.RuntimeReleaseObjectParams{ObjectID: id}).Execute(ctx, t)
}

// Apply invokes a JavaScript function with the element as both this and its first
// argument (for example, "el => el.textContent"). See EvaluateOptions for results.
func (e *Element) Apply(ctx context.Context, jsFunction string, out any, options ...EvaluateOptions) error {
	id, err := e.ObjectID(ctx)
	if err != nil {
		return err
	}
	defer releaseObject(e.tab, id)
	var opt EvaluateOptions
	if len(options) > 0 {
		opt = options[0]
	}
	r, err := cdp.Runtime.CallFunctionOn(cdp.RuntimeCallFunctionOnParams{ObjectID: &id, FunctionDeclaration: "function(){return (" + jsFunction + ").call(this,this)}", AwaitPromise: &opt.AwaitPromise, ReturnByValue: cdp.Ptr(!opt.RemoteObject), UserGesture: cdp.Ptr(true)}).Execute(ctx, e.tab)
	if err != nil {
		return err
	}
	return decodeEvaluation(r.Result, r.ExceptionDetails, out, opt.RemoteObject)
}
func (e *Element) related(ctx context.Context, expression string) (*Element, error) {
	var object cdp.RuntimeRemoteObject
	if err := e.Apply(ctx, "el=>"+expression, &object, EvaluateOptions{RemoteObject: true}); err != nil {
		return nil, err
	}
	if object.ObjectID == nil {
		return nil, ErrNotFound
	}
	defer releaseObject(e.tab, *object.ObjectID)
	r, err := cdp.DOM.DescribeNode(cdp.DOMDescribeNodeParams{ObjectID: object.ObjectID, Depth: cdp.Ptr(-1), Pierce: cdp.Ptr(true)}).Execute(ctx, e.tab)
	if err != nil {
		return nil, err
	}
	return &Element{tab: e.tab, node: r.Node}, nil
}

// Parent resolves the current parent node, or returns ErrNotFound when absent.
func (e *Element) Parent(ctx context.Context) (*Element, error) {
	return e.related(ctx, "el.parentNode")
}

// GetFrame resolves an out-of-process iframe target. For in-process frames use ContentDocument.
func (e *Element) GetFrame(ctx context.Context) (*Tab, error) {
	n := e.Node()
	if n.FrameID == nil {
		return nil, fmt.Errorf("dgodriver: element is not a frame")
	}
	for _, t := range e.tab.browser.Targets() {
		if t.TargetID() == string(*n.FrameID) {
			return t, nil
		}
	}
	return nil, fmt.Errorf("dgodriver: frame is in-process; use ContentDocument or QuerySelector")
}

// Click invokes the element's JavaScript click method; MouseClick sends mouse input.
func (e *Element) Click(ctx context.Context) error { return e.Apply(ctx, "el=>el.click()", nil) }

// Focus gives keyboard focus to the element.
func (e *Element) Focus(ctx context.Context) error {
	n := e.Node()
	_, err := cdp.DOM.Focus(cdp.DOMFocusParams{BackendNodeID: &n.BackendNodeID}).Execute(ctx, e.tab)
	return err
}

// ScrollIntoView scrolls the element into the viewport when needed.
func (e *Element) ScrollIntoView(ctx context.Context) error {
	n := e.Node()
	_, err := cdp.DOM.ScrollIntoViewIfNeeded(cdp.DOMScrollIntoViewIfNeededParams{BackendNodeID: &n.BackendNodeID}).Execute(ctx, e.tab)
	return err
}

// TextAll reads textContent, including descendant text.
func (e *Element) TextAll(ctx context.Context) (string, error) {
	var text string
	err := e.Apply(ctx, "el=>el.textContent", &text)
	return text, err
}

// Text reads only direct text-node children.
func (e *Element) Text(ctx context.Context) (string, error) {
	var text string
	err := e.Apply(ctx, "el=>Array.from(el.childNodes).filter(n=>n.nodeType===3).map(n=>n.textContent).join('')", &text)
	return text, err
}

// GetHTML returns the element's outer HTML.
func (e *Element) GetHTML(ctx context.Context) (string, error) {
	n := e.Node()
	r, err := cdp.DOM.GetOuterHTML(cdp.DOMGetOuterHTMLParams{BackendNodeID: &n.BackendNodeID}).Execute(ctx, e.tab)
	return r.OuterHTML, err
}
func jsString(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

// SetText replaces textContent, removing existing child nodes.
func (e *Element) SetText(ctx context.Context, value string) error {
	return e.Apply(ctx, "el=>{el.textContent="+jsString(value)+"}", nil)
}

// SetValue uses the native setter so React/Vue controlled inputs see input events.
func (e *Element) SetValue(ctx context.Context, value string) error {
	return e.Apply(ctx, "el=>{let proto=el;let setter;while(proto&&!setter){setter=Object.getOwnPropertyDescriptor(proto,'value')?.set;proto=Object.getPrototypeOf(proto)}const native=Object.getOwnPropertyDescriptor(Object.getPrototypeOf(el),'value')?.set;const set=native||setter;if(!set)throw new Error('element has no value setter');set.call(el,"+jsString(value)+");el.dispatchEvent(new InputEvent('input',{bubbles:true,cancelable:true,inputType:'insertText',data:"+jsString(value)+"}));el.dispatchEvent(new Event('change',{bubbles:true}))}", nil)
}

// ClearInput clears the value using the native setter and dispatches input/change events.
func (e *Element) ClearInput(ctx context.Context) error { return e.SetValue(ctx, "") }

// ClearInputByDeleting selects all text and sends Backspace using keyboard input.
func (e *Element) ClearInputByDeleting(ctx context.Context) error {
	if err := e.Focus(ctx); err != nil {
		return err
	}
	modifier := Control
	if runtimeOS() == "darwin" {
		modifier = Meta
	}
	if err := e.tab.PressKey(ctx, "a", modifier); err != nil {
		return err
	}
	return e.tab.PressKey(ctx, "Backspace")
}

// SendKeys focuses the element and types text using keyboard input.
func (e *Element) SendKeys(ctx context.Context, text string) error {
	if err := e.Focus(ctx); err != nil {
		return err
	}
	return e.tab.TypeText(ctx, text)
}

// PressKey focuses the element and dispatches a key with optional modifiers.
func (e *Element) PressKey(ctx context.Context, key string, modifiers ...KeyModifiers) error {
	if err := e.Focus(ctx); err != nil {
		return err
	}
	return e.tab.PressKey(ctx, key, modifiers...)
}

// SendFile selects local files for a file input using absolute paths.
func (e *Element) SendFile(ctx context.Context, paths ...string) error {
	files := make([]string, len(paths))
	for i, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return err
		}
		info, err := os.Stat(abs)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return fmt.Errorf("dgodriver: upload path is a directory")
		}
		files[i] = abs
	}
	n := e.Node()
	_, err := cdp.DOM.SetFileInputFiles(cdp.DOMSetFileInputFilesParams{Files: files, BackendNodeID: &n.BackendNodeID}).Execute(ctx, e.tab)
	return err
}
func (e *Element) SelectOption(ctx context.Context, values ...string) error {
	raw, _ := json.Marshal(values)
	return e.Apply(ctx, "el=>{const values="+string(raw)+";if(el.tagName==='OPTION'){el.selected=true;el=el.parentElement}else{for(const option of el.options)option.selected=values.includes(option.value)||values.includes(option.text)}el.dispatchEvent(new Event('input',{bubbles:true}));el.dispatchEvent(new Event('change',{bubbles:true}))}", nil)
}
func (e *Element) GetJSAttributes(ctx context.Context) (map[string]any, error) {
	var attrs map[string]any
	err := e.Apply(ctx, "el=>{const out={};for(const key in el){try{const v=el[key];if(v===null||['string','number','boolean'].includes(typeof v))out[key]=v}catch{}}return out}", &attrs)
	return attrs, err
}
func (e *Element) SetAttribute(ctx context.Context, name, value string) error {
	return e.Apply(ctx, "el=>el.setAttribute("+jsString(name)+","+jsString(value)+")", nil)
}
func (e *Element) RemoveAttribute(ctx context.Context, name string) error {
	return e.Apply(ctx, "el=>el.removeAttribute("+jsString(name)+")", nil)
}
func (e *Element) SaveToDOM(ctx context.Context, attributes ...map[string]string) error {
	attrs := e.Attributes()
	if len(attributes) > 0 {
		attrs = attributes[0]
	}
	raw, _ := json.Marshal(attrs)
	return e.Apply(ctx, "el=>Object.entries("+string(raw)+").forEach(([k,v])=>el.setAttribute(k,v))", nil)
}
func (e *Element) RemoveFromDOM(ctx context.Context) error {
	return e.Apply(ctx, "el=>el.remove()", nil)
}
func (e *Element) QuerySelectorAll(ctx context.Context, selector string) ([]*Element, error) {
	n := e.Node()
	if strings.EqualFold(n.NodeName, "iframe") {
		if n.ContentDocument != nil {
			n = *n.ContentDocument
		} else {
			frame, err := e.GetFrame(ctx)
			if err != nil {
				return nil, err
			}
			return frame.QuerySelectorAll(ctx, selector)
		}
	}
	if n.NodeID == 0 {
		if err := e.Update(ctx); err != nil {
			return nil, err
		}
		n = e.Node()
	}
	// Backend nodes may have no frontend ID until DOM.requestNode pushes them.
	object, err := e.ObjectID(ctx)
	if err != nil {
		return nil, err
	}
	defer releaseObject(e.tab, object)
	if n.NodeID == 0 {
		r, err := cdp.DOM.RequestNode(cdp.DOMRequestNodeParams{ObjectID: object}).Execute(ctx, e.tab)
		if err != nil {
			return nil, err
		}
		n.NodeID = r.NodeID
	}
	r, err := cdp.DOM.QuerySelectorAll(cdp.DOMQuerySelectorAllParams{NodeID: n.NodeID, Selector: selector}).Execute(ctx, e.tab)
	if err != nil {
		return nil, err
	}
	var items []*Element
	for _, id := range r.NodeIDs {
		element, err := e.tab.describe(ctx, id)
		if err != nil {
			return nil, err
		}
		items = append(items, element)
	}
	return items, nil
}
func (e *Element) QuerySelector(ctx context.Context, selector string) (*Element, error) {
	items, err := e.QuerySelectorAll(ctx, selector)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, ErrNotFound
	}
	return items[0], nil
}

type Position struct {
	X, Y, Width, Height      float64
	Left, Top, Right, Bottom float64
	AbsX, AbsY               float64
}

func (p Position) Center() (float64, float64) { return p.X + p.Width/2, p.Y + p.Height/2 }
func (p Position) ToViewport(scale float64) cdp.PageViewport {
	if scale <= 0 {
		scale = 1
	}
	return cdp.PageViewport{X: p.X, Y: p.Y, Width: p.Width, Height: p.Height, Scale: scale}
}
func (e *Element) GetPosition(ctx context.Context, absolute ...bool) (Position, error) {
	var p Position
	err := e.Apply(ctx, "el=>{const r=el.getBoundingClientRect();let x=r.x,y=r.y,w=el.ownerDocument.defaultView;const sx=w.scrollX,sy=w.scrollY;try{while(w.frameElement){const f=w.frameElement.getBoundingClientRect();x+=f.x+w.frameElement.clientLeft;y+=f.y+w.frameElement.clientTop;w=w.parent}}catch{}return {X:x,Y:y,Width:r.width,Height:r.height,Left:x,Top:y,Right:x+r.width,Bottom:y+r.height,AbsX:x+sx,AbsY:y+sy}}", &p)
	if len(absolute) > 0 && absolute[0] {
		p.X = p.AbsX
		p.Y = p.AbsY
	}
	return p, err
}
func (e *Element) MouseClick(ctx context.Context, options ...MouseOptions) error {
	if err := e.ScrollIntoView(ctx); err != nil {
		return err
	}
	p, err := e.GetPosition(ctx)
	if err != nil {
		return err
	}
	if p.Width <= 0 || p.Height <= 0 {
		return fmt.Errorf("dgodriver: element has no visible box")
	}
	x, y := p.Center()
	return e.tab.MouseClick(ctx, x, y, options...)
}
func (e *Element) MouseMove(ctx context.Context) error {
	if err := e.ScrollIntoView(ctx); err != nil {
		return err
	}
	p, err := e.GetPosition(ctx)
	if err != nil {
		return err
	}
	x, y := p.Center()
	return e.tab.MouseMove(ctx, x, y)
}
func (e *Element) MouseDrag(ctx context.Context, destination *Element, steps ...int) error {
	p, err := e.GetPosition(ctx)
	if err != nil {
		return err
	}
	q, err := destination.GetPosition(ctx)
	if err != nil {
		return err
	}
	x, y := p.Center()
	dx, dy := q.Center()
	return e.tab.MouseDrag(ctx, x, y, dx, dy, steps...)
}
func (e *Element) Flash(ctx context.Context, duration ...time.Duration) error {
	ms := 500
	if len(duration) > 0 {
		ms = int(duration[0] / time.Millisecond)
	}
	return e.Apply(ctx, fmt.Sprintf("el=>{const old=el.style.outline;el.style.outline='3px solid #ff4081';setTimeout(()=>{el.style.outline=old},%d)}", ms), nil)
}
func (e *Element) HighlightOverlay(ctx context.Context) error {
	if err := e.tab.Enable(ctx, "DOM"); err != nil {
		return err
	}
	if err := e.tab.Enable(ctx, "Overlay"); err != nil {
		return err
	}
	n := e.Node()
	return e.tab.Send(ctx, "Overlay.highlightNode", map[string]any{"backendNodeId": n.BackendNodeID, "highlightConfig": map[string]any{"showInfo": true, "contentColor": map[string]any{"r": 255, "g": 64, "b": 129, "a": 0.3}}}, nil)
}
