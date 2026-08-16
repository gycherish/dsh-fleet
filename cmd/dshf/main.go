// Command dshf is the dsh-fleet control plane: one console for a fleet of
// DeepSeek Harness machines, plus the operator CLI that manages it.
//
// The daemon and the CLI are one binary on purpose. Docker is the supported
// deployment, and `docker compose exec dshf dshf node add …` is a much better
// operator story than shipping and versioning a second image.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/gycherish/dsh-fleet/internal/audit"
	"github.com/gycherish/dsh-fleet/internal/config"
	"github.com/gycherish/dsh-fleet/internal/nodes"
	"github.com/gycherish/dsh-fleet/internal/proxy"
	"github.com/gycherish/dsh-fleet/internal/store"
	"github.com/gycherish/dsh-fleet/internal/uplink"
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
  dshf serve                     run the control plane
  dshf node add <id> [--label]   register a machine and mint its one-time token
  dshf node ls                   list machines and their status
  dshf node revoke <id>          withdraw a machine's token
  dshf version                   print the build version

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
	registry := uplink.NewRegistry()
	auditor, stopAudit := audit.New(pool, logger)
	defer stopAudit()

	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := pool.Ping(r.Context()); err != nil {
			http.Error(w, "database unreachable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("content-type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})

	mux.Handle("GET /uplink", &uplink.Handler{
		Registry: registry,
		Auth:     nodeStore,
		Sink:     nodeStore,
		Log:      logger,
		NotFound: nodes.ErrNotFound,
		Revoked:  nodes.ErrRevoked,
	})

	// Browser plane. There is NO user authentication yet, so this is bound to
	// loopback by default (see DSHF_BIND) and must not be exposed until the
	// console's own auth lands.
	proxyHandler := &proxy.Handler{
		Registry:        registry,
		Log:             logger,
		Audit:           auditor,
		AllowPrivileged: false,
	}
	mux.Handle("/n/{node}/{rest...}", proxyHandler)

	mux.HandleFunc("GET /api/nodes", func(w http.ResponseWriter, r *http.Request) {
		writeNodeList(w, r, nodeStore, registry, logger)
	})

	server := &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: the uplink and the browser event streams are both
		// long-lived, and a write deadline would sever them mid-stream.
		IdleTimeout: 120 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", cfg.Listen, "publicUrl", cfg.PublicURL.String(), "version", version)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
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

func writeNodeList(w http.ResponseWriter, r *http.Request, s *nodes.Store, reg *uplink.Registry, log *slog.Logger) {
	list, err := s.List(r.Context())
	if err != nil {
		log.Error("cannot list nodes", "err", err)
		http.Error(w, "cannot list nodes", http.StatusInternalServerError)
		return
	}
	online := map[string]bool{}
	for _, id := range reg.Online() {
		online[id] = true
	}
	w.Header().Set("content-type", "application/json")
	_, _ = w.Write([]byte("["))
	for i, n := range list {
		if i > 0 {
			_, _ = w.Write([]byte(","))
		}
		snapshot := "null"
		if len(n.Snapshot) > 0 {
			snapshot = string(n.Snapshot)
		}
		fmt.Fprintf(w,
			`{"id":%q,"label":%q,"online":%t,"dshVersion":%q,"platform":%q,"snapshot":%s}`,
			n.ID, n.Label, online[n.ID], n.DSHVersion, n.Platform, snapshot,
		)
	}
	_, _ = w.Write([]byte("]"))
}

// ── node ─────────────────────────────────────────────────────────────────────

func nodeCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: dshf node <add|ls|revoke>")
	}
	dsn, err := config.LoadDatabaseURL()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := store.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	s := nodes.New(pool)

	switch args[0] {
	case "add":
		fs := flag.NewFlagSet("node add", flag.ContinueOnError)
		label := fs.String("label", "", "operator-facing display name")
		// Two passes, because flag stops at the first positional: the first
		// pass consumes any leading flags and surfaces the id, the second
		// consumes flags written after it. Operators write both orders and
		// neither should be a usage error.
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			return errors.New("usage: dshf node add <id> [--label NAME]")
		}
		id := fs.Arg(0)
		if err := fs.Parse(fs.Args()[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return fmt.Errorf("unexpected arguments after node id: %s", strings.Join(fs.Args(), " "))
		}
		token, err := s.Register(ctx, id, *label)
		if err != nil {
			return err
		}
		// Printed exactly once: only the hash is stored, so there is no way to
		// show it again.
		fmt.Printf("registered node %q\n\n", id)
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
