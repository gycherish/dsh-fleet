// Command dshf is the dsh-fleet control plane: one console for a fleet of
// DeepSeek Harness machines, plus the operator CLI that manages it.
//
// The daemon and the CLI are one binary on purpose. Docker is the supported
// deployment, and `docker compose exec dshf dshf node add …` is a much better
// operator story than shipping and versioning a second image.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gycherish/dsh-fleet/internal/audit"
	"github.com/gycherish/dsh-fleet/internal/certs"
	"github.com/gycherish/dsh-fleet/internal/config"
	"github.com/gycherish/dsh-fleet/internal/console"
	"github.com/gycherish/dsh-fleet/internal/nodes"
	"github.com/gycherish/dsh-fleet/internal/proxy"
	"github.com/gycherish/dsh-fleet/internal/store"
	"github.com/gycherish/dsh-fleet/internal/uplink"
	"github.com/gycherish/dsh-fleet/internal/users"
)

// version is stamped at build time via -ldflags.
var version = "dev"

// offlineAfter is how long without contact before the console calls a node
// offline. Two missed heartbeats plus slack.
const offlineAfter = 60 * time.Second

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "dshf: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("no command given")
	}
	switch args[0] {
	case "serve":
		return serve(args[1:])
	case "node":
		return nodeCmd(args[1:])
	case "user":
		return userCmd(args[1:])
	case "cert":
		return certCmd(args[1:])
	case "version", "--version", "-v":
		fmt.Println(version)
		return nil
	case "help", "--help", "-h":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `dshf — dsh-fleet control plane

Usage:
  dshf serve                       run the control plane
  dshf node add <id> [--label]     register a machine and mint its one-time token
  dshf node add <id> --rotate      reissue the token for a machine already known
  dshf node ls                     list machines and their status
  dshf node revoke <id>            withdraw a machine's token
  dshf user add <name> [--admin]   create a console account
  dshf user ls                     list console accounts
  dshf user passwd <name>          reset an account's password
  dshf user token add <name>       mint a token that machine can enrol themselves with
  dshf user token ls|revoke        list or withdraw those tokens
  dshf cert [--dir D] [host...]    mint a self-signed certificate for HTTPS
  dshf version                     print the build version

Configuration is environment-only; see deploy/.env.example.
`)
}

// ── serve ────────────────────────────────────────────────────────────────────

func serve(_ []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := newLogger(cfg.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	bootCtx, cancelBoot := context.WithTimeout(ctx, 60*time.Second)
	defer cancelBoot()

	pool, err := store.Open(bootCtx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	logger.Info("database connected")

	applied, err := store.Migrate(bootCtx, pool, cfg.MigrationsDir)
	if err != nil {
		return err
	}
	if len(applied) > 0 {
		logger.Info("migrations applied", "versions", strings.Join(applied, ","))
	} else {
		logger.Info("schema up to date")
	}

	nodeStore := nodes.New(pool)
	userStore := users.New(pool, logger)
	registry := uplink.NewRegistry()
	auditor, stopAudit := audit.New(pool, logger)
	defer stopAudit()

	created, err := userStore.EnsureBootstrapAdmin(bootCtx, cfg.AdminUser, cfg.AdminPassword)
	if err != nil {
		return err
	}
	if created {
		logger.Info("bootstrap admin created", "user", cfg.AdminUser)
	}

	guard := &console.Guard{
		Users: userStore,
		Log:   logger,
		// Follows the declared origin so a plain-HTTP development run still
		// gets working cookies while a real deployment gets Secure ones.
		Secure: cfg.PublicURL.Scheme == "https",
	}

	mux := http.NewServeMux()

	// ── unauthenticated ──
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := pool.Ping(r.Context()); err != nil {
			http.Error(w, "database unreachable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("content-type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET "+console.PathLogin, guard.LoginPage)
	mux.HandleFunc("POST "+console.PathLogin, guard.Login)
	mux.HandleFunc("POST "+console.PathLogout, guard.Logout)

	// The node uplink authenticates with its own token, not a console session,
	// so it deliberately sits outside the browser guard.
	mux.Handle("GET /uplink", &uplink.Handler{
		Registry: registry,
		Auth:     nodeStore,
		// Self-enrolment spans both stores — a token belongs to a person, the
		// machine name to the fleet — so it is composed here rather than making
		// either store know about the other.
		Enrol:    &enroller{users: userStore, nodes: nodeStore},
		Sink:     nodeStore,
		Log:      logger,
		NotFound: nodes.ErrNotFound,
		Revoked:  nodes.ErrRevoked,
		Foreign:  nodes.ErrForeign,
		OwnToken: nodes.ErrHasOwnToken,
	})

	// ── the console, behind the session guard ──
	// A bare visit to the reserved prefix is the short way back to the chooser
	// once a machine has taken over the origin root.
	mux.Handle("GET "+console.Prefix+"/{$}", guard.Require(http.HandlerFunc(console.Home)))
	mux.Handle("GET "+console.PathConsole, guard.Require(&console.NodesPage{
		Nodes: nodeStore,
		Live:  registry,
		Log:   logger,
	}))
	mux.Handle("GET "+console.PathSelect+"{node}", guard.Require(http.HandlerFunc(guard.Select)))
	// What the injected overlay reads, and the only console surface a machine's
	// own page ever talks to.
	mux.Handle("GET "+console.PathState, guard.Require(&console.StatePage{
		Nodes: nodeStore,
		Live:  registry,
		Log:   logger,
	}))
	mux.Handle("GET "+console.PathOverlay, guard.Require(http.HandlerFunc(console.Overlay)))

	// Everyone manages their own account and their own machine tokens.
	account := &console.AccountPage{
		Users: userStore,
		Log:   logger,
		// The address a node dials, shown beside a new token so the whole
		// configuration can be copied at once.
		Uplink: uplinkURL(cfg.PublicURL),
	}
	mux.Handle("GET "+console.PathAccount, guard.Require(account))
	mux.Handle("POST "+console.PathAccount+"/password", guard.Require(http.HandlerFunc(account.ChangePassword)))
	mux.Handle("POST "+console.PathAccount+"/tokens", guard.Require(http.HandlerFunc(account.MintToken)))
	mux.Handle("POST "+console.PathAccount+"/tokens/revoke", guard.Require(http.HandlerFunc(account.RevokeToken)))

	// Managing other people is an admin matter.
	people := &console.PeoplePage{Users: userStore, Log: logger}
	mux.Handle("GET "+console.PathPeople, guard.RequireAdmin(people))
	mux.Handle("POST "+console.PathPeople, guard.RequireAdmin(http.HandlerFunc(people.Create)))
	mux.Handle("POST "+console.PathPeople+"/update", guard.RequireAdmin(http.HandlerFunc(people.Update)))

	// ── everything else belongs to the selected machine ──
	//
	// The node's application owns the origin root because its client addresses
	// `/api/...` and its assets absolutely. Anything the control plane needs
	// for itself lives under console.Prefix, out of that application's way.
	access, err := proxy.ParseAccess(cfg.PrivilegedAccess)
	if err != nil {
		return err
	}
	logger.Info("privileged access", "level", string(access))

	mux.Handle("/", guard.Require(&proxy.Handler{
		Registry:    registry,
		Log:         logger,
		Audit:       auditor,
		Privileged:  access,
		SelectNode:  console.SelectedNode,
		NoSelection: http.HandlerFunc(noMachineSelected),
		// A machine can go down while someone is driving it, so the dead end
		// still needs a real page even though the chooser now refuses to walk
		// into one.
		Unreachable: &console.OfflinePage{Nodes: nodeStore, Live: registry, Log: logger},
		// The machine owns the origin root, so this tag is the console's only
		// place to stand: without it there is no way back to the chooser and no
		// way to sign out.
		Inject: console.OverlayScript,
	}))

	server := &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: the uplink and the browser event streams are both
		// long-lived, and a write deadline would sever them mid-stream.
		IdleTimeout: 120 * time.Second,
	}

	if cfg.ServesTLS() {
		// HTTP/1.1 only, deliberately.
		//
		// ListenAndServeTLS negotiates HTTP/2 through ALPN by default, and Go's
		// HTTP/2 server does not implement RFC 8441 Extended CONNECT — the only
		// way to open a WebSocket over h2. Every event downlink is a WebSocket,
		// so leaving h2 on breaks the product on any browser that negotiates it,
		// which Safari does. A non-nil empty map is how net/http says "no ALPN
		// protocols beyond http/1.1".
		//
		// The multiplexing lost matters far less than the sockets gained: this
		// carries a handful of long-lived streams, not hundreds of small ones.
		server.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){}
	}

	go purgeSessions(ctx, userStore, logger)

	warnIfInsecureOrigin(cfg, logger)

	errc := make(chan error, 1)
	go func() {
		logger.Info("listening",
			"addr", cfg.Listen, "publicUrl", cfg.PublicURL.String(),
			"tls", cfg.ServesTLS(), "version", version)
		var err error
		if cfg.ServesTLS() {
			err = server.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
		} else {
			err = server.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		logger.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return server.Shutdown(shutdownCtx)
}

// warnIfInsecureOrigin says so when the declared origin will break the UI.
//
// Browsers gate `crypto.randomUUID` and the rest of the secure-context APIs on
// HTTPS, exempting only loopback. The dsh client calls them, so a plain-HTTP
// LAN origin serves pages that fail with "crypto.randomUUID is not a function"
// — deep in a settings screen, nowhere near the cause. Saying it at boot is
// the difference between a five-minute fix and an afternoon.
func warnIfInsecureOrigin(cfg *config.Config, log *slog.Logger) {
	if cfg.PublicURL.Scheme == "https" {
		return
	}
	host := cfg.PublicURL.Hostname()
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return
	}
	log.Warn("public URL is not a secure context; browsers will disable crypto.randomUUID and the dsh UI will fail",
		"publicUrl", cfg.PublicURL.String(),
		"fix", "run `dshf cert` and set DSHF_TLS_CERT / DSHF_TLS_KEY, or front this with a TLS proxy")
}

// noMachineSelected answers a request that arrived before the browser picked
// a machine.
//
// A navigation goes to the chooser; anything else gets a status, because
// answering a fetch with the chooser's HTML would surface as a parse error
// rather than as the actionable "pick a machine first".
// uplinkURL turns the browser-facing origin into the address a node dials.
//
// Same host, `ws`/`wss` scheme, `/uplink` path — derived rather than configured
// so the two can never drift, which is the kind of mismatch that shows up as a
// node that silently never connects.
func uplinkURL(public *url.URL) string {
	scheme := "ws"
	if public.Scheme == "https" {
		scheme = "wss"
	}
	return scheme + "://" + public.Host + "/uplink"
}

// enroller joins the two stores self-enrolment needs.
type enroller struct {
	users *users.Store
	nodes *nodes.Store
}

func (e *enroller) AuthenticateToken(ctx context.Context, username, token string) (uuid.UUID, error) {
	user, err := e.users.AuthenticateToken(ctx, username, token)
	if err != nil {
		// Mapped onto the uplink's "unknown node or bad token" so a caller
		// guessing usernames learns nothing from the difference.
		return uuid.Nil, fmt.Errorf("%w: %w", nodes.ErrNotFound, err)
	}
	return user.ID, nil
}

func (e *enroller) Claim(ctx context.Context, nodeID string, ownerID uuid.UUID, label string) error {
	return e.nodes.EnsureOwned(ctx, nodeID, ownerID, label)
}

func noMachineSelected(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && strings.Contains(r.Header.Get("accept"), "text/html") {
		http.Redirect(w, r, console.PathConsole, http.StatusSeeOther)
		return
	}
	http.Error(w, "no machine selected", http.StatusServiceUnavailable)
}

// purgeSessions drops expired browser sessions hourly. Expiry is already
// enforced on every lookup; this only keeps the table from growing forever.
func purgeSessions(ctx context.Context, s *users.Store, log *slog.Logger) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := s.PurgeExpiredSessions(ctx)
			if err != nil {
				log.Warn("cannot purge sessions", "err", err)
				continue
			}
			if n > 0 {
				log.Info("purged expired sessions", "count", n)
			}
		}
	}
}

// ── node ─────────────────────────────────────────────────────────────────────

func nodeCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: dshf node <add|ls|revoke>")
	}
	ctx, pool, cancel, err := operatorPool()
	if err != nil {
		return err
	}
	defer cancel()
	defer pool.Close()
	s := nodes.New(pool)

	switch args[0] {
	case "add":
		fs := flag.NewFlagSet("node add", flag.ContinueOnError)
		label := fs.String("label", "", "operator-facing display name")
		rotate := fs.Bool("rotate", false, "reissue the token for a machine that already exists")
		id, err := parseOneArg(fs, args[1:], "dshf node add <id> [--label NAME] [--rotate]")
		if err != nil {
			return err
		}
		verb := "registered"
		var token string
		if *rotate {
			// Re-enrolment keeps the row, so the machine's history survives and
			// the operator does not have to invent a second name for it.
			verb = "reissued token for"
			token, err = s.Rotate(ctx, id, *label)
		} else {
			token, err = s.Register(ctx, id, *label)
		}
		if errors.Is(err, nodes.ErrExists) {
			return fmt.Errorf("%w (use --rotate to issue it a new token)", err)
		}
		if err != nil {
			return err
		}
		// Printed exactly once: only the hash is stored, so there is no way to
		// show it again.
		fmt.Printf("%s node %q\n\n", verb, id)
		fmt.Printf("  DSH_FLEET_NODE_ID=%s\n", id)
		fmt.Printf("  DSH_FLEET_TOKEN=%s\n\n", token)
		fmt.Println("This token is shown once. Set it on the node alongside DSH_FLEET_URL.")
		return nil

	case "ls":
		list, err := s.List(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tLABEL\tSTATUS\tDSH\tPLATFORM\tLAST SEEN")
		for _, n := range list {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
				n.ID, dash(n.Label), nodeStatus(n), dash(n.DSHVersion), dash(n.Platform), lastSeen(n))
		}
		return tw.Flush()

	case "revoke":
		if len(args) != 2 {
			return errors.New("usage: dshf node revoke <id>")
		}
		if err := s.Revoke(ctx, args[1]); err != nil {
			return err
		}
		fmt.Printf("revoked node %q; it is refused at its next reconnect\n", args[1])
		return nil

	default:
		return fmt.Errorf("unknown node subcommand %q", args[0])
	}
}

// ── user ─────────────────────────────────────────────────────────────────────

func userCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: dshf user <add|ls>")
	}
	ctx, pool, cancel, err := operatorPool()
	if err != nil {
		return err
	}
	defer cancel()
	defer pool.Close()
	s := users.New(pool, newLogger("warn"))

	switch args[0] {
	case "add":
		fs := flag.NewFlagSet("user add", flag.ContinueOnError)
		admin := fs.Bool("admin", false, "grant administrator rights")
		name, err := parseOneArg(fs, args[1:], "dshf user add <name> [--admin]")
		if err != nil {
			return err
		}
		// Read from the environment rather than a flag: a password in argv is
		// visible in the process list and in shell history.
		password := os.Getenv("DSHF_NEW_PASSWORD")
		if strings.TrimSpace(password) == "" {
			return errors.New("set DSHF_NEW_PASSWORD to the new account's password")
		}
		u, err := s.Create(ctx, name, password, *admin)
		if err != nil {
			return err
		}
		fmt.Printf("created account %q (admin=%t)\n", u.Username, u.IsAdmin)
		return nil

	case "ls":
		list, err := s.List(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "USERNAME\tADMIN\tSTATUS\tCREATED")
		for _, u := range list {
			status := "active"
			if u.DisabledAt != nil {
				status = "disabled"
			}
			fmt.Fprintf(tw, "%s\t%t\t%s\t%s\n",
				u.Username, u.IsAdmin, status, u.CreatedAt.Local().Format(time.RFC3339))
		}
		return tw.Flush()

	case "passwd":
		name, err := parseOneArg(flag.NewFlagSet("user passwd", flag.ContinueOnError),
			args[1:], "dshf user passwd <name>")
		if err != nil {
			return err
		}
		password := os.Getenv("DSHF_NEW_PASSWORD")
		if strings.TrimSpace(password) == "" {
			return errors.New("set DSHF_NEW_PASSWORD to the new password")
		}
		u, err := s.Find(ctx, name)
		if err != nil {
			return err
		}
		// No current password: this is the reset path, for the account nobody can
		// sign into any more.
		if err := s.SetPassword(ctx, u.ID, "", password); err != nil {
			return err
		}
		fmt.Printf("reset the password for %q; its other sessions were signed out\n", u.Username)
		return nil

	case "token":
		if len(args) < 2 {
			return errors.New("usage: dshf user token <add|ls|revoke> <name> [...]")
		}
		return userTokenCmd(ctx, s, args[1:])

	default:
		return fmt.Errorf("unknown user subcommand %q", args[0])
	}
}

// userTokenCmd manages the tokens a person uses to enrol their own machines.
func userTokenCmd(ctx context.Context, s *users.Store, args []string) error {
	switch args[0] {
	case "add":
		fs := flag.NewFlagSet("user token add", flag.ContinueOnError)
		label := fs.String("name", "", "what this token is for, e.g. laptop")
		name, err := parseOneArg(fs, args[1:], "dshf user token add <user> --name WHAT")
		if err != nil {
			return err
		}
		if strings.TrimSpace(*label) == "" {
			return errors.New("give the token a name with --name, so it can be recognised before it is revoked")
		}
		u, err := s.Find(ctx, name)
		if err != nil {
			return err
		}
		token, err := s.MintToken(ctx, u.ID, *label)
		if err != nil {
			return err
		}
		// Printed once: only the hash is stored.
		fmt.Printf("minted token %q for %q\n\n", *label, u.Username)
		fmt.Printf("  DSH_FLEET_USERNAME=%s\n", u.Username)
		fmt.Printf("  DSH_FLEET_TOKEN=%s\n\n", token)
		fmt.Println("Set these on the machine with DSH_FLEET_URL and a DSH_FLEET_NODE_ID of your choosing;")
		fmt.Println("it registers itself on first connection. This token is shown once.")
		return nil

	case "ls":
		name, err := parseOneArg(flag.NewFlagSet("user token ls", flag.ContinueOnError),
			args[1:], "dshf user token ls <user>")
		if err != nil {
			return err
		}
		u, err := s.Find(ctx, name)
		if err != nil {
			return err
		}
		list, err := s.ListTokens(ctx, u.ID)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tNAME\tSTATUS\tLAST USED")
		for _, t := range list {
			status := "active"
			if t.RevokedAt != nil {
				status = "revoked"
			}
			used := "never"
			if t.LastUsedAt != nil {
				used = t.LastUsedAt.Local().Format(time.RFC3339)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", t.ID, t.Name, status, used)
		}
		return tw.Flush()

	case "revoke":
		if len(args) != 3 {
			return errors.New("usage: dshf user token revoke <user> <token-id>")
		}
		u, err := s.Find(ctx, args[1])
		if err != nil {
			return err
		}
		id, err := uuid.Parse(args[2])
		if err != nil {
			return fmt.Errorf("%q is not a token id; find it with `dshf user token ls`", args[2])
		}
		if err := s.RevokeToken(ctx, u.ID, id); err != nil {
			return err
		}
		fmt.Println("revoked; every machine enrolled with it is refused at its next reconnect")
		return nil

	default:
		return fmt.Errorf("unknown token subcommand %q", args[0])
	}
}

// ── cert ─────────────────────────────────────────────────────────────────────

func certCmd(args []string) error {
	fs := flag.NewFlagSet("cert", flag.ContinueOnError)
	dir := fs.String("dir", "certs", "directory to write cert.pem and key.pem into")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Cover this host's own addresses by default: the point of the exercise is
	// that a phone types a LAN address, and a certificate without that address
	// in its SAN list is rejected however carefully it was trusted.
	hosts := fs.Args()
	if len(hosts) == 0 {
		hosts = certs.LocalAddresses()
	}

	out, err := certs.SelfSigned(*dir, hosts)
	if err != nil {
		return err
	}

	fmt.Printf("wrote %s and %s\n\n", out.CertPath, out.KeyPath)
	fmt.Printf("  covers   %s\n", strings.Join(append(out.DNSNames, out.IPs...), ", "))
	fmt.Printf("  expires  %s\n\n", out.NotAfter.Local().Format(time.RFC3339))
	fmt.Println("Then start the control plane with:")
	fmt.Printf("  DSHF_TLS_CERT=%s\n", out.CertPath)
	fmt.Printf("  DSHF_TLS_KEY=%s\n", out.KeyPath)
	fmt.Println("  DSHF_PUBLIC_URL=https://<the address you will type>:8080")
	fmt.Println()
	fmt.Println("The certificate signs itself, so the first visit shows a warning.")
	fmt.Println("Accepting it makes the origin a secure context, which is what the")
	fmt.Println("dsh UI needs: crypto.randomUUID does not exist without one.")
	return nil
}

// ── shared helpers ───────────────────────────────────────────────────────────

// operatorPool opens the database for a one-shot CLI command.
func operatorPool() (context.Context, *pgxpool.Pool, context.CancelFunc, error) {
	dsn, err := config.LoadDatabaseURL()
	if err != nil {
		return nil, nil, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	pool, err := store.Open(ctx, dsn)
	if err != nil {
		cancel()
		return nil, nil, nil, err
	}
	return ctx, pool, cancel, nil
}

// parseOneArg reads flags written before or after the single positional
// argument, because Go's flag package stops at the first non-flag token and
// operators write both orders.
func parseOneArg(fs *flag.FlagSet, args []string, usage string) (string, error) {
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if fs.NArg() == 0 {
		return "", errors.New("usage: " + usage)
	}
	value := fs.Arg(0)
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return "", err
	}
	if fs.NArg() != 0 {
		return "", fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	return value, nil
}

// nodeStatus reports liveness from last_seen_at rather than a stored flag, so
// a control-plane crash cannot leave a node reading as online forever.
func nodeStatus(n nodes.Node) string {
	switch {
	case n.RevokedAt != nil:
		return "revoked"
	case n.LastSeenAt == nil:
		return "never-seen"
	case time.Since(*n.LastSeenAt) < offlineAfter:
		return "online"
	default:
		return "offline"
	}
}

func lastSeen(n nodes.Node) string {
	if n.LastSeenAt == nil {
		return "-"
	}
	return n.LastSeenAt.Local().Format(time.RFC3339)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
	slog.SetDefault(logger)
	return logger
}
