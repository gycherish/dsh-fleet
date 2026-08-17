package console

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gycherish/dsh-fleet/internal/nodes"
)

// offlineAfter is how long without contact before a node reads as offline.
// Two missed heartbeats plus slack.
const offlineAfter = 60 * time.Second

// NodeSource supplies the registered machines.
type NodeSource interface {
	List(ctx context.Context) ([]nodes.Node, error)
}

// LiveSource reports which machines currently hold an uplink.
type LiveSource interface {
	Online() []string
}

// NodesPage renders the machine chooser.
type NodesPage struct {
	Nodes NodeSource
	Live  LiveSource
	Log   *slog.Logger
}

// nodeView is one row as the template needs it. The telemetry snapshot is
// projected here rather than in the template so a node running a newer plugin,
// with keys this build has never heard of, still renders.
type nodeView struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Status   string `json:"status"` // online | offline | never-seen | revoked
	DSH      string `json:"dsh,omitempty"`
	Platform string `json:"platform,omitempty"`
	LastSeen string `json:"lastSeen"`
	Agents   string `json:"agents,omitempty"`
	Tools    int    `json:"tools,omitempty"`
	Failed   int    `json:"failed,omitempty"`
	Plugins  int    `json:"plugins,omitempty"`
	// Current marks the machine this browser is already driving, so the page
	// orients someone who came back to switch.
	Current bool `json:"current"`
}

// snapshot is the subset of a node's telemetry this page displays. Every field
// is optional: telemetry is deliberately an open object.
type snapshot struct {
	Agents struct {
		Total   int `json:"total"`
		Running int `json:"running"`
	} `json:"agents"`
	Tools   int `json:"tools"`
	Entries []struct {
		Phase string `json:"phase"`
	} `json:"entries"`
}

func (p *NodesPage) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	list, err := p.Nodes.List(r.Context())
	if err != nil {
		p.Log.Error("console: cannot list nodes", "err", err)
		http.Error(w, "cannot list machines", http.StatusInternalServerError)
		return
	}
	online := map[string]bool{}
	for _, id := range p.Live.Online() {
		online[id] = true
	}

	current := SelectedNode(r)
	views := make([]nodeView, 0, len(list))
	for _, n := range list {
		v := project(n, online[n.ID])
		v.Current = n.ID == current
		views = append(views, v)
	}

	user := UserFrom(r.Context())
	username := ""
	if user != nil {
		username = user.Username
	}
	admin := user != nil && user.IsAdmin
	renderNodes(w, nodesData{Nodes: views, User: username, Prefix: Prefix, Admin: admin})
}

// Home sends a bare visit to the reserved prefix on to the chooser.
//
// The overlay injected into a machine's pages is how anyone actually gets
// back; this short URL is the fallback for a browser that ran the page with
// scripting off, or a session that expired while a machine was open.
func Home(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, PathConsole, http.StatusSeeOther)
}

// Select points this browser at one machine and hands it the origin root.
//
// The redirect is what makes the scheme work: from here on the browser is
// talking to the node's own application at `/`, exactly the paths that
// application expects to own.
func (g *Guard) Select(w http.ResponseWriter, r *http.Request) {
	nodeID := strings.TrimPrefix(r.URL.Path, PathSelect)
	if nodeID == "" || strings.Contains(nodeID, "/") {
		http.Error(w, "no machine named", http.StatusBadRequest)
		return
	}
	g.SetNode(w, nodeID)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func project(n nodes.Node, live bool) nodeView {
	v := nodeView{
		ID:       n.ID,
		Label:    n.Label,
		DSH:      n.DSHVersion,
		Platform: n.Platform,
		Status:   status(n, live),
		LastSeen: "never",
	}
	if v.Label == "" {
		v.Label = n.ID
	}
	if n.LastSeenAt != nil {
		v.LastSeen = humanSince(time.Since(*n.LastSeenAt))
	}
	if len(n.Snapshot) > 0 {
		var s snapshot
		// A snapshot this build cannot parse is not an error worth showing: the
		// row still carries identity and liveness, which is what the chooser is
		// for.
		if err := json.Unmarshal(n.Snapshot, &s); err == nil {
			v.Agents = itoa(s.Agents.Running) + " / " + itoa(s.Agents.Total)
			v.Tools = s.Tools
			v.Plugins = len(s.Entries)
			for _, e := range s.Entries {
				if e.Phase == "failed" {
					v.Failed++
				}
			}
		}
	}
	return v
}

func status(n nodes.Node, live bool) string {
	switch {
	case n.RevokedAt != nil:
		return "revoked"
	case live:
		return "online"
	case n.LastSeenAt == nil:
		return "never-seen"
	case time.Since(*n.LastSeenAt) < offlineAfter:
		return "online"
	default:
		return "offline"
	}
}

// nowSince is time.Since, named so a caller reads as "how long ago was this".
func nowSince(t time.Time) time.Duration { return time.Since(t) }

// humanSince renders an age the way an operator reads it, not to the second.
func humanSince(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return itoa(int(d.Minutes())) + "m ago"
	case d < 24*time.Hour:
		return itoa(int(d.Hours())) + "h ago"
	default:
		return itoa(int(d.Hours()/24)) + "d ago"
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
