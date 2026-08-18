package console

import (
	"log/slog"
	"net/http"
	"strconv"

	"github.com/gycherish/dsh-fleet/internal/audit"
)

// AuditPage shows what the privilege gate decided.
//
// Admin-only, and read-only. The trail existed from the beginning and nothing
// could read it: every decision went into the database and stayed there, which
// makes a boundary this project owns one nobody can inspect.
type AuditPage struct {
	Trail *audit.Recorder
	Log   *slog.Logger
}

type auditData struct {
	User       string
	Prefix     string
	Rows       []auditRow
	Node       string
	DeniedOnly bool
	Error      string
}

type auditRow struct {
	When    string
	Who     string
	Machine string
	Refused bool
	Status  int
	Method  string
	Reason  string
}

func (p *AuditPage) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	me := UserFrom(r.Context())
	if me == nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	query := r.URL.Query()
	filter := audit.Filter{
		Node:       query.Get("node"),
		DeniedOnly: query.Has("denied"),
		Limit:      200,
	}
	if n, err := strconv.Atoi(query.Get("limit")); err == nil && n > 0 {
		filter.Limit = n
	}

	data := auditData{User: me.Username, Prefix: Prefix, Node: filter.Node, DeniedOnly: filter.DeniedOnly}
	list, err := p.Trail.Recent(r.Context(), filter)
	if err != nil {
		p.Log.Error("console: cannot read the audit trail", "err", err)
		data.Error = "Could not read the trail."
	}
	for _, d := range list {
		data.Rows = append(data.Rows, auditRow{
			When:    d.At.Local().Format("01-02 15:04:05"),
			Who:     d.User,
			Machine: d.Node,
			Refused: !d.Allowed,
			Status:  d.Status,
			Method:  d.Path,
			Reason:  d.Reason,
		})
	}
	renderAudit(w, data)
}
