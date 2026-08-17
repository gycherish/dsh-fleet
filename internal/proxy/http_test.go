package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
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

// Every method dsh pins to loopback must land in exactly one tier here. A
// method that falls out of both is forwarded unconditionally, which for this
// set means a remote credential write nobody decided to allow.
func TestEveryPinnedMethodIsTiered(t *testing.T) {
	pinned := []string{
		"host.pickDirectory", "host.openPath",
		"settings.describe", "settings.openDocument", "settings.update",
		"settings.replace", "settings.mutate",
		"credentials.describe", "credentials.set", "credentials.unset",
		"agentPreset.read", "agentPreset.copy", "agentPreset.openDocument", "agentPreset.remove",
	}
	for _, method := range pinned {
		_, isRead := privilegedRead[method]
		_, isWrite := privilegedWrite[method]
		if isRead == isWrite {
			t.Errorf("%s must be in exactly one tier (read=%t write=%t)", method, isRead, isWrite)
		}
	}
	// These are deliberately ordinary: the roster carries only ids and trust,
	// and choosing a preset grants nothing session.create already did not.
	for _, method := range []string{"agentPreset.list", "agentPreset.select", "session.prompt", "host.listDirectory"} {
		if _, r := privilegedRead[method]; r {
			t.Errorf("%s should not be gated", method)
		}
		if _, w := privilegedWrite[method]; w {
			t.Errorf("%s should not be gated", method)
		}
	}
}

// Nothing that can carry or reveal a secret value may sit in the read tier: it
// is on by default, so a mistake there is a default-on disclosure.
func TestReadTierHoldsNothingThatWrites(t *testing.T) {
	for method := range privilegedRead {
		for _, verb := range []string{".set", ".unset", ".update", ".replace", ".mutate", ".copy", ".remove", ".openDocument", ".pickDirectory", ".openPath", ".discoverModels"} {
			if strings.HasSuffix(method, verb) {
				t.Errorf("%s looks like a write but sits in the read tier", method)
			}
		}
	}
}

func TestAccessLevels(t *testing.T) {
	cases := []struct {
		level       Access
		method      string
		wantRefused bool
	}{
		{AccessNone, "settings.describe", true},
		{AccessNone, "credentials.set", true},
		{AccessRead, "settings.describe", false},
		{AccessRead, "credentials.describe", false},
		{AccessRead, "agentPreset.read", false},
		{AccessRead, "credentials.set", true},
		{AccessRead, "settings.update", true},
		{AccessRead, "llm.discoverModels", true},
		{AccessRead, "host.openPath", true},
		{AccessFull, "credentials.set", false},
		{AccessFull, "host.openPath", false},
		{AccessRead, "session.prompt", false},
	}
	for _, tc := range cases {
		if refused, _ := tc.level.refuse(tc.method); refused != tc.wantRefused {
			t.Errorf("%s.refuse(%s) = %t, want %t", tc.level, tc.method, refused, tc.wantRefused)
		}
	}
}

func TestParseAccess(t *testing.T) {
	for input, want := range map[string]Access{"": AccessFull, "none": AccessNone, "READ": AccessRead, " full ": AccessFull} {
		got, err := ParseAccess(input)
		if err != nil || got != want {
			t.Errorf("ParseAccess(%q) = (%q, %v), want %q", input, got, err, want)
		}
	}
	if _, err := ParseAccess("everything"); err == nil {
		t.Error("an unknown level must fail loud rather than defaulting")
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

func TestPrivilegedWriteIsRefusedAndAudited(t *testing.T) {
	audit := &recordingAudit{}
	h := &Handler{
		Registry:   uplink.NewRegistry(),
		Log:        discardLogger(),
		Audit:      audit,
		SelectNode: func(*http.Request) string { return "devbox" },
		Privileged: AccessRead,
	}
	// The registry is empty, so reaching the node would fail with 502. A 403
	// therefore proves the gate ran before any connection lookup.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/api/credentials.set", nil))

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if audit.calls != 1 || audit.allowed || audit.path != "credentials.set" {
		t.Fatalf("audit = %+v, want one denied decision for credentials.set", audit)
	}
}

// The default has to let every button work. Withholding the writes read as a
// safety measure but was not one — anyone past the login can already run shell
// commands through an ordinary session — and it broke the Agent presets page
// with "transport failure for /api/settings.update: HTTP 403".
func TestDefaultForwardsTheWholePinnedSet(t *testing.T) {
	h := &Handler{
		Registry:   uplink.NewRegistry(),
		Log:        discardLogger(),
		Audit:      &recordingAudit{},
		SelectNode: func(*http.Request) string { return "devbox" },
		// Privileged left at its zero value on purpose.
	}
	methods := []string{"settings.describe", "credentials.describe", "agentPreset.read"}
	for method := range privilegedWrite {
		methods = append(methods, method)
	}
	for _, method := range methods {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", "/api/"+method, nil))
		// 502 means it passed the gate and only then found no node.
		if w.Code != http.StatusBadGateway {
			t.Errorf("%s: status = %d, want 502 (allowed, node absent)", method, w.Code)
		}
	}
}

func TestFullAccessOpensTheGate(t *testing.T) {
	h := &Handler{
		Registry:   uplink.NewRegistry(),
		Log:        discardLogger(),
		Audit:      &recordingAudit{},
		SelectNode: func(*http.Request) string { return "devbox" },
		Privileged: AccessFull,
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
