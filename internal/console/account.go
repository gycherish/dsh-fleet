package console

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/gycherish/dsh-fleet/internal/users"
)

// AccountPage is one person's own settings: their password and their tokens.
//
// Separate from the admin user list because the two have different audiences
// and different risks. Everyone reaches this one; only an admin reaches the
// other, and nothing here can touch another account.
type AccountPage struct {
	Users *users.Store
	Log   *slog.Logger
	// Uplink is the address a node dials, derived from the public URL. Shown
	// with a freshly minted token so the whole configuration can be copied in
	// one go rather than assembled from three places.
	Uplink string

	mu      sync.Mutex
	reveals map[string]reveal
}

// revealLifetime is how long a freshly minted token stays collectable.
//
// Long enough to survive the redirect and a slow render, short enough that a
// plaintext does not sit in memory because somebody closed the tab.
const revealLifetime = 2 * time.Minute

// reveal is one minted token waiting to be shown exactly once.
//
// It exists because rendering the mint straight into the POST response made
// refresh re-submit the form: the browser re-posted, minted a second token, and
// showed that instead. Redirecting fixes refresh, but the plaintext must not
// travel in the URL to get there — so the redirect carries a one-use key and
// the token stays here, in memory, never written down.
type reveal struct {
	owner uuid.UUID
	token string
	name  string
	at    time.Time
}

// stash keeps a minted token for one collection and returns its key.
func (p *AccountPage) stash(owner uuid.UUID, token, name string) string {
	key := uuid.NewString()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.reveals == nil {
		p.reveals = map[string]reveal{}
	}
	// Opportunistic sweep: a tab closed before collecting would otherwise leave
	// its plaintext here until the process restarts.
	for k, v := range p.reveals {
		if time.Since(v.at) > revealLifetime {
			delete(p.reveals, k)
		}
	}
	p.reveals[key] = reveal{owner: owner, token: token, name: name, at: time.Now()}
	return key
}

// collect returns a stashed token and removes it, so a reload shows nothing.
func (p *AccountPage) collect(key string, owner uuid.UUID) (string, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok := p.reveals[key]
	if !ok {
		return "", ""
	}
	delete(p.reveals, key)
	// Scoped to the owner: a key guessed or shared cannot reveal another
	// account's token.
	if entry.owner != owner || time.Since(entry.at) > revealLifetime {
		return "", ""
	}
	return entry.token, entry.name
}

// accountData is what account.html renders.
type accountData struct {
	User   string
	Admin  bool
	Prefix string
	Tokens []tokenView
	Uplink string
	// Minted carries a token's plaintext exactly once, straight after it is
	// created. It is never stored, so this is the only chance to show it.
	Minted     string
	MintedName string
	Error      string
	Notice     string
}

type tokenView struct {
	ID       string
	Name     string
	Created  string
	LastUsed string
	Revoked  bool
}

func (p *AccountPage) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	if user == nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	query := r.URL.Query()
	// A one-use key, not the token: collecting it here is what makes refresh
	// harmless.
	token, name := p.collect(query.Get("shown"), user.ID)
	p.render(w, r, user, query.Get("error"), query.Get("notice"), token, name)
}

func (p *AccountPage) render(w http.ResponseWriter, r *http.Request, user *users.User, failure, notice, minted, mintedName string) {
	data := accountData{
		User:       user.Username,
		Admin:      user.IsAdmin,
		Prefix:     Prefix,
		Uplink:     p.Uplink,
		Minted:     minted,
		MintedName: mintedName,
		Error:      failure,
		Notice:     notice,
	}
	list, err := p.Users.ListTokens(r.Context(), user.ID)
	if err != nil {
		p.Log.Error("console: cannot list tokens", "err", err)
		data.Error = "Could not read your tokens."
	}
	for _, t := range list {
		view := tokenView{
			ID:       t.ID.String(),
			Name:     t.Name,
			Created:  t.CreatedAt.Local().Format("2006-01-02"),
			LastUsed: "never used",
			Revoked:  t.RevokedAt != nil,
		}
		if t.LastUsedAt != nil {
			view.LastUsed = "last used " + humanSince(nowSince(*t.LastUsedAt))
		}
		data.Tokens = append(data.Tokens, view)
	}
	renderAccount(w, data)
}

// ChangePassword handles the self-service password form.
func (p *AccountPage) ChangePassword(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	if user == nil || r.ParseForm() != nil {
		http.Error(w, "malformed request", http.StatusBadRequest)
		return
	}
	current, next, again := r.FormValue("current"), r.FormValue("next"), r.FormValue("again")

	switch {
	case next != again:
		p.redirect(w, r, "The two new passwords do not match.", "")
		return
	case current == "":
		p.redirect(w, r, "Enter your current password.", "")
		return
	}

	err := p.Users.SetPassword(r.Context(), user.ID, current, next)
	switch {
	case errors.Is(err, users.ErrBadCredentials):
		p.redirect(w, r, "That is not your current password.", "")
	case errors.Is(err, users.ErrWeakPassword):
		p.redirect(w, r, err.Error(), "")
	case err != nil:
		p.Log.Error("console: cannot change password", "err", err)
		p.redirect(w, r, "Could not change the password.", "")
	default:
		// Every session went with it, including this one, so the next request
		// lands on the login page. Saying so beats looking broken.
		p.Log.Info("console: password changed", "user", user.Username)
		http.Redirect(w, r, PathLogin+"?notice="+url.QueryEscape("Password changed. Sign in again."), http.StatusSeeOther)
	}
}

// MintToken issues a token for the signed-in account.
func (p *AccountPage) MintToken(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	if user == nil || r.ParseForm() != nil {
		http.Error(w, "malformed request", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	token, err := p.Users.MintToken(r.Context(), user.ID, name)
	if err != nil {
		if name == "" {
			p.redirect(w, r, "Give the token a name, so you can recognise it later.", "")
			return
		}
		p.Log.Error("console: cannot mint token", "err", err)
		p.redirect(w, r, "Could not create the token.", "")
		return
	}
	p.Log.Info("console: token minted", "user", user.Username, "name", name)
	// Redirect, so refreshing the result cannot re-submit the form and mint a
	// second token — which is exactly what rendering this response directly did.
	// The URL carries a one-use collection key; the plaintext stays in memory.
	http.Redirect(w, r, PathAccount+"?shown="+url.QueryEscape(p.stash(user.ID, token, name)), http.StatusSeeOther)
}

// RevokeToken withdraws one of the signed-in account's tokens.
func (p *AccountPage) RevokeToken(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	if user == nil || r.ParseForm() != nil {
		http.Error(w, "malformed request", http.StatusBadRequest)
		return
	}
	id, err := uuid.Parse(r.FormValue("id"))
	if err != nil {
		p.redirect(w, r, "That is not a token id.", "")
		return
	}
	// Scoped to the owner inside the store, so a guessed id from another
	// account is refused rather than honoured.
	if err := p.Users.RevokeToken(r.Context(), user.ID, id); err != nil {
		p.redirect(w, r, "Could not revoke that token.", "")
		return
	}
	p.Log.Info("console: token revoked", "user", user.Username)
	p.redirect(w, r, "", "Token revoked. Machines using it are refused at their next reconnect.")
}

func (p *AccountPage) redirect(w http.ResponseWriter, r *http.Request, failure, notice string) {
	target := PathAccount
	switch {
	case failure != "":
		target += "?error=" + url.QueryEscape(failure)
	case notice != "":
		target += "?notice=" + url.QueryEscape(notice)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}
