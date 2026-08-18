// Package nodes owns the registered-machine table: registration, token
// verification, and the facts a connected node reports about itself.
package nodes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gycherish/dsh-fleet/internal/auth"
	"github.com/gycherish/dsh-fleet/pkg/envelope"
)

// ErrNotFound reports an unregistered node id.
var ErrNotFound = errors.New("nodes: no such node")

// ErrRevoked reports a node whose token has been withdrawn.
var ErrRevoked = errors.New("nodes: node token is revoked")

// ErrExists reports a duplicate registration.
var ErrExists = errors.New("nodes: node id is already registered")

// Node is one registered machine.
type Node struct {
	ID            string
	Label         string
	CreatedAt     time.Time
	RevokedAt     *time.Time
	DSHVersion    string
	PluginVersion string
	Platform      string
	Arch          string
	Cwd           string
	LastSeenAt    *time.Time
	Snapshot      json.RawMessage
}

// Store is the nodes table.
type Store struct{ pool *pgxpool.Pool }

// New returns a Store over pool.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Register creates a node and returns its one-time plaintext token.
//
// The token is generated here rather than accepted from the caller so no code
// path can register a node with a weak or reused secret.
func (s *Store) Register(ctx context.Context, id, label string) (string, error) {
	token, err := auth.NewNodeToken()
	if err != nil {
		return "", err
	}
	hash, err := auth.Hash(token)
	if err != nil {
		return "", err
	}
	const q = `INSERT INTO nodes (id, label, token_hash) VALUES ($1, NULLIF($2, ''), $3)`
	if _, err := s.pool.Exec(ctx, q, id, label, hash); err != nil {
		if isUniqueViolation(err) {
			return "", fmt.Errorf("%w: %s", ErrExists, id)
		}
		return "", fmt.Errorf("nodes: cannot register %q: %w", id, err)
	}
	return token, nil
}

// Rotate issues a fresh token for an existing node and lifts any revocation.
//
// Revoking is not deleting — the row keeps its telemetry history — so without
// this the operator who revokes a leaked token can never re-enrol that machine
// under its own name, which is precisely when they want to. Any live
// connection keeps running on the old token until it drops; the next handshake
// needs the new one.
func (s *Store) Rotate(ctx context.Context, id, label string) (string, error) {
	token, err := auth.NewNodeToken()
	if err != nil {
		return "", err
	}
	hash, err := auth.Hash(token)
	if err != nil {
		return "", err
	}
	// A blank label leaves the stored one alone rather than erasing it. The
	// owner_id guard keeps this off a self-enrolled machine: giving it a node
	// token would leave it holding two credentials, which the schema forbids and
	// which would make "what authenticates this machine" a matter of reading
	// order. Such a machine is re-enrolled by its owner's token, not here.
	const q = `UPDATE nodes
	              SET token_hash = $2,
	                  label = COALESCE(NULLIF($3, ''), label),
	                  revoked_at = NULL
	            WHERE id = $1 AND owner_id IS NULL`
	tag, err := s.pool.Exec(ctx, q, id, hash, label)
	if err != nil {
		return "", fmt.Errorf("nodes: cannot rotate %q: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return "", ErrNotFound
	}
	return token, nil
}

// ErrForeign reports a machine id already claimed by another account.
var ErrForeign = errors.New("nodes: that machine id belongs to another account")

// ErrHasOwnToken reports a machine registered with `dshf node add`, which
// authenticates with a token of its own rather than a person's.
//
// Distinct from ErrForeign because the fix is different and the first version
// of this told people the wrong one: "belongs to another account" sends someone
// looking for a colleague to blame, when the answer is either to use that
// machine's own token or to pick a different name.
var ErrHasOwnToken = errors.New("nodes: that machine is registered with a machine token of its own")

// EnsureOwned registers a machine to an account, or confirms it already is.
//
// This is the self-enrolment path: a machine that presents its owner's token
// needs no prior registration, so the first connection creates the row. An id
// already held by somebody else is refused rather than taken over — otherwise
// guessing a colleague's machine name would be enough to impersonate it.
func (s *Store) EnsureOwned(ctx context.Context, id string, ownerID uuid.UUID, label string) error {
	const q = `
		INSERT INTO nodes (id, label, owner_id) VALUES ($1, NULLIF($2, ''), $3)
		ON CONFLICT (id) DO UPDATE
		   SET label      = COALESCE(nodes.label, NULLIF($2, '')),
		       revoked_at = NULL
		 WHERE nodes.owner_id = $3`
	tag, err := s.pool.Exec(ctx, q, id, label, ownerID)
	if err != nil {
		return fmt.Errorf("nodes: cannot enrol %q: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		// The row exists and the WHERE excluded it. Which of the two reasons it
		// was decides what the operator should do about it, so find out rather
		// than reporting the more alarming one.
		return s.whyNotClaimable(ctx, id)
	}
	return nil
}

// whyNotClaimable explains a refused claim on an existing machine.
func (s *Store) whyNotClaimable(ctx context.Context, id string) error {
	const q = `SELECT token_hash IS NOT NULL FROM nodes WHERE id = $1`
	var ownToken bool
	switch err := s.pool.QueryRow(ctx, q, id).Scan(&ownToken); {
	case errors.Is(err, pgx.ErrNoRows):
		// Raced with a delete; the next attempt creates it.
		return ErrNotFound
	case err != nil:
		return fmt.Errorf("nodes: cannot read %q: %w", id, err)
	}
	if ownToken {
		return ErrHasOwnToken
	}
	return ErrForeign
}

// Authenticate verifies a presented token against the stored hash.
//
// A revoked node and an unknown id are reported as distinct errors: the
// uplink maps them onto different close codes so an operator reading a node's
// log can tell "I deleted you" from "your token is wrong".
func (s *Store) Authenticate(ctx context.Context, id, token string) error {
	const q = `SELECT token_hash, revoked_at FROM nodes WHERE id = $1`
	// Nullable since self-enrolled machines hold no token of their own.
	var hash *string
	var revokedAt *time.Time
	switch err := s.pool.QueryRow(ctx, q, id).Scan(&hash, &revokedAt); {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrNotFound
	case err != nil:
		return fmt.Errorf("nodes: cannot read %q: %w", id, err)
	}
	if revokedAt != nil {
		return ErrRevoked
	}
	if hash == nil {
		// Owner-enrolled: it must present a user token, and presenting a node
		// token instead is as wrong as presenting the wrong one.
		return ErrNotFound
	}
	ok, err := auth.Verify(token, *hash)
	if err != nil {
		return fmt.Errorf("nodes: cannot verify %q: %w", id, err)
	}
	if !ok {
		return ErrNotFound
	}
	return nil
}

// RecordHello stores the facts a node reported at handshake and marks it seen.
func (s *Store) RecordHello(ctx context.Context, id string, d envelope.NodeDescriptor, caps []string) error {
	capsJSON, err := json.Marshal(caps)
	if err != nil {
		return fmt.Errorf("nodes: cannot encode caps: %w", err)
	}
	const q = `
		UPDATE nodes SET
			label          = COALESCE(label, NULLIF($2, '')),
			dsh_version    = $3,
			plugin_version = $4,
			platform       = $5,
			arch           = $6,
			cwd            = $7,
			caps           = $8,
			last_seen_at   = now()
		WHERE id = $1`
	// label uses COALESCE so an operator-set name survives a node that reports
	// its own; the node only supplies one when the console has none.
	_, err = s.pool.Exec(ctx, q, id, d.Label, d.DSHVersion, d.PluginVersion, d.Platform, d.Arch, d.Cwd, capsJSON)
	if err != nil {
		return fmt.Errorf("nodes: cannot record hello for %q: %w", id, err)
	}
	return nil
}

// Touch refreshes last_seen_at. Liveness is derived from this column rather
// than stored as a flag, so a control-plane crash cannot leave nodes stuck
// online.
func (s *Store) Touch(ctx context.Context, id string) error {
	const q = `UPDATE nodes SET last_seen_at = now() WHERE id = $1`
	if _, err := s.pool.Exec(ctx, q, id); err != nil {
		return fmt.Errorf("nodes: cannot touch %q: %w", id, err)
	}
	return nil
}

// RecordTelemetry appends one snapshot and denormalises it onto the node row.
func (s *Store) RecordTelemetry(ctx context.Context, id string, ts time.Time, snapshot json.RawMessage) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("nodes: cannot begin telemetry write: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const insert = `INSERT INTO node_telemetry (node_id, ts, snapshot) VALUES ($1, $2, $3)`
	if _, err := tx.Exec(ctx, insert, id, ts, snapshot); err != nil {
		return fmt.Errorf("nodes: cannot append telemetry for %q: %w", id, err)
	}
	const update = `UPDATE nodes SET latest_snapshot = $2, last_seen_at = now() WHERE id = $1`
	if _, err := tx.Exec(ctx, update, id, snapshot); err != nil {
		return fmt.Errorf("nodes: cannot update snapshot for %q: %w", id, err)
	}
	return tx.Commit(ctx)
}

// List returns every registered node in creation order.
func (s *Store) List(ctx context.Context) ([]Node, error) {
	const q = `
		SELECT id, COALESCE(label, ''), created_at, revoked_at,
		       COALESCE(dsh_version, ''), COALESCE(plugin_version, ''),
		       COALESCE(platform, ''), COALESCE(arch, ''), COALESCE(cwd, ''),
		       last_seen_at, latest_snapshot
		FROM nodes ORDER BY created_at`
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("nodes: cannot list: %w", err)
	}
	defer rows.Close()

	var out []Node
	for rows.Next() {
		var n Node
		if err := rows.Scan(
			&n.ID, &n.Label, &n.CreatedAt, &n.RevokedAt,
			&n.DSHVersion, &n.PluginVersion, &n.Platform, &n.Arch, &n.Cwd,
			&n.LastSeenAt, &n.Snapshot,
		); err != nil {
			return nil, fmt.Errorf("nodes: cannot scan row: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// Revoke withdraws a node's token without deleting its history.
func (s *Store) Revoke(ctx context.Context, id string) error {
	const q = `UPDATE nodes SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`
	tag, err := s.pool.Exec(ctx, q, id)
	if err != nil {
		return fmt.Errorf("nodes: cannot revoke %q: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}

// PruneTelemetry drops snapshots older than the retention window.
//
// The table is append-only at one row per node every thirty seconds — about a
// million rows per node per year, and eighteen megabytes in the first two days
// of a three-node development fleet. Nothing reads the history yet; the console
// reads the latest snapshot, which is denormalised onto the node row. So this
// exists to stop a table nobody queries from being the largest thing in the
// database.
//
// Deleted in batches so a first run against a long-neglected table does not
// take one long lock.
func (s *Store) PruneTelemetry(ctx context.Context, keep time.Duration) (int64, error) {
	const q = `
		DELETE FROM node_telemetry
		WHERE ctid IN (
			SELECT ctid FROM node_telemetry WHERE ts < now() - $1::interval LIMIT 10000
		)`
	var total int64
	for {
		tag, err := s.pool.Exec(ctx, q, keep.String())
		if err != nil {
			return total, fmt.Errorf("nodes: cannot prune telemetry: %w", err)
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < 10000 {
			return total, nil
		}
		// Yield between batches: a purge must never be why a request waits.
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		default:
		}
	}
}
