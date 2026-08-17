package console

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// State is what the console overlay needs to draw itself.
//
// It is one request rather than three because the overlay opens on a tap and
// has to render immediately: the machine list is useless without knowing which
// machine is current, and the sign-out control is useless without knowing
// whether anyone is signed in.
type State struct {
	User    string     `json:"user"`
	Current string     `json:"current"`
	Prefix  string     `json:"prefix"`
	Nodes   []nodeView `json:"nodes"`
}

// StatePage serves State as JSON.
type StatePage struct {
	Nodes NodeSource
	Live  LiveSource
	Log   *slog.Logger
}

func (p *StatePage) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	list, err := p.Nodes.List(r.Context())
	if err != nil {
		p.Log.Error("console: cannot list nodes", "err", err)
		http.Error(w, `{"error":"cannot list machines"}`, http.StatusInternalServerError)
		return
	}
	online := map[string]bool{}
	for _, id := range p.Live.Online() {
		online[id] = true
	}

	current := SelectedNode(r)
	state := State{Current: current, Prefix: Prefix, Nodes: make([]nodeView, 0, len(list))}
	if user := UserFrom(r.Context()); user != nil {
		state.User = user.Username
	}
	for _, n := range list {
		v := project(n, online[n.ID])
		v.Current = n.ID == current
		state.Nodes = append(state.Nodes, v)
	}

	w.Header().Set("content-type", "application/json; charset=utf-8")
	// Liveness is the whole point; a cached copy would show a machine as online
	// long after it went away.
	w.Header().Set("cache-control", "no-store")
	if err := json.NewEncoder(w).Encode(state); err != nil {
		p.Log.Warn("console: cannot write state", "err", err)
	}
}
