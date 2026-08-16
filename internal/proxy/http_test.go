package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gycherish/dsh-fleet/internal/uplink"
	"github.com/gycherish/dsh-fleet/pkg/envelope"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		path       string
		wantNs     string
		wantMethod string
	}{
		{"/api/session.list", envelope.NsDSH, "session.list"},
		{"/api/events.mux", envelope.NsDSH, "events.mux"},
		{"/api/session.export?sessionId=x", envelope.NsDSH, "session.export"},
		{"/fleet/fleet.file.read", envelope.NsFleet, "fleet.file.read"},
		{"/", envelope.NsDSH, ""},
		{"/assets/index-abc.js", envelope.NsDSH, "assets/index-abc.js"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			ns, method := classify(tc.path)
			if ns != tc.wantNs || method != tc.wantMethod {
				t.Fatalf("classify(%q) = (%q, %q), want (%q, %q)", tc.path, ns, method, tc.wantNs, tc.wantMethod)
			}
		})
	}
}

// The privileged set is the boundary this project owns, because a custom
// carrier does not pass through dsh's own loopback pin. A silent gap here is
// a remote credential read.
func TestPrivilegedSetCoversTheLoopbackPinnedMethods(t *testing.T) {
	pinned := []string{
		"host.pickDirectory", "host.openPath",
		"settings.describe", "settings.openDocument", "settings.update",
		"settings.replace", "settings.mutate",
		"credentials.describe", "credentials.set", "credentials.unset",
		"agentPreset.read", "agentPreset.copy", "agentPreset.openDocument", "agentPreset.remove",
	}
	for _, method := range pinned {
		if _, ok := privileged[method]; !ok {
			t.Errorf("%s is loopback-pinned by dsh but not gated here", method)
		}
	}
	// These are deliberately ordinary: the roster carries only ids and trust,
	// and choosing a preset grants nothing session.create already did not.
	for _, method := range []string{"agentPreset.list", "agentPreset.select", "session.prompt", "host.listDirectory"} {
		if _, ok := privileged[method]; ok {
			t.Errorf("%s should not be gated", method)
		}
	}
}

type recordingAudit struct {
	node, ns, path string
	allowed        bool
	status         int
	reason         string
	calls          int
}

func (a *recordingAudit) RecordDecision(node, ns, path string, allowed bool, status int, reason string) {
	a.node, a.ns, a.path, a.allowed, a.status, a.reason = node, ns, path, allowed, status, reason
	a.calls++
}

func TestPrivilegedMethodIsRefusedAndAudited(t *testing.T) {
	audit := &recordingAudit{}
	h := &Handler{
		Registry:   uplink.NewRegistry(),
		Log:        discardLogger(),
		Audit:      audit,
		SelectNode: func(*http.Request) string { return "devbox" },
	}
	// The registry is empty, so reaching the node would fail with 502. A 403
	// therefore proves the gate ran before any connection lookup.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/api/credentials.describe", nil))

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if audit.calls != 1 || audit.allowed || audit.path != "credentials.describe" {
		t.Fatalf("audit = %+v, want one denied decision for credentials.describe", audit)
	}
}

func TestAllowPrivilegedOpensTheGate(t *testing.T) {
	h := &Handler{
		Registry:        uplink.NewRegistry(),
		Log:             discardLogger(),
		Audit:           &recordingAudit{},
		SelectNode:      func(*http.Request) string { return "devbox" },
		AllowPrivileged: true,
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/api/credentials.describe", nil))

	// Past the gate, then 502 because no node is connected.
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (gate open, node absent)", w.Code)
	}
}

func TestNoSelectionDelegates(t *testing.T) {
	var reached bool
	h := &Handler{
		Registry:    uplink.NewRegistry(),
		Log:         discardLogger(),
		SelectNode:  func(*http.Request) string { return "" },
		NoSelection: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }),
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if !reached {
		t.Fatal("a request naming no machine should reach NoSelection")
	}
}

func TestOfflineNodeIsBadGateway(t *testing.T) {
	h := &Handler{
		Registry:   uplink.NewRegistry(),
		Log:        discardLogger(),
		Audit:      &recordingAudit{},
		SelectNode: func(*http.Request) string { return "absent" },
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
}
