package users

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gycherish/dsh-fleet/internal/auth"
)

// ErrBadToken reports a token that is unknown, revoked, or does not belong to
// the presented account.
//
// One error for all three on purpose: a caller holding a wrong token learns
// only that it is wrong, not whether the account exists or the token used to.
var ErrBadToken = errors.New("users: token is not valid for that account")

// Token is one credential a person holds.
type Token struct {
	ID         uuid.UUID
	Name       string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

// MintToken issues a token for one account and returns the plaintext once.
//
// Named rather than anonymous so the list is legible months later: "laptop"
// and "the one on the NAS" are the difference between revoking confidently and
// revoking everything.
func (s *Store) MintToken(ctx context.Context, userID uuid.UUID, name string) (string, error) {
	label := strings.TrimSpace(name)
	if label == "" {
		return "", errors.New("users: a token needs a name")
	}
	token, err := auth.NewUserToken()
	if err != nil {
		return "", err
	}
	hash, err := auth.Hash(token)
	if err != nil {
		return "", err
	}
	const q = `INSERT INTO user_tokens (user_id, name, token_hash) VALUES ($1, $2, $3)`
	if _, err := s.pool.Exec(ctx, q, userID, label, hash); err != nil {
		return "", fmt.Errorf("users: cannot mint a token: %w", err)
	}
	return token, nil
}

// ListTokens returns one account's tokens, newest first.
func (s *Store) ListTokens(ctx context.Context, userID uuid.UUID) ([]Token, error) {
	const q = `
		SELECT id, name, created_at, last_used_at, revoked_at
		FROM user_tokens WHERE user_id = $1 ORDER BY created_at DESC`
	rows, err := s.pool.Query(ctx, q, userID)
	if err != nil {
		return nil, fmt.Errorf("users: cannot list tokens: %w", err)
	}
	defer rows.Close()

	var out []Token
	for rows.Next() {
		var t Token
		if err := rows.Scan(&t.ID, &t.Name, &t.CreatedAt, &t.LastUsedAt, &t.RevokedAt); err != nil {
			return nil, fmt.Errorf("users: cannot scan a token: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeToken withdraws one token, which disconnects every machine enrolled
// with it at that machine's next handshake.
//
// Scoped to the owner so one account cannot revoke another's by guessing an id.
func (s *Store) RevokeToken(ctx context.Context, userID, tokenID uuid.UUID) error {
	const q = `UPDATE user_tokens SET revoked_at = now()
	            WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`
	tag, err := s.pool.Exec(ctx, q, tokenID, userID)
	if err != nil {
		return fmt.Errorf("users: cannot revoke a token: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrBadToken
	}
	return nil
}

// AuthenticateToken verifies a username and user token, and reports the owner.
//
// Every live token for that account is tried, because the presented plaintext
// carries nothing that identifies which row it is. An account's token list is
// short by nature; if that ever stops being true, a lookup key belongs in the
// token itself rather than a wider scan here.
func (s *Store) AuthenticateToken(ctx context.Context, username, token string) (*User, error) {
	user, err := s.byUsername(ctx, username)
	if err != nil {
		return nil, err
	}

	const q = `SELECT id, token_hash FROM user_tokens
	            WHERE user_id = $1 AND revoked_at IS NULL`
	rows, err := s.pool.Query(ctx, q, user.ID)
	if err != nil {
		return nil, fmt.Errorf("users: cannot read tokens: %w", err)
	}
	defer rows.Close()

	type candidate struct {
		id   uuid.UUID
		hash string
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.hash); err != nil {
			return nil, fmt.Errorf("users: cannot scan a token: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for _, c := range candidates {
		ok, err := auth.Verify(token, c.hash)
		if err != nil {
			return nil, fmt.Errorf("users: cannot verify a token: %w", err)
		}
		if ok {
			s.touchToken(ctx, c.id)
			return user, nil
		}
	}
	return nil, ErrBadToken
}

// touchToken records use, best effort: a failure here must not refuse a node
// that presented a valid credential.
func (s *Store) touchToken(ctx context.Context, id uuid.UUID) {
	const q = `UPDATE user_tokens SET last_used_at = now() WHERE id = $1`
	if _, err := s.pool.Exec(ctx, q, id); err != nil {
		s.log.Warn("cannot record token use", "err", err)
	}
}

// Find resolves an account by name, for the operator commands.
func (s *Store) Find(ctx context.Context, username string) (*User, error) {
	user, err := s.byUsername(ctx, username)
	if errors.Is(err, ErrBadToken) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, username)
	}
	return user, err
}

func (s *Store) byUsername(ctx context.Context, username string) (*User, error) {
	const q = `SELECT id, username, is_admin, created_at, disabled_at
	            FROM users WHERE lower(username) = lower($1)`
	var u User
	switch err := s.pool.QueryRow(ctx, q, strings.TrimSpace(username)).
		Scan(&u.ID, &u.Username, &u.IsAdmin, &u.CreatedAt, &u.DisabledAt); {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, ErrBadToken
	case err != nil:
		return nil, fmt.Errorf("users: cannot read %q: %w", username, err)
	}
	if u.DisabledAt != nil {
		return nil, ErrBadToken
	}
	return &u, nil
}
