// Package audit records what the privilege gate decided.
//
// This exists because a custom carrier bypasses dsh's own loopback pin on the
// privileged method set, so this project owns that boundary. Owning a
// boundary means being able to show what crossed it.
package audit

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Recorder writes gate decisions without blocking the request that produced
// them.
type Recorder struct {
	pool *pgxpool.Pool
	log  *slog.Logger
	// queue is bounded: an audit backlog must cost memory that stops growing,
	// not memory that grows until the process dies.
	queue chan entry
}

type entry struct {
	nodeID  string
	ns      string
	path    string
	allowed bool
	status  int
	reason  string
}

// New starts a Recorder and returns it with its stop function.
func New(pool *pgxpool.Pool, log *slog.Logger) (*Recorder, func()) {
	r := &Recorder{pool: pool, log: log, queue: make(chan entry, 1024)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go r.drain(ctx, done)
	return r, func() {
		cancel()
		<-done
	}
}

// RecordDecision enqueues one decision.
//
// A full queue drops the entry with a warning rather than stalling a browser
// request. Losing an audit row under load is bad; wedging the console is worse,
// and the dropped-count warning makes the loss visible.
func (r *Recorder) RecordDecision(nodeID, ns, path string, allowed bool, status int, reason string) {
	select {
	case r.queue <- entry{nodeID, ns, path, allowed, status, reason}:
	default:
		r.log.Warn("audit: queue full, decision dropped", "node", nodeID, "path", path)
	}
}

func (r *Recorder) drain(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	for {
		select {
		case <-ctx.Done():
			// Flush what is already queued so a clean shutdown does not lose
			// the decisions that led up to it.
			for {
				select {
				case e := <-r.queue:
					r.write(context.WithoutCancel(ctx), e)
				default:
					return
				}
			}
		case e := <-r.queue:
			r.write(ctx, e)
		}
	}
}

func (r *Recorder) write(ctx context.Context, e entry) {
	writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	const q = `
		INSERT INTO audit_log (node_id, ns, path, allowed, status, reason)
		VALUES ($1, $2, $3, $4, NULLIF($5, 0), NULLIF($6, ''))`
	if _, err := r.pool.Exec(writeCtx, q, e.nodeID, e.ns, e.path, e.allowed, e.status, e.reason); err != nil {
		r.log.Warn("audit: cannot record decision", "err", err)
	}
}
