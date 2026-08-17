package console

import (
	"log/slog"
	"net/http"
)

// OfflinePage stands in for a machine that is not connected.
//
// It exists because the bare 502 it replaces was a dead end. A selected
// machine owns the origin root, so when it cannot answer, the page served in
// its place is the only thing on screen — and a page of plain text carries no
// way back to the chooser and no way to sign out. Opening an offline machine
// on a phone stranded you exactly the way opening a working one used to.
type OfflinePage struct {
	Nodes NodeSource
	Live  LiveSource
	Log   *slog.Logger
}

func (p *OfflinePage) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	nodeID := SelectedNode(r)

	// Only a navigation gets a page. An asset or an `/api` call from a machine
	// page that is already open must keep failing as a status the caller can
	// read, or a fetch would parse this HTML as its answer.
	if !isNavigation(r) {
		http.Error(w, "machine is not connected", http.StatusBadGateway)
		return
	}

	view := nodeView{ID: nodeID, Label: nodeID, Status: "offline", LastSeen: "never"}
	if list, err := p.Nodes.List(r.Context()); err == nil {
		online := map[string]bool{}
		for _, id := range p.Live.Online() {
			online[id] = true
		}
		for _, n := range list {
			if n.ID == nodeID {
				view = project(n, online[n.ID])
				break
			}
		}
	} else {
		// Worth a page anyway: the identity came from the cookie, and the way
		// back matters more than the details.
		p.Log.Error("console: cannot list nodes for the offline page", "err", err)
	}

	w.Header().Set("cache-control", "no-store")
	renderOffline(w, view)
}
