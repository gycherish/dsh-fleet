// Package nodes owns the registered-machine table: registration, token
// verification, and the facts a connected node reports about itself.
package nodes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

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
	// A blank label leaves the stored one alone rather than erasing it.
	const q = `UPDATE nodes
	              SET token_hash = $2,
	                  label = COALESCE(NULLIF($3, ''), label),
	                  revoked_at = NULL
	            WHERE id = $1`
	tag, err := s.pool.Exec(ctx, q, id, hash, label)
	if err != nil {
		return "", fmt.Errorf("nodes: cannot rotate %q: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return "", ErrNotFound
	}
	return token, nil
}

// Authenticate verifies a presented token against the stored hash.
//
// A revoked node and an unknown id are reported as distinct errors: the
// uplink maps them onto different close codes so an operator reading a node's
// log can tell "I deleted you" from "your token is wrong".
func (s *Store) Authenticate(ctx context.Context, id, token string) error {
	const q = `SELECT token_hash, revoked_at FROM nodes WHERE id = $1`
	var hash string
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
	ok, err := auth.Verify(token, hash)
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
