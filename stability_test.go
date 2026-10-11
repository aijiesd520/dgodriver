package dgodriver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aijiesd520/dgodriver/cdp"
	"github.com/gorilla/websocket"
)

func TestTargetRemovalPrunesOrderAndSession(t *testing.T) {
	tr := &transport{sessions: make(map[string]*Connection)}
	b := &Browser{targets: make(map[string]*Tab)}
	first := b.upsert(cdp.TargetTargetInfo{TargetID: "first", Type: "page"}, nil)
	first.conn = tr.session("first-session")
	b.upsert(cdp.TargetTargetInfo{TargetID: "second", Type: "page"}, nil)
	backing := b.order[:cap(b.order)]
	b.removeTarget("first")
	b.removeTarget("first")
	if !reflect.DeepEqual(b.order, []string{"second"}) || backing[1] != "" {
		t.Fatalf("order not pruned and cleared: order=%v backing=%v", b.order, backing)
	}
	if !first.destroyed || !first.conn.Closed() || len(tr.sessions) != 0 {
		t.Fatal("closed target or session retained")
	}
	for n := 0; n < 1000; n++ {
		id := cdp.TargetTargetID(fmt.Sprintf("churn-%d", n))
		b.upsert(cdp.TargetTargetInfo{TargetID: id}, nil)
		b.removeTarget(string(id))
	}
	if len(b.order) != 1 || len(b.targets) != 1 {
		t.Fatalf("closed targets accumulated: order=%d targets=%d", len(b.order), len(b.targets))
	}
}

func TestTargetEventsPreserveLifecycleOrder(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		for {
			var request wireMessage
			if ws.ReadJSON(&request) != nil {
				return
			}
			if request.Method == "Target.setDiscoverTargets" {
				for n := 0; n < 200; n++ {
					id := fmt.Sprintf("target-%d", n)
					info, _ := json.Marshal(map[string]any{"targetInfo": map[string]any{"targetId": id, "type": "page"}})
					destroyed, _ := json.Marshal(map[string]any{"targetId": id})
					_ = ws.WriteJSON(Event{Method: "Target.targetCreated", Params: info})
					_ = ws.WriteJSON(Event{Method: "Target.targetDestroyed", Params: destroyed})
					_ = ws.WriteJSON(Event{Method: "Target.targetInfoChanged", Params: info})
				}
				_ = ws.WriteJSON(Event{Method: "Target.targetCreated", Params: json.RawMessage(`{"targetInfo":{"targetId":"sentinel","type":"page"}}`)})
			}
			_ = ws.WriteJSON(wireMessage{ID: request.ID, Result: json.RawMessage(`{"targetInfos":[]}`)})
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, err := Start(ctx, Config{WebSocketURL: "ws" + strings.TrimPrefix(server.URL, "http")})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	b.Connection.mu.Lock()
	subscriptions := len(b.Connection.handlers)
	b.Connection.mu.Unlock()
	if subscriptions != 1 {
		t.Fatalf("target lifecycle split across %d subscriptions", subscriptions)
	}
	if err := poll(ctx, func() (bool, error) {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.targets["sentinel"] != nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.targets) != 1 || !reflect.DeepEqual(b.order, []string{"sentinel"}) {
		t.Fatalf("destroyed targets resurrected: targets=%d order=%v", len(b.targets), b.order)
	}
}

func TestTargetQueriesDoNotResurrectDestroyedTargets(t *testing.T) {
	for _, method := range []string{"Target.getTargets", "Target.getTargetInfo"} {
		t.Run(method, func(t *testing.T) {
			ready, release := make(chan struct{}), make(chan struct{})
			defer close(release)
			upgrader := websocket.Upgrader{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ws, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer ws.Close()
				var request wireMessage
				if ws.ReadJSON(&request) != nil {
					return
				}
				close(ready)
				<-release
				result := `{"targetInfo":{"targetId":"closed","type":"page"}}`
				if method == "Target.getTargets" {
					result = `{"targetInfos":[{"targetId":"closed","type":"page"},{"targetId":"alive","type":"page"}]}`
				}
				_ = ws.WriteJSON(wireMessage{ID: request.ID, Result: json.RawMessage(result)})
				// Keep the connection alive until the client closes it.
				_, _, _ = ws.ReadMessage()
			}))
			t.Cleanup(server.Close)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			c, err := Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"))
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			b := &Browser{Connection: c, targets: make(map[string]*Tab)}
			finished := make(chan error, 1)
			go func() {
				if method == "Target.getTargets" {
					finished <- b.UpdateTargets(ctx)
				} else {
					_, err := b.Tab(ctx, "closed")
					finished <- err
				}
			}()
			select {
			case <-ready:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			b.handleTarget(Event{Method: "Target.targetDestroyed", Params: json.RawMessage(`{"targetId":"closed"}`)})
			release <- struct{}{}
			err = <-finished
			if method == "Target.getTargets" && err != nil || method == "Target.getTargetInfo" && !errors.Is(err, ErrClosed) {
				t.Fatalf("unexpected query result: %v", err)
			}
			b.mu.Lock()
			defer b.mu.Unlock()
			if b.targets["closed"] != nil || len(b.targetQueries) != 0 {
				t.Fatal("stale target resurrected or query history retained")
			}
			if method == "Target.getTargets" && b.targets["alive"] == nil {
				t.Fatal("live target omitted from snapshot")
			}
		})
	}
}

func testUnresponsiveConnection(t *testing.T) *Connection {
	t.Helper()
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		for {
			var request wireMessage
			if ws.ReadJSON(&request) != nil {
				return
			}
			if request.Method != "Never.reply" {
				_ = ws.WriteJSON(wireMessage{ID: request.ID, Result: json.RawMessage(`{}`)})
			}
		}
	}))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestSendWithoutDeadlineTimesOut(t *testing.T) {
	c := testUnresponsiveConnection(t)
	started := time.Now()
	result := make(chan error, 1)
	go func() { result <- c.Send(context.Background(), "Never.reply", nil, nil) }()
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	case <-time.After(DefaultSendTimeout + 3*time.Second):
		t.Fatal("command without a deadline remained blocked")
	}
	if time.Since(started) < DefaultSendTimeout-250*time.Millisecond {
		t.Fatal("fallback timeout expired too early")
	}
	c.transport.mu.Lock()
	pending := len(c.transport.pending)
	c.transport.mu.Unlock()
	if pending != 0 {
		t.Fatalf("timed out command retained %d pending calls", pending)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Send(ctx, "Echo", nil, nil); err != nil {
		t.Fatalf("timeout broke the connection: %v", err)
	}
}

func TestSendCanCancelWhileWaitingForWriter(t *testing.T) {
	c := testUnresponsiveConnection(t)
	c.transport.writeLock <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- c.Send(ctx, "Echo", nil, nil) }()
	select {
	case err := <-finished:
		<-c.transport.writeLock
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		<-c.transport.writeLock
		t.Fatal("writer queue ignored cancellation")
	}
	if err := c.Send(context.Background(), "Echo", nil, nil); err != nil {
		t.Fatal(err)
	}
}
