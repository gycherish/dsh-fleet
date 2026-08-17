package uplink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/gycherish/dsh-fleet/pkg/envelope"
)

// Tunable connection parameters. These are the values offered in `welcome`,
// so a node adopts them for the lifetime of its connection.
const (
	heartbeatInterval = 20 * time.Second
	maxChunkBytes     = 256 * 1024
	telemetryInterval = 30 * time.Second

	// A single slow reader can stall this many chunks before it starts holding
	// up the whole multiplexed connection. See the note on chunk delivery below.
	streamBuffer = 32

	// Bound on one inbound frame. The largest legitimate frame is a data chunk
	// at maxChunkBytes, inflated by base64 and JSON escaping.
	readLimit = 2 * 1024 * 1024
)

// Response is one node answer: the status line plus a streaming body.
type Response struct {
	Status  int
	Headers map[string]string
	// Body streams the node's response. The caller must always close it;
	// closing early cancels the request on the node.
	Body io.ReadCloser
}

// RequestError is a node-reported failure carrying the protocol's error code.
type RequestError struct {
	Code    string
	Message string
}

func (e *RequestError) Error() string { return fmt.Sprintf("node error (%s): %s", e.Code, e.Message) }

// pending is one in-flight request's delivery state.
type pending struct {
	head   chan *Response
	fail   chan *RequestError
	chunks chan []byte
	// nextSeq enforces the protocol's ordering rule. Silently tolerating a gap
	// would corrupt an SSE stream in a way that only surfaces later as UI
	// desync, so a violation kills the connection instead.
	nextSeq int
	once    sync.Once
}

func (p *pending) closeChunks() { p.once.Do(func() { close(p.chunks) }) }

// Conn is one authenticated node's live connection.
type Conn struct {
	NodeID string
	Caps   []string

	ws     *websocket.Conn
	log    *slog.Logger
	sink   TelemetrySink
	writeM sync.Mutex

	mu      sync.Mutex
	pending map[string]*pending
	bridges map[string]*Bridge
	closed  bool

	nextID   uint64
	lastPong time.Time
	done     chan struct{}
}

// TelemetrySink receives what a node reports about itself.
type TelemetrySink interface {
	RecordTelemetry(ctx context.Context, nodeID string, ts time.Time, snapshot json.RawMessage) error
	Touch(ctx context.Context, nodeID string) error
}

func newConn(ws *websocket.Conn, nodeID string, caps []string, sink TelemetrySink, log *slog.Logger) *Conn {
	return &Conn{
		NodeID:   nodeID,
		Caps:     caps,
		ws:       ws,
		log:      log.With("node", nodeID),
		sink:     sink,
		pending:  map[string]*pending{},
		bridges:  map[string]*Bridge{},
		lastPong: time.Now(),
		done:     make(chan struct{}),
	}
}

/*
Bridge is one browser WebSocket relayed to the node's own server.

Inbound messages arrive on Messages; the channel closes when either end hangs
up. Send is safe from one goroutine at a time, which is what the single
browser reader provides.
*/
type Bridge struct {
	conn *Conn
	id   string
	up   chan string
	// Bounded for the same reason response chunks are: one stalled reader
	// costs slack rather than the whole multiplexed connection.
	Messages chan string
	once     sync.Once
}

// Send relays one message from the browser toward the node.
func (b *Bridge) Send(ctx context.Context, text string) error {
	return b.conn.write(ctx, envelope.WsMsg{
		T: envelope.TWsMsg, ID: b.id,
		Body: envelope.NewPayload([]byte(text), true),
	})
}

// Close tears the bridge down; calling it twice is a no-op.
func (b *Bridge) Close(code int, reason string) {
	b.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = b.conn.write(ctx, envelope.WsClose{T: envelope.TWsClose, ID: b.id, Code: code, Reason: reason})
		b.conn.dropBridge(b.id)
	})
}

// OpenWS asks the node to dial one WebSocket and bridge it back.
//
// It returns once the node reports the socket connected, so a caller that has
// already accepted the browser's upgrade knows whether it can keep it.
func (c *Conn) OpenWS(ctx context.Context, path string, headers map[string]string) (*Bridge, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errors.New("uplink: node is not connected")
	}
	c.nextID++
	id := fmt.Sprintf("w%d", c.nextID)
	bridge := &Bridge{conn: c, id: id, up: make(chan string, 1), Messages: make(chan string, streamBuffer)}
	c.bridges[id] = bridge
	c.mu.Unlock()

	frame := envelope.WsOpen{T: envelope.TWsOpen, ID: id, Path: path, Headers: headers}
	if err := c.write(ctx, frame); err != nil {
		c.dropBridge(id)
		return nil, err
	}

	select {
	case <-ctx.Done():
		bridge.Close(1001, "cancelled")
		return nil, ctx.Err()
	case <-c.done:
		c.dropBridge(id)
		return nil, errors.New("uplink: node disconnected")
	case <-bridge.up:
		return bridge, nil
	case <-time.After(15 * time.Second):
		bridge.Close(1011, "node did not open the socket")
		return nil, errors.New("uplink: node did not open the socket in time")
	}
}

func (c *Conn) dropBridge(id string) {
	c.mu.Lock()
	bridge := c.bridges[id]
	delete(c.bridges, id)
	c.mu.Unlock()
	if bridge != nil {
		// Closing wakes a reader blocked on Messages, which is how the browser
		// side learns the far end went away.
		close(bridge.Messages)
	}
}

// Done closes when the connection ends.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Do sends one request and waits for its status line.
//
// It returns as soon as the node answers `head`, so a streaming response is
// forwarded to the browser as it arrives rather than after it completes. The
// caller owns Response.Body and must close it.
func (c *Conn) Do(ctx context.Context, ns, method, path string, headers map[string]string, body []byte, textual bool) (*Response, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errors.New("uplink: node is not connected")
	}
	c.nextID++
	id := fmt.Sprintf("%d", c.nextID)
	p := &pending{
		head:   make(chan *Response, 1),
		fail:   make(chan *RequestError, 1),
		chunks: make(chan []byte, streamBuffer),
	}
	c.pending[id] = p
	c.mu.Unlock()

	req := envelope.Req{
		T: envelope.TReq, ID: id, Ns: ns, Method: method, Path: path, Headers: headers,
	}
	if body != nil {
		payload := envelope.NewPayload(body, textual)
		req.Body = &payload
	}
	if err := c.write(ctx, req); err != nil {
		c.settle(id)
		return nil, err
	}

	select {
	case <-ctx.Done():
		// Tell the node to stop working; it still answers with err/cancelled,
		// which is what frees the correlation slot.
		c.cancel(id)
		return nil, ctx.Err()
	case <-c.done:
		c.settle(id)
		return nil, errors.New("uplink: node disconnected")
	case e := <-p.fail:
		c.settle(id)
		return nil, e
	case resp := <-p.head:
		pr, pw := io.Pipe()
		resp.Body = &bodyReader{r: pr, cancel: func() { c.cancel(id) }}
		go c.pump(id, p, pw)
		return resp, nil
	}
}

// pump moves delivered chunks into the response pipe.
//
// It runs per request so the connection's read loop is not blocked by one slow
// consumer -- up to streamBuffer chunks of slack. Beyond that the read loop
// does block, which is the deliberate trade: the protocol has no per-stream
// flow control, and dropping a chunk would corrupt the stream.
func (c *Conn) pump(id string, p *pending, pw *io.PipeWriter) {
	defer c.settle(id)
	for chunk := range p.chunks {
		if chunk == nil {
			_ = pw.Close()
			return
		}
		if _, err := pw.Write(chunk); err != nil {
			// The consumer went away; cancelling stops the node producing more.
			c.cancel(id)
			_ = pw.CloseWithError(err)
			return
		}
	}
	_ = pw.CloseWithError(errors.New("uplink: node disconnected mid-response"))
}

// serve runs the read loop until the connection ends.
func (c *Conn) serve(ctx context.Context) error {
	defer c.shutdown()

	go c.heartbeat(ctx)

	for {
		_, raw, err := c.ws.Read(ctx)
		if err != nil {
			return err
		}
		kind, err := envelope.Discriminant(raw)
		if err != nil {
			return fmt.Errorf("uplink: %w", err)
		}
		if err := c.dispatch(ctx, kind, raw); err != nil {
			return err
		}
	}
}

func (c *Conn) dispatch(ctx context.Context, kind string, raw []byte) error {
	switch kind {
	case envelope.THead:
		var f envelope.Head
		if err := json.Unmarshal(raw, &f); err != nil {
			return fmt.Errorf("uplink: bad head frame: %w", err)
		}
		if p := c.lookup(f.ID); p != nil {
			p.head <- &Response{Status: f.Status, Headers: f.Headers}
		}

	case envelope.TData:
		var f envelope.Data
		if err := json.Unmarshal(raw, &f); err != nil {
			return fmt.Errorf("uplink: bad data frame: %w", err)
		}
		p := c.lookup(f.ID)
		if p == nil {
			return nil // already settled; a late chunk is not fatal
		}
		if f.Seq != p.nextSeq {
			return fmt.Errorf("uplink: out-of-order chunk on %s: want seq %d, got %d", f.ID, p.nextSeq, f.Seq)
		}
		p.nextSeq++
		bytes, err := f.Body.Bytes()
		if err != nil {
			return fmt.Errorf("uplink: %w", err)
		}
		select {
		case p.chunks <- bytes:
		case <-ctx.Done():
			return ctx.Err()
		}

	case envelope.TEnd:
		var f envelope.End
		if err := json.Unmarshal(raw, &f); err != nil {
			return fmt.Errorf("uplink: bad end frame: %w", err)
		}
		if p := c.lookup(f.ID); p != nil {
			if f.Chunks != p.nextSeq {
				return fmt.Errorf("uplink: chunk count mismatch on %s: saw %d, node claims %d", f.ID, p.nextSeq, f.Chunks)
			}
			select {
			case p.chunks <- nil:
			case <-ctx.Done():
				return ctx.Err()
			}
		}

	case envelope.TErr:
		var f envelope.Err
		if err := json.Unmarshal(raw, &f); err != nil {
			return fmt.Errorf("uplink: bad err frame: %w", err)
		}
		p := c.lookup(f.ID)
		if p == nil {
			return nil
		}
		re := &RequestError{Code: f.Code, Message: f.Message}
		select {
		case p.fail <- re:
		default:
			// head already delivered: the failure belongs to the body stream.
			p.closeChunks()
		}

	case envelope.TTelemetry:
		var f envelope.Telemetry
		if err := json.Unmarshal(raw, &f); err != nil {
			return fmt.Errorf("uplink: bad telemetry frame: %w", err)
		}
		// Persistence must not stall the frame loop or kill the connection:
		// telemetry is diagnostic, and a database hiccup is not a node fault.
		go func() {
			writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			if err := c.sink.RecordTelemetry(writeCtx, c.NodeID, time.UnixMilli(f.Ts), f.Snapshot); err != nil {
				c.log.Warn("cannot persist telemetry", "err", err)
			}
		}()

	case envelope.TWsUp:
		var f envelope.WsUp
		if err := json.Unmarshal(raw, &f); err != nil {
			return fmt.Errorf("uplink: bad ws.up frame: %w", err)
		}
		c.mu.Lock()
		bridge := c.bridges[f.ID]
		c.mu.Unlock()
		if bridge != nil {
			select {
			case bridge.up <- f.Protocol:
			default: // already reported
			}
		}

	case envelope.TWsMsg:
		var f envelope.WsMsg
		if err := json.Unmarshal(raw, &f); err != nil {
			return fmt.Errorf("uplink: bad ws.msg frame: %w", err)
		}
		c.mu.Lock()
		bridge := c.bridges[f.ID]
		c.mu.Unlock()
		if bridge == nil {
			return nil // closed while in flight
		}
		text, err := f.Body.Bytes()
		if err != nil {
			return fmt.Errorf("uplink: %w", err)
		}
		select {
		case bridge.Messages <- string(text):
		case <-ctx.Done():
			return ctx.Err()
		}

	case envelope.TWsClose:
		var f envelope.WsClose
		if err := json.Unmarshal(raw, &f); err != nil {
			return fmt.Errorf("uplink: bad ws.close frame: %w", err)
		}
		c.dropBridge(f.ID)

	case envelope.TPong:
		c.mu.Lock()
		c.lastPong = time.Now()
		c.mu.Unlock()

	default:
		// Forward compatibility: a newer node may add frames. Ignoring keeps an
		// additive protocol change from becoming a breaking one.
		c.log.Warn("ignoring unknown frame", "t", kind)
	}
	return nil
}

func (c *Conn) heartbeat(ctx context.Context) {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return
		case <-ticker.C:
			c.mu.Lock()
			silent := time.Since(c.lastPong)
			c.mu.Unlock()
			if silent > 2*heartbeatInterval+5*time.Second {
				c.log.Warn("heartbeat lost; closing", "silentFor", silent.String())
				_ = c.ws.Close(websocket.StatusGoingAway, "heartbeat lost")
				return
			}
			if err := c.write(ctx, envelope.Ping{T: envelope.TPing, Ts: time.Now().UnixMilli()}); err != nil {
				return
			}
			touchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			if err := c.sink.Touch(touchCtx, c.NodeID); err != nil {
				c.log.Warn("cannot touch node", "err", err)
			}
			cancel()
		}
	}
}

func (c *Conn) cancel(id string) {
	// Best effort with its own deadline: the caller's context is usually the
	// one that just expired.
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	_ = c.write(ctx, envelope.Cancel{T: envelope.TCancel, ID: id})
}

func (c *Conn) write(ctx context.Context, frame any) error {
	raw, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("uplink: cannot encode frame: %w", err)
	}
	// One writer at a time: websocket.Conn forbids concurrent writes, and both
	// the heartbeat and every request goroutine reach this method.
	c.writeM.Lock()
	defer c.writeM.Unlock()
	return c.ws.Write(ctx, websocket.MessageText, raw)
}

func (c *Conn) lookup(id string) *pending {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pending[id]
}

func (c *Conn) settle(id string) {
	c.mu.Lock()
	p := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if p != nil {
		p.closeChunks()
	}
}

func (c *Conn) shutdown() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	live := make([]*pending, 0, len(c.pending))
	for id, p := range c.pending {
		live = append(live, p)
		delete(c.pending, id)
	}
	bridges := make([]*Bridge, 0, len(c.bridges))
	for id, b := range c.bridges {
		bridges = append(bridges, b)
		delete(c.bridges, id)
	}
	c.mu.Unlock()

	for _, b := range bridges {
		close(b.Messages)
	}

	// Correlation ids are per-connection, so nothing in flight can survive a
	// reconnect. Failing them now beats waiting for a reply that cannot come.
	for _, p := range live {
		p.closeChunks()
	}
	close(c.done)
}

// bodyReader adapts the response pipe so closing it cancels the node request.
type bodyReader struct {
	r      *io.PipeReader
	cancel func()
	once   sync.Once
}

func (b *bodyReader) Read(p []byte) (int, error) { return b.r.Read(p) }

func (b *bodyReader) Close() error {
	b.once.Do(func() {
		b.cancel()
		_ = b.r.Close()
	})
	return nil
}
