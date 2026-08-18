package audit

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Decision is one recorded gate decision, joined to the names behind its ids.
type Decision struct {
	At      time.Time
	User    string
	Node    string
	Ns      string
	Path    string
	Allowed bool
	Status  int
	Reason  string
}

// Filter narrows a read. Every field is optional; the zero value reads the
// most recent decisions across the whole fleet.
type Filter struct {
	Node string
	User string
	// DeniedOnly keeps the refusals, which is the question this log usually
	// gets asked: not "what happened" but "what was stopped".
	DeniedOnly bool
	Limit      int
}

// Recent reads the newest decisions first.
//
// Ids are resolved to names here rather than at the caller: an audit line
// naming a uuid is one the reader has to go and decode, which in practice
// means it does not get read.
func (r *Recorder) Recent(ctx context.Context, f Filter) ([]Decision, error) {
	where := []string{"TRUE"}
	args := []any{}
	if f.Node != "" {
		args = append(args, f.Node)
		where = append(where, fmt.Sprintf("a.node_id = $%d", len(args)))
	}
	if f.User != "" {
		args = append(args, f.User)
		where = append(where, fmt.Sprintf("lower(u.username) = lower($%d)", len(args)))
	}
	if f.DeniedOnly {
		where = append(where, "NOT a.allowed")
	}
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	args = append(args, limit)

	q := `
		SELECT a.ts, COALESCE(u.username, ''), COALESCE(a.node_id, ''),
		       a.ns, a.path, a.allowed, COALESCE(a.status, 0), COALESCE(a.reason, '')
		FROM audit_log a
		LEFT JOIN users u ON u.id = a.user_id
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY a.ts DESC, a.id DESC
		LIMIT $` + fmt.Sprint(len(args))

	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("audit: cannot read the trail: %w", err)
	}
	defer rows.Close()

	var out []Decision
	for rows.Next() {
		var d Decision
		if err := rows.Scan(&d.At, &d.User, &d.Node, &d.Ns, &d.Path, &d.Allowed, &d.Status, &d.Reason); err != nil {
			return nil, fmt.Errorf("audit: cannot scan a decision: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
