package console

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"github.com/gycherish/dsh-fleet/internal/users"
)

// PeoplePage is the admin view of every console account.
type PeoplePage struct {
	Users *users.Store
	Log   *slog.Logger
}

type peopleData struct {
	User    string
	Prefix  string
	People  []personView
	Error   string
	Notice  string
	Created string
}

type personView struct {
	ID       string
	Username string
	Admin    bool
	Disabled bool
	Created  string
	// Self marks the signed-in account, which the page refuses to let anyone
	// delete or demote out from under themselves.
	Self bool
}

func (p *PeoplePage) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	me := UserFrom(r.Context())
	if me == nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	query := r.URL.Query()
	data := peopleData{
		User: me.Username, Prefix: Prefix,
		Error: query.Get("error"), Notice: query.Get("notice"), Created: query.Get("created"),
	}
	list, err := p.Users.List(r.Context())
	if err != nil {
		p.Log.Error("console: cannot list accounts", "err", err)
		http.Error(w, "cannot list accounts", http.StatusInternalServerError)
		return
	}
	for _, u := range list {
		data.People = append(data.People, personView{
			ID: u.ID.String(), Username: u.Username, Admin: u.IsAdmin,
			Disabled: u.DisabledAt != nil,
			Created:  u.CreatedAt.Local().Format("2006-01-02"),
			Self:     u.ID == me.ID,
		})
	}
	renderPeople(w, data)
}

// Create adds an account.
func (p *PeoplePage) Create(w http.ResponseWriter, r *http.Request) {
	if r.ParseForm() != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("username"))
	password, again := r.FormValue("password"), r.FormValue("again")
	admin := r.FormValue("admin") == "on"

	if password != again {
		p.redirect(w, r, "The two passwords do not match.", "", "")
		return
	}
	if len(password) < users.MinPasswordLength {
		p.redirect(w, r, users.ErrWeakPassword.Error(), "", "")
		return
	}
	user, err := p.Users.Create(r.Context(), name, password, admin)
	switch {
	case errors.Is(err, users.ErrExists):
		p.redirect(w, r, "That username is taken.", "", "")
	case err != nil:
		p.Log.Error("console: cannot create account", "err", err)
		p.redirect(w, r, "Could not create the account.", "", "")
	default:
		p.Log.Info("console: account created", "user", user.Username, "admin", admin)
		p.redirect(w, r, "", "", user.Username)
	}
}

// Update applies one change to one account: reset, admin, disable, or delete.
//
// One handler rather than four routes because they share every guard — parse
// the id, refuse to act on yourself where it would lock you out, and never
// remove the last admin.
func (p *PeoplePage) Update(w http.ResponseWriter, r *http.Request) {
	me := UserFrom(r.Context())
	if me == nil || r.ParseForm() != nil {
		http.Error(w, "malformed request", http.StatusBadRequest)
		return
	}
	id, err := uuid.Parse(r.FormValue("id"))
	if err != nil {
		p.redirect(w, r, "That is not an account id.", "", "")
		return
	}
	action := r.FormValue("action")

	// Losing every admin means nobody can ever add one back, so these two are
	// checked before they are applied rather than repaired afterwards.
	if action == "delete" || action == "demote" || action == "disable" {
		if err := p.refuseLockout(r, id, action); err != nil {
			p.redirect(w, r, err.Error(), "", "")
			return
		}
	}

	switch action {
	case "reset":
		password, again := r.FormValue("password"), r.FormValue("again")
		if password != again {
			p.redirect(w, r, "The two passwords do not match.", "", "")
			return
		}
		// No current password: an admin resetting one has no plaintext to check.
		err = p.Users.SetPassword(r.Context(), id, "", password)
		if errors.Is(err, users.ErrWeakPassword) {
			p.redirect(w, r, err.Error(), "", "")
			return
		}
		p.done(w, r, err, "Password reset. That account's other sessions were signed out.")
	case "promote":
		p.done(w, r, p.Users.SetAdmin(r.Context(), id, true), "Now an administrator.")
	case "demote":
		p.done(w, r, p.Users.SetAdmin(r.Context(), id, false), "No longer an administrator.")
	case "disable":
		p.done(w, r, p.Users.SetDisabled(r.Context(), id, true), "Access withdrawn, and sessions signed out.")
	case "enable":
		p.done(w, r, p.Users.SetDisabled(r.Context(), id, false), "Access restored.")
	case "delete":
		p.done(w, r, p.Users.Delete(r.Context(), id),
			"Account deleted. Machines it enrolled keep running until their next reconnect.")
	default:
		p.redirect(w, r, "Unknown action.", "", "")
	}
}

// refuseLockout stops a change that would leave the console with no admin, and
// stops anyone locking themselves out by hand.
func (p *PeoplePage) refuseLockout(r *http.Request, id uuid.UUID, action string) error {
	me := UserFrom(r.Context())
	if me != nil && me.ID == id {
		switch action {
		case "delete":
			return errors.New("You cannot delete the account you are signed in as.")
		case "disable":
			return errors.New("You cannot withdraw your own access.")
		}
	}
	target, err := p.targetIsAdmin(r, id)
	if err != nil || !target {
		return nil
	}
	remaining, err := p.Users.CountAdmins(r.Context())
	if err != nil {
		p.Log.Error("console: cannot count admins", "err", err)
		return errors.New("Could not check how many administrators remain.")
	}
	if remaining <= 1 {
		return errors.New("This is the last administrator; promote another one first.")
	}
	return nil
}

func (p *PeoplePage) targetIsAdmin(r *http.Request, id uuid.UUID) (bool, error) {
	list, err := p.Users.List(r.Context())
	if err != nil {
		return false, err
	}
	for _, u := range list {
		if u.ID == id {
			return u.IsAdmin && u.DisabledAt == nil, nil
		}
	}
	return false, nil
}

func (p *PeoplePage) done(w http.ResponseWriter, r *http.Request, err error, notice string) {
	if err != nil {
		p.Log.Error("console: account change failed", "err", err)
		p.redirect(w, r, "Could not apply that change.", "", "")
		return
	}
	p.redirect(w, r, "", notice, "")
}

func (p *PeoplePage) redirect(w http.ResponseWriter, r *http.Request, failure, notice, created string) {
	target := PathPeople
	switch {
	case failure != "":
		target += "?error=" + url.QueryEscape(failure)
	case notice != "":
		target += "?notice=" + url.QueryEscape(notice)
	case created != "":
		target += "?created=" + url.QueryEscape(created)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}
