package dgodriver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aijiesd520/dgodriver/cdp"
	"github.com/gorilla/websocket"
)

// ErrClosed indicates that a connection, target or subscription has closed.
var ErrClosed = errors.New("dgodriver: connection closed")

// ErrNotFound indicates that an immediate element or page query found no match.
var ErrNotFound = errors.New("dgodriver: element not found")

// ProtocolError is a CDP command error, including the command's method name.
type ProtocolError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
	Method  string          `json:"-"`
}

func (e *ProtocolError) Error() string {
	return fmt.Sprintf("dgodriver: %s: %s (CDP %d)", e.Method, e.Message, e.Code)
}

// Event contains an unmodified CDP event payload and its optional target session.
type Event struct {
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params"`
	SessionID string          `json:"sessionId,omitempty"`
}

// Decode unmarshals the event parameters into out.
func (e Event) Decode(out any) error { return json.Unmarshal(e.Params, out) }

type wireMessage struct {
	ID        int64           `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *ProtocolError  `json:"error,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
}
type pendingCall struct {
	session  string
	response chan wireMessage
}

type transport struct {
	ws        *websocket.Conn
	writeLock chan struct{}
	mu        sync.Mutex
	next      atomic.Int64
	pending   map[int64]pendingCall
	sessions  map[string]*Connection
	done      chan struct{}
	once      sync.Once
	err       error
}

// Connection is a browser connection or a flattened CDP target session.
// Concurrent Send calls are safe. Callbacks execute outside the WebSocket reader.
type Connection struct {
	transport *transport
	session   string
	mu        sync.Mutex
	handlers  map[*Subscription]struct{}
	done      chan struct{}
	once      sync.Once
	enableMu  sync.Mutex
	enabled   map[string]bool
	lastEvent atomic.Int64
}

// Dial opens a browser-level CDP connection. ctx bounds only the connection setup.
// Close the returned connection when finished.
func Dial(ctx context.Context, websocketURL string) (*Connection, error) {
	ws, response, err := websocket.DefaultDialer.DialContext(ctx, websocketURL, nil)
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("dgodriver: connect: %w", err)
	}
	t := &transport{
		ws:        ws,
		writeLock: make(chan struct{}, 1),
		pending:   make(map[int64]pendingCall),
		sessions:  make(map[string]*Connection),
		done:      make(chan struct{}),
	}
	c := t.session("")
	go t.readLoop()
	return c, nil
}
func (t *transport) session(id string) *Connection {
	t.mu.Lock()
	defer t.mu.Unlock()
	if c := t.sessions[id]; c != nil {
		return c
	}
	c := &Connection{transport: t, session: id, handlers: make(map[*Subscription]struct{}), enabled: make(map[string]bool), done: make(chan struct{})}
	c.lastEvent.Store(time.Now().UnixNano())
	t.sessions[id] = c
	return c
}

func (t *transport) removeSession(id string) {
	if id == "" {
		return
	}
	t.mu.Lock()
	delete(t.sessions, id)
	t.mu.Unlock()
}

// DefaultSendTimeout bounds commands without a context deadline. Supply a context
// with a deadline to allow a longer command or enforce a shorter timeout.
const DefaultSendTimeout = 60 * time.Second

// SessionID returns the flattened target session ID; the browser session is empty.
func (c *Connection) SessionID() string { return c.session }

// Done is closed when this connection or target session terminates.
func (c *Connection) Done() <-chan struct{} { return c.done }

// Closed reports whether the connection or session has terminated.
func (c *Connection) Closed() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// Send executes a CDP command, decoding its result into out when non-nil.
// Commands without a context deadline use DefaultSendTimeout. Concurrent calls
// are safe; a response timeout does not close the connection.
func (c *Connection) Send(ctx context.Context, method string, params, out any) error {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultSendTimeout)
		defer cancel()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.Closed() {
		return ErrClosed
	}
	t := c.transport
	id := t.next.Add(1)
	request := struct {
		ID      int64  `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
		Session string `json:"sessionId,omitempty"`
	}{id, method, params, c.session}
	raw, err := json.Marshal(request)
	if err != nil {
		return err
	}
	response := make(chan wireMessage, 1)
	t.mu.Lock()
	select {
	case <-t.done:
		t.mu.Unlock()
		return ErrClosed
	default:
	}
	t.pending[id] = pendingCall{c.session, response}
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		delete(t.pending, id)
		t.mu.Unlock()
	}()
	select {
	case t.writeLock <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return ErrClosed
	case <-t.done:
		return ErrClosed
	}
	if err = ctx.Err(); err != nil {
		<-t.writeLock
		return err
	}
	if c.Closed() {
		<-t.writeLock
		return ErrClosed
	}
	deadline := time.Now().Add(30 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	err = t.ws.SetWriteDeadline(deadline)
	if err == nil {
		err = t.ws.WriteMessage(websocket.TextMessage, raw)
	}
	<-t.writeLock
	if err != nil {
		t.shutdown(err)
		return fmt.Errorf("dgodriver: send %s: %w", method, err)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return ErrClosed
	case <-t.done:
		t.mu.Lock()
		err = t.err
		t.mu.Unlock()
		return fmt.Errorf("%w: %v", ErrClosed, err)
	case msg := <-response:
		if msg.Error != nil {
			msg.Error.Method = method
			return msg.Error
		}
		domain, action, ok := strings.Cut(method, ".")
		if ok && (action == "enable" || action == "disable") {
			c.mu.Lock()
			if action == "enable" {
				c.enabled[domain] = true
			} else {
				delete(c.enabled, domain)
			}
			c.mu.Unlock()
		}
		if out != nil && len(msg.Result) > 0 {
			return json.Unmarshal(msg.Result, out)
		}
		return nil
	}
}

// Call executes a CDP command and returns its raw JSON result.
func (c *Connection) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	var result json.RawMessage
	err := c.Send(ctx, method, params, &result)
	return result, err
}
func (t *transport) readLoop() {
	for {
		_, raw, err := t.ws.ReadMessage()
		if err != nil {
			t.shutdown(err)
			return
		}
		var msg wireMessage
		if err = json.Unmarshal(raw, &msg); err != nil {
			t.shutdown(err)
			return
		}
		if msg.ID != 0 {
			t.mu.Lock()
			p, ok := t.pending[msg.ID]
			delete(t.pending, msg.ID)
			t.mu.Unlock()
			if ok {
				p.response <- msg
			}
			continue
		}
		t.mu.Lock()
		c := t.sessions[msg.SessionID]
		t.mu.Unlock()
		if c != nil {
			c.dispatch(Event{msg.Method, msg.Params, msg.SessionID})
		}
		if msg.Method == "Target.detachedFromTarget" {
			var p struct {
				SessionID string `json:"sessionId"`
			}
			if json.Unmarshal(msg.Params, &p) == nil {
				t.mu.Lock()
				child := t.sessions[p.SessionID]
				delete(t.sessions, p.SessionID)
				t.mu.Unlock()
				if child != nil {
					child.finish()
				}
			}
		}
	}
}
func (c *Connection) dispatch(e Event) {
	c.lastEvent.Store(time.Now().UnixNano())
	c.mu.Lock()
	defer c.mu.Unlock()
	for s := range c.handlers {
		if s.method == "*" || s.method == e.Method || strings.TrimSuffix(s.method, ".*") == strings.SplitN(e.Method, ".", 2)[0] {
			s.enqueue(e)
		}
	}
}
func (t *transport) shutdown(err error) {
	t.once.Do(func() {
		t.mu.Lock()
		t.err = err
		close(t.done)
		sessions := make([]*Connection, 0, len(t.sessions))
		for _, c := range t.sessions {
			sessions = append(sessions, c)
		}
		t.pending = make(map[int64]pendingCall)
		t.mu.Unlock()
		_ = t.ws.Close()
		for _, c := range sessions {
			c.finish()
		}
	})
}
func (c *Connection) finish() {
	c.once.Do(func() {
		close(c.done)
		c.mu.Lock()
		for s := range c.handlers {
			s.stop()
		}
		c.handlers = make(map[*Subscription]struct{})
		c.mu.Unlock()
	})
}

// Close closes a browser connection or detaches a target session.
// It does not terminate a browser process; use Browser.Close for launched browsers.
func (c *Connection) Close() error {
	if c.session == "" {
		c.transport.shutdown(ErrClosed)
		return nil
	}
	if c.Closed() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	root := c.transport.session("")
	err := root.Send(ctx, "Target.detachFromTarget", map[string]any{"sessionId": c.session}, nil)
	c.finish()
	c.transport.mu.Lock()
	delete(c.transport.sessions, c.session)
	c.transport.mu.Unlock()
	return err
}

// Subscription delivers events serially in order to a single handler, outside
// the WebSocket reader. Its queue is unbounded: slow callbacks can grow memory
// usage. Keep handlers short and use a bounded worker queue for expensive work.
// Close or cancel the subscription context when finished. Close is safe from
// the callback and discards pending events without waiting for a running callback.
type Subscription struct {
	conn     *Connection
	method   string
	callback func(Event)
	mu       sync.Mutex
	queue    []Event
	wake     chan struct{}
	done     chan struct{}
	once     sync.Once
}

func (s *Subscription) enqueue(e Event) {
	s.mu.Lock()
	select {
	case <-s.done:
		s.mu.Unlock()
		return
	default:
	}
	s.queue = append(s.queue, e)
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
func (s *Subscription) stop() {
	s.once.Do(func() {
		close(s.done)
		s.mu.Lock()
		s.queue = nil
		s.mu.Unlock()
	})
}

// Close unregisters the handler and releases pending events.
func (s *Subscription) Close() error {
	s.conn.mu.Lock()
	delete(s.conn.handlers, s)
	s.conn.mu.Unlock()
	s.stop()
	return nil
}
func (s *Subscription) loop(ctx context.Context) {
	defer s.Close()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.done:
			return
		case <-s.wake:
		}
		for {
			s.mu.Lock()
			if len(s.queue) == 0 {
				s.mu.Unlock()
				break
			}
			e := s.queue[0]
			s.queue[0] = Event{}
			s.queue = s.queue[1:]
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-s.done:
				return
			default:
			}
			s.callback(e)
		}
	}
}

// AddHandler registers without enabling a domain. method accepts an exact event
// name, "Domain.*" or "*". Use On for automatic enable. See Subscription for
// callback ordering, queue growth and cleanup requirements.
func (c *Connection) AddHandler(ctx context.Context, method string, fn func(Event)) *Subscription {
	s := &Subscription{conn: c, method: method, callback: fn, wake: make(chan struct{}, 1), done: make(chan struct{})}
	c.mu.Lock()
	if c.Closed() {
		s.stop()
	} else {
		c.handlers[s] = struct{}{}
	}
	c.mu.Unlock()
	go s.loop(ctx)
	return s
}

// Enable enables a CDP domain once; CSS and Overlay also enable DOM.
func (c *Connection) Enable(ctx context.Context, domain string) error {
	if domain == "CSS" || domain == "Overlay" {
		if err := c.Enable(ctx, "DOM"); err != nil {
			return err
		}
	}
	c.enableMu.Lock()
	defer c.enableMu.Unlock()
	c.mu.Lock()
	enabled := c.enabled[domain]
	c.mu.Unlock()
	if enabled {
		return nil
	}
	if err := c.Send(ctx, domain+".enable", nil, nil); err != nil {
		return err
	}
	return nil
}

// Disable disables a CDP domain and clears its enabled state on success.
func (c *Connection) Disable(ctx context.Context, domain string) error {
	c.enableMu.Lock()
	defer c.enableMu.Unlock()
	return c.Send(ctx, domain+".disable", nil, nil)
}

// On registers a handler and enables supported event domains automatically.
// See AddHandler for patterns and Subscription for queue and lifetime semantics.
func (c *Connection) On(ctx context.Context, method string, fn func(Event)) (*Subscription, error) {
	s := c.AddHandler(ctx, method, fn)
	domain := strings.SplitN(method, ".", 2)[0]
	switch domain {
	case "Network", "Page", "Runtime", "DOM", "Log", "Security", "Performance", "CSS", "Inspector", "Debugger", "Profiler", "Accessibility":
		if err := c.Enable(ctx, domain); err != nil {
			s.Close()
			return nil, err
		}
	}
	return s, nil
}

// RemoveHandlers closes handlers registered with method; empty method removes all.
func (c *Connection) RemoveHandlers(method string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for s := range c.handlers {
		if method == "" || s.method == method {
			s.stop()
			delete(c.handlers, s)
		}
	}
}

// OnEvent decodes generated CDP events before delivering them to the callback.
func OnEvent[T cdp.Event](ctx context.Context, c *Connection, fn func(T)) (*Subscription, error) {
	var sample T
	value := reflect.ValueOf(&sample).Elem()
	if value.Kind() == reflect.Pointer {
		value.Set(reflect.New(value.Type().Elem()))
	}
	return c.On(ctx, sample.CDPEventName(), func(e Event) {
		var value T
		if e.Decode(&value) == nil {
			fn(value)
		}
	})
}

// Sleep waits for duration or returns early when ctx is canceled.
func Sleep(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Wait waits until no CDP events have arrived for idle (default 250 milliseconds).
// This measures CDP event activity, not network or document readiness.
func (c *Connection) Wait(ctx context.Context, idle time.Duration) error {
	if idle <= 0 {
		idle = 250 * time.Millisecond
	}
	for {
		if c.Closed() {
			return ErrClosed
		}
		remaining := idle - time.Since(time.Unix(0, c.lastEvent.Load()))
		if remaining <= 0 {
			return nil
		}
		if err := Sleep(ctx, remaining); err != nil {
			return err
		}
	}
}
