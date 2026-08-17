package uplink

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/gycherish/dsh-fleet/pkg/envelope"
)

type nopSink struct{}

func (nopSink) RecordTelemetry(context.Context, string, time.Time, json.RawMessage) error { return nil }
func (nopSink) Touch(context.Context, string) error                                       { return nil }

// pair returns a Conn wired to a bare peer socket standing in for the node.
func pair(t *testing.T) (*Conn, *websocket.Conn) {
	t.Helper()
	accepted := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ws.SetReadLimit(readLimit)
		accepted <- ws
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)

	peer, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	peer.SetReadLimit(readLimit)
	t.Cleanup(func() { _ = peer.CloseNow() })

	srv := <-accepted
	t.Cleanup(func() { _ = srv.CloseNow() })
	return newConn(srv, "devbox", nil, nopSink{}, slog.New(slog.NewTextHandler(io.Discard, nil))), peer
}

// A browser that goes away must not take the machine down with it.
//
// websocket.Write closes the whole connection when its context ends mid-frame,
// and Do was handing it the browser's request context. One visitor closing a
// tab therefore tore down the multiplexed uplink and every other session on
// that node — it dropped this node twice during one test run before the fix.
func TestCancelledRequestLeavesTheConnectionUsable(t *testing.T) {
	conn, peer := pair(t)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := conn.Do(cancelled, envelope.NsDSH, "POST", "/api/session.list", nil, []byte("{}"), true); err == nil {
		t.Fatal("a cancelled request should report its cancellation")
	}

	// Now prove the connection still carries traffic.
	go func() {
		_, _ = conn.Do(context.Background(), envelope.NsDSH, "POST", "/api/workspace.list", nil, []byte("{}"), true)
	}()

	deadline, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	for {
		_, data, err := peer.Read(deadline)
		if err != nil {
			t.Fatalf("the uplink died after a cancelled request: %v", err)
		}
		var frame map[string]any
		if err := json.Unmarshal(data, &frame); err != nil {
			t.Fatalf("unparseable frame: %v", err)
		}
		// The cancelled request's own req/cancel frames may still be queued.
		if frame["path"] == "/api/workspace.list" {
			return
		}
	}
}

// The heartbeat and a request share one writer, so a cancelled request must
// not leave the mutex or the socket in a state the next writer inherits.
func TestManyCancelledRequestsDoNotAccumulateDamage(t *testing.T) {
	conn, peer := pair(t)

	for i := 0; i < 25; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, _ = conn.Do(ctx, envelope.NsDSH, "POST", "/api/session.list", nil, []byte("{}"), true)
	}

	go func() {
		_, _ = conn.Do(context.Background(), envelope.NsDSH, "POST", "/api/workspace.list", nil, []byte("{}"), true)
	}()

	deadline, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	for {
		_, data, err := peer.Read(deadline)
		if err != nil {
			t.Fatalf("the uplink died after 25 cancelled requests: %v", err)
		}
		var frame map[string]any
		if err := json.Unmarshal(data, &frame); err != nil {
			continue
		}
		if frame["path"] == "/api/workspace.list" {
			return
		}
	}
}
