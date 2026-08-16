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
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/dsh-fleet/dsh-fleet/internal/config"
	"github.com/dsh-fleet/dsh-fleet/internal/store"
)

// version is stamped at build time via -ldflags.
var version = "dev"

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
	case "version", "--version", "-v":
		fmt.Println(version)
		return nil
	case "help", "--help", "-h":
		usage()
		return nil
	case "node", "token", "user":
		// These need the node registry and credential minting, which land with
		// the uplink router. Failing loudly beats a stub that appears to work
		// and hands out a token the control plane will not accept.
		return fmt.Errorf("command %q is not implemented yet; see api/envelope.md for the protocol it will serve", args[0])
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `dshf — dsh-fleet control plane

Usage:
  dshf serve          run the control plane (default in the container image)
  dshf version        print the build version

Planned:
  dshf node add       register a machine and mint its one-time token
  dshf node ls        list machines and their live status
  dshf user add       create a console account

Configuration is environment-only; see deployments/.env.example.
`)
}

func serve(_ []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := newLogger(cfg.LogLevel)

	// Signal handling wraps everything below so a shutdown during migration or
	// pool setup is still orderly.
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

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := pool.Ping(r.Context()); err != nil {
			http.Error(w, "database unreachable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("content-type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /uplink", func(w http.ResponseWriter, _ *http.Request) {
		// The node plugin already speaks the protocol in api/envelope.md; the
		// router that answers it is the next piece of work. Answering 501 keeps
		// a connecting node in its ordinary backoff loop instead of leaving it
		// to interpret a 404 as a misconfigured URL.
		http.Error(w, "uplink router not implemented", http.StatusNotImplemented)
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
