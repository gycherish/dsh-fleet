package envelope

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// docs/envelope.md calls itself the single source of truth and says both
// implementations derive from it. Nothing enforced that, and it drifted: the
// whole `ws.*` family shipped in Go and TypeScript while the document listed
// neither it nor `hello`'s `username`, and the request section still described
// a carrier that had been replaced months earlier.
//
// Drift like that produces no failing test and no bug report. It surfaces when
// somebody writes a third implementation from the document and finds the wire
// does something else.

const specPath = "../../docs/envelope.md"

// documentedFrames reads the frame diagram at the top of the spec.
func documentedFrames(t *testing.T) []string {
	t.Helper()
	source, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("cannot read the spec: %v", err)
	}
	// Lines of the form "  ├─ name   description".
	re := regexp.MustCompile(`(?m)^\s*[├└]─\s+([a-z.]+)`)
	seen := map[string]bool{}
	var out []string
	for _, m := range re.FindAllStringSubmatch(string(source), -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	return out
}

// implementedFrames is every frame constant this package defines.
func implementedFrames() []string {
	all := []string{
		THello, TWelcome, TReq, TCancel, THead, TData, TEnd, TErr,
		TTelemetry, TPing, TPong, TWsOpen, TWsUp, TWsMsg, TWsClose,
	}
	sort.Strings(all)
	return all
}

func TestEveryFrameIsDocumented(t *testing.T) {
	documented := documentedFrames(t)
	if len(documented) == 0 {
		t.Fatal("found no frame diagram in the spec; this check would pass vacuously")
	}
	inDoc := map[string]bool{}
	for _, f := range documented {
		inDoc[f] = true
	}
	for _, f := range implementedFrames() {
		if !inDoc[f] {
			t.Errorf("%q is implemented but missing from the frame diagram in %s", f, specPath)
		}
	}
}

func TestEveryDocumentedFrameExists(t *testing.T) {
	implemented := map[string]bool{}
	for _, f := range implementedFrames() {
		implemented[f] = true
	}
	for _, f := range documentedFrames(t) {
		if !implemented[f] {
			t.Errorf("%q appears in the frame diagram of %s but no constant defines it", f, specPath)
		}
	}
}

// A frame with a diagram entry and no section is documented in name only.
func TestEveryFrameHasASection(t *testing.T) {
	source, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("cannot read the spec: %v", err)
	}
	text := string(source)
	for _, f := range implementedFrames() {
		// Either its own heading, or named in a heading it shares.
		if !strings.Contains(text, "### `"+f+"`") && !strings.Contains(text, "`"+f+"`") {
			t.Errorf("%q has no prose in %s, only a diagram line", f, specPath)
		}
	}
}

// The close codes the two sides exchange must be listed too: a node that sends
// one the control plane's operator cannot look up is a support ticket.
func TestEveryCloseCodeIsDocumented(t *testing.T) {
	source, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("cannot read the spec: %v", err)
	}
	text := string(source)
	for _, code := range []string{"4001", "4002", "4003", "4004", "4005", "4006"} {
		if !strings.Contains(text, "`"+code+"`") {
			t.Errorf("close code %s is defined but not documented in %s", code, specPath)
		}
	}
}
