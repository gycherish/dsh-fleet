// Package users owns console accounts and browser sessions.
//
// This is the authentication layer dsh deliberately does not provide. Its own
// documentation calls the `/api` fence "a reachability policy, not
// authentication", so everything reachable through this control plane is
// behind the checks in this package.
package users

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gycherish/dsh-fleet/internal/auth"
)

// SessionLifetime is how long a console login stays valid.
const SessionLifetime = 30 * 24 * time.Hour

// ErrBadCredentials reports a failed login. Unknown user and wrong password
// deliberately collapse into one error so a caller cannot enumerate accounts.
var ErrBadCredentials = errors.New("users: invalid username or password")

// ErrNoSession reports a missing, expired, or revoked browser session.
var ErrNoSession = errors.New("users: no valid session")

// ErrExists reports a duplicate username.
var ErrExists = errors.New("users: username is already taken")

// User is one console account.
type User struct {
	ID        uuid.UUID
	Username  string
	IsAdmin   bool
	CreatedAt time.Time
}

// Store is the users and user_sessions tables.
type Store struct{ pool *pgxpool.Pool }

// New returns a Store over pool.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// EnsureBootstrapAdmin creates the first account when none exists.
//
// It is a no-op once any account is present, which is why the compose file can
// keep DSHF_ADMIN_PASSWORD set without it silently becoming a permanent
// backdoor: after first boot the value is never consulted again.
func (s *Store) EnsureBootstrapAdmin(ctx context.Context, username, password string) (bool, error) {
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&count); err != nil {
		return false, fmt.Errorf("users: cannot count accounts: %w", err)
	}
	if count > 0 {
		return false, nil
	}
	if _, err := s.Create(ctx, username, password, true); err != nil {
		return false, err
	}
	return true, nil
}

// Create adds an account.
func (s *Store) Create(ctx context.Context, username, password string, admin bool) (*User, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, errors.New("users: username must not be blank")
	}
	if len(password) < 8 {
		return nil, errors.New("users: password must be at least 8 characters")
	}
	hash, err := auth.Hash(password)
	if err != nil {
		return nil, err
	}
	const q = `
		INSERT INTO users (username, password_hash, is_admin)
		VALUES ($1, $2, $3)
		RETURNING id, username, is_admin, created_at`
	var u User
	err = s.pool.QueryRow(ctx, q, username, hash, admin).Scan(&u.ID, &u.Username, &u.IsAdmin, &u.CreatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: %s", ErrExists, username)
		}
		return nil, fmt.Errorf("users: cannot create %q: %w", username, err)
	}
	return &u, nil
}

// Authenticate verifies a username and password.
func (s *Store) Authenticate(ctx context.Context, username, password string) (*User, error) {
	const q = `
		SELECT id, username, password_hash, is_admin, created_at, disabled_at
		FROM users WHERE lower(username) = lower($1)`
	var u User
	var hash string
	var disabledAt *time.Time
	switch err := s.pool.QueryRow(ctx, q, strings.TrimSpace(username)).
		Scan(&u.ID, &u.Username, &hash, &u.IsAdmin, &u.CreatedAt, &disabledAt); {
	case errors.Is(err, pgx.ErrNoRows):
		// Still pay the hashing cost so a missing account is not measurably
		// faster to reject than a wrong password.
		_, _ = auth.Hash(password)
		return nil, ErrBadCredentials
	case err != nil:
		return nil, fmt.Errorf("users: cannot read %q: %w", username, err)
	}
	if disabledAt != nil {
		return nil, ErrBadCredentials
	}
	ok, err := auth.Verify(password, hash)
	if err != nil {
		return nil, fmt.Errorf("users: cannot verify %q: %w", username, err)
	}
	if !ok {
		return nil, ErrBadCredentials
	}
	return &u, nil
}

// StartSession issues a browser session and returns its cookie value.
//
// Only the SHA-256 of that value is stored. Session tokens are high-entropy,
// so a fast hash is the right trade here: a stolen database dump must not
// yield live sessions, but there is no low-entropy secret to slow down.
func (s *Store) StartSession(ctx context.Context, userID uuid.UUID, userAgent string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("users: cannot read entropy: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))

	const q = `
		INSERT INTO user_sessions (user_id, token_hash, expires_at, user_agent)
		VALUES ($1, $2, now() + $3::interval, NULLIF($4, ''))`
	_, err := s.pool.Exec(ctx, q, userID, sum[:], SessionLifetime.String(), truncate(userAgent, 400))
	if err != nil {
		return "", fmt.Errorf("users: cannot start session: %w", err)
	}
	return token, nil
}

// LookupSession resolves a cookie value to its account.
func (s *Store) LookupSession(ctx context.Context, token string) (*User, error) {
	if token == "" {
		return nil, ErrNoSession
	}
	sum := sha256.Sum256([]byte(token))
	const q = `
		SELECT s.token_hash, u.id, u.username, u.is_admin, u.created_at
		FROM user_sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1 AND s.expires_at > now() AND u.disabled_at IS NULL`
	var stored []byte
	var u User
	switch err := s.pool.QueryRow(ctx, q, sum[:]).Scan(&stored, &u.ID, &u.Username, &u.IsAdmin, &u.CreatedAt); {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, ErrNoSession
	case err != nil:
		return nil, fmt.Errorf("users: cannot read session: %w", err)
	}
	// The index lookup already matched, but comparing in constant time keeps
	// the code honest if the query ever grows a range or prefix condition.
	if subtle.ConstantTimeCompare(stored, sum[:]) != 1 {
		return nil, ErrNoSession
	}

	// Best effort: a failed touch must not fail the request it belongs to.
	go func() {
		touchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, _ = s.pool.Exec(touchCtx, `UPDATE user_sessions SET last_seen_at = now() WHERE token_hash = $1`, sum[:])
	}()

	return &u, nil
}

// EndSession revokes one browser session.
func (s *Store) EndSession(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	sum := sha256.Sum256([]byte(token))
	if _, err := s.pool.Exec(ctx, `DELETE FROM user_sessions WHERE token_hash = $1`, sum[:]); err != nil {
		return fmt.Errorf("users: cannot end session: %w", err)
	}
	return nil
}

// PurgeExpiredSessions deletes sessions past their expiry.
func (s *Store) PurgeExpiredSessions(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM user_sessions WHERE expires_at <= now()`)
	if err != nil {
		return 0, fmt.Errorf("users: cannot purge sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}

// List returns every account in creation order.
func (s *Store) List(ctx context.Context) ([]User, error) {
	const q = `SELECT id, username, is_admin, created_at FROM users ORDER BY created_at`
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("users: cannot list: %w", err)
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.IsAdmin, &u.CreatedAt); err != nil {
			return nil, fmt.Errorf("users: cannot scan row: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}
