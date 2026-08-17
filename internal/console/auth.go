// Package console serves the browser plane: login, the node chooser, and the
// guard that stands in front of everything a node exposes.
package console

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gycherish/dsh-fleet/internal/users"
)

// Prefix reserves the control plane's own URLs.
//
// Everything outside it belongs to the selected node, because the dsh web
// client addresses `/api/...` and its assets absolutely and therefore needs
// the origin root. The prefix is deliberately unlikely to collide with any
// route that application serves.
const Prefix = "/_fleet"

// Paths the console owns.
const (
	PathLogin   = Prefix + "/login"
	PathLogout  = Prefix + "/logout"
	PathConsole = Prefix + "/console"
	PathSelect  = Prefix + "/select/"
	PathState   = Prefix + "/state"
	PathOverlay = Prefix + "/overlay.js"
	PathAccount = Prefix + "/account"
	PathPeople  = Prefix + "/people"
)

// SessionCookie is the browser session cookie name.
const SessionCookie = "dshf_session"

// NodeCookie remembers which node this browser is driving.
//
// It exists because the dsh web client calls `/api/...` as an absolute path.
// Serving a node under a path prefix therefore cannot work on its own: the
// client's own requests would miss the prefix. The cookie lets `/api/...`
// resolve to the node whose page the browser last opened.
//
// One consequence is honest and unavoidable: a browser drives one node at a
// time. Two tabs on two nodes share the cookie and would fight. Host-based
// selection is the fix for that and is what a multi-node deployment should
// use; this cookie is what makes a DNS-free setup work at all.
const NodeCookie = "dshf_node"

type ctxKey int

const userKey ctxKey = iota

// Guard authenticates browser requests.
type Guard struct {
	Users *users.Store
	Log   *slog.Logger
	// Secure marks cookies Secure. It follows the public URL's scheme, so a
	// plain-HTTP development run still gets working cookies.
	Secure bool
}

// UserFrom returns the authenticated account carried by a guarded request.
func UserFrom(ctx context.Context) *users.User {
	u, _ := ctx.Value(userKey).(*users.User)
	return u
}

// Require wraps a handler so only authenticated requests reach it.
//
// An unauthenticated navigation is redirected to the login page; anything else
// gets a bare 401. Redirecting a fetch would hand the caller an HTML login
// page with a 200, which is the classic way an expired session turns into an
// unreadable JSON parse error in the console.
func (g *Guard) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(SessionCookie)
		if err == nil {
			user, err := g.Users.LookupSession(r.Context(), cookie.Value)
			if err == nil {
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey, user)))
				return
			}
			if !errors.Is(err, users.ErrNoSession) {
				g.Log.Error("console: cannot resolve session", "err", err)
				http.Error(w, "cannot resolve session", http.StatusInternalServerError)
				return
			}
			g.clearCookie(w, SessionCookie)
		}
		if isNavigation(r) {
			target := PathLogin + "?next=" + url.QueryEscape(r.URL.RequestURI())
			http.Redirect(w, r, target, http.StatusSeeOther)
			return
		}
		http.Error(w, "authentication required", http.StatusUnauthorized)
	})
}

// RequireAdmin wraps a handler so only administrators reach it.
//
// Layered inside Require rather than replacing it, so an ordinary account gets
// "not yours" rather than "sign in", which is the truthful answer and does not
// send someone round a login loop they can never finish.
func (g *Guard) RequireAdmin(next http.Handler) http.Handler {
	return g.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := UserFrom(r.Context())
		if user == nil || !user.IsAdmin {
			http.Error(w, "administrators only", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	}))
}

// Login handles the login form.
func (g *Guard) Login(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}
	next := sanitizeNext(r.FormValue("next"))
	user, err := g.Users.Authenticate(r.Context(), r.FormValue("username"), r.FormValue("password"))
	if err != nil {
		if !errors.Is(err, users.ErrBadCredentials) {
			g.Log.Error("console: cannot authenticate", "err", err)
			http.Error(w, "cannot authenticate", http.StatusInternalServerError)
			return
		}
		renderLogin(w, http.StatusUnauthorized, next, "Wrong username or password.")
		return
	}

	token, err := g.Users.StartSession(r.Context(), user.ID, r.UserAgent())
	if err != nil {
		g.Log.Error("console: cannot start session", "err", err)
		http.Error(w, "cannot start session", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   g.Secure,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(users.SessionLifetime),
	})
	g.Log.Info("console: login", "user", user.Username)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// Logout revokes the current session.
func (g *Guard) Logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(SessionCookie); err == nil {
		if err := g.Users.EndSession(r.Context(), cookie.Value); err != nil {
			g.Log.Warn("console: cannot end session", "err", err)
		}
	}
	g.clearCookie(w, SessionCookie)
	g.clearCookie(w, NodeCookie)
	http.Redirect(w, r, PathLogin, http.StatusSeeOther)
}

// LoginPage renders the form, or skips it when a session is already valid.
func (g *Guard) LoginPage(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(SessionCookie); err == nil {
		if _, err := g.Users.LookupSession(r.Context(), cookie.Value); err == nil {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
	}
	renderLogin(w, http.StatusOK, sanitizeNext(r.URL.Query().Get("next")), "")
}

// SetNode remembers which node this browser is driving.
func (g *Guard) SetNode(w http.ResponseWriter, nodeID string) {
	http.SetCookie(w, &http.Cookie{
		Name:     NodeCookie,
		Value:    nodeID,
		Path:     "/",
		HttpOnly: true,
		Secure:   g.Secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// SelectedNode reports the node this browser last opened.
func SelectedNode(r *http.Request) string {
	cookie, err := r.Cookie(NodeCookie)
	if err != nil {
		return ""
	}
	return cookie.Value
}

func (g *Guard) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   g.Secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// isNavigation reports whether a browser is loading a page rather than making
// a background call, so the guard can redirect the first and refuse the second.
func isNavigation(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if mode := r.Header.Get("sec-fetch-mode"); mode != "" {
		return mode == "navigate"
	}
	// Older browsers send no Fetch Metadata; Accept is the remaining signal.
	return strings.Contains(r.Header.Get("accept"), "text/html")
}

// sanitizeNext keeps a post-login redirect inside this origin. An absolute or
// scheme-relative target would make the login form an open redirect.
func sanitizeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}
	return next
}
