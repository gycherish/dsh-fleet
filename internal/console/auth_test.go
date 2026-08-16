package console

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// An unsanitised `next` would make the login form an open redirect, which is
// the classic phishing lever on any authenticated app.
func TestSanitizeNext(t *testing.T) {
	cases := map[string]string{
		"":                          "/",
		"/":                         "/",
		"/api/session.list":         "/api/session.list",
		"/_fleet/console":           "/_fleet/console",
		"//evil.example.com":        "/",
		"https://evil.example.com":  "/",
		"http://evil.example.com/x": "/",
		"javascript:alert(1)":       "/",
		"evil.example.com":          "/",
	}
	for in, want := range cases {
		if got := sanitizeNext(in); got != want {
			t.Errorf("sanitizeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

// The guard must redirect a page load but refuse a background call. Answering
// a fetch with the login page's HTML and a 200 is how an expired session turns
// into an unreadable parse error instead of a clear 401.
func TestIsNavigation(t *testing.T) {
	cases := []struct {
		name    string
		method  string
		headers map[string]string
		want    bool
	}{
		{"browser navigation", "GET", map[string]string{"sec-fetch-mode": "navigate"}, true},
		{"browser fetch", "GET", map[string]string{"sec-fetch-mode": "cors"}, false},
		{"same-origin subresource", "GET", map[string]string{"sec-fetch-mode": "no-cors"}, false},
		{"legacy html GET", "GET", map[string]string{"accept": "text/html,*/*"}, true},
		{"legacy json GET", "GET", map[string]string{"accept": "application/json"}, false},
		{"post is never a navigation", "POST", map[string]string{"sec-fetch-mode": "navigate"}, false},
		{"bare GET", "GET", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "/", nil)
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			if got := isNavigation(r); got != tc.want {
				t.Fatalf("isNavigation = %t, want %t", got, tc.want)
			}
		})
	}
}

// Fetch Metadata must win over Accept: a browser that sends both is telling
// the truth in the header that cannot be spoofed by markup.
func TestIsNavigationPrefersFetchMetadata(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("sec-fetch-mode", "cors")
	r.Header.Set("accept", "text/html")
	if isNavigation(r) {
		t.Fatal("Accept should not override an explicit sec-fetch-mode")
	}
}

func TestSelectedNode(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	if got := SelectedNode(r); got != "" {
		t.Fatalf("no cookie should select nothing, got %q", got)
	}
	r.AddCookie(&http.Cookie{Name: NodeCookie, Value: "laptop"})
	if got := SelectedNode(r); got != "laptop" {
		t.Fatalf("SelectedNode = %q, want laptop", got)
	}
}

// A machine id is a single path segment. Anything else is either a mistake or
// an attempt to reach past the handler.
func TestSelectRejectsMalformedIDs(t *testing.T) {
	g := &Guard{}
	for _, path := range []string{PathSelect, PathSelect + "a/b", PathSelect + "../x"} {
		w := httptest.NewRecorder()
		g.Select(w, httptest.NewRequest("GET", path, nil))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", path, w.Code)
		}
	}
}

func TestSelectSetsCookieAndSendsToRoot(t *testing.T) {
	g := &Guard{}
	w := httptest.NewRecorder()
	g.Select(w, httptest.NewRequest("GET", PathSelect+"devbox", nil))

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/" {
		t.Fatalf("Location = %q, want /", loc)
	}
	var found bool
	for _, c := range w.Result().Cookies() {
		if c.Name == NodeCookie && c.Value == "devbox" {
			found = true
			if c.Path != "/" {
				t.Errorf("cookie path = %q, want / so it covers the node's whole app", c.Path)
			}
			if !c.HttpOnly {
				t.Error("the node cookie should be HttpOnly")
			}
		}
	}
	if !found {
		t.Fatal("Select did not set the node cookie")
	}
}
