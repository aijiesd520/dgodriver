package dgodriver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestConnectionConcurrentEventsAndCancellation(t *testing.T) {
	var serverMu sync.Mutex
	var socket *websocket.Conn
	upgrade := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrade.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		serverMu.Lock()
		socket = ws
		serverMu.Unlock()
		defer ws.Close()
		var writeMu sync.Mutex
		for {
			var request wireMessage
			if ws.ReadJSON(&request) != nil {
				return
			}
			go func(m wireMessage) {
				if m.Method == "Never.reply" {
					return
				}
				time.Sleep(time.Duration(10-m.ID%10) * time.Millisecond)
				writeMu.Lock()
				defer writeMu.Unlock()
				if m.Method == "Fire.event" {
					_ = ws.WriteJSON(Event{Method: "Test.ready", Params: json.RawMessage(`{"value":42}`)})
				}
				if m.Method == "Fire.session" {
					_ = ws.WriteJSON(Event{Method: "Session.ready", Params: json.RawMessage(`{}`), SessionID: m.SessionID})
				}
				if m.Method == "Detach.session" {
					_ = ws.WriteJSON(Event{Method: "Target.detachedFromTarget", Params: json.RawMessage(`{"sessionId":"child"}`)})
				}
				if m.Method == "Fail.command" {
					_ = ws.WriteJSON(wireMessage{ID: m.ID, Error: &ProtocolError{Code: -32601, Message: "unknown command"}})
				} else {
					_ = ws.WriteJSON(wireMessage{ID: m.ID, Result: json.RawMessage(fmt.Sprintf(`{"method":%q}`, m.Method))})
				}
			}(request)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	callbackDone := make(chan error, 1)
	sub := c.AddHandler(ctx, "Test.ready", func(e Event) { callbackDone <- c.Send(ctx, "Callback.command", nil, nil) })
	defer sub.Close()
	if err = c.Send(ctx, "Fire.event", nil, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-callbackDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("callback deadlock")
	}
	var wg sync.WaitGroup
	child := c.transport.session("child")
	rootEvents := make(chan struct{}, 1)
	childEvents := make(chan struct{}, 1)
	rootSubscription := c.AddHandler(ctx, "Session.ready", func(Event) { rootEvents <- struct{}{} })
	defer rootSubscription.Close()
	childSubscription := child.AddHandler(ctx, "Session.ready", func(Event) { childEvents <- struct{}{} })
	defer childSubscription.Close()
	if err = child.Send(ctx, "Fire.session", nil, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-childEvents:
	case <-ctx.Done():
		t.Fatal("missing child event")
	}
	select {
	case <-rootEvents:
		t.Fatal("session event leaked to browser")
	default:
	}
	childPending := make(chan error, 1)
	go func() { childPending <- child.Send(ctx, "Never.reply", nil, nil) }()
	if err = c.Send(ctx, "Detach.session", nil, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-childPending:
		if !errors.Is(err, ErrClosed) {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("detached session kept pending call")
	}
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var result struct{ Method string }
			method := fmt.Sprintf("Echo.%d", i)
			if err := c.Send(ctx, method, nil, &result); err != nil || result.Method != method {
				t.Errorf("wrong response: %s %v", result.Method, err)
			}
		}(i)
	}
	wg.Wait()
	limited, stop := context.WithTimeout(ctx, 20*time.Millisecond)
	defer stop()
	if err = c.Send(limited, "Never.reply", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	c.transport.mu.Lock()
	pending := len(c.transport.pending)
	c.transport.mu.Unlock()
	if pending != 0 {
		t.Fatalf("leaked pending calls: %d", pending)
	}
	var protocolError *ProtocolError
	if err = c.Send(ctx, "Fail.command", nil, nil); !errors.As(err, &protocolError) || protocolError.Method != "Fail.command" {
		t.Fatal(err)
	}
	failed := make(chan error, 1)
	go func() { failed <- c.Send(ctx, "Never.reply", nil, nil) }()
	time.Sleep(10 * time.Millisecond)
	serverMu.Lock()
	socket.Close()
	serverMu.Unlock()
	select {
	case err = <-failed:
		if !errors.Is(err, ErrClosed) {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("pending call not released")
	}
}
