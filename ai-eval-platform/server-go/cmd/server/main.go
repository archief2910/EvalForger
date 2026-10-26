// Command server runs the EvalForge API, the evaluation dispatcher and (with
// --mcp) the stdio MCP server. All wiring lives here; internal packages expose
// constructors and know nothing about process flags.
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

	"evalforge/internal/api"
	"evalforge/internal/config"
	"evalforge/internal/database"
	"evalforge/internal/logging"
	"evalforge/internal/queue"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "evalforge:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	// --create-key prints the only copy of a secret to stdout; keep structured
	// logs off that stream so a shell capture cannot corrupt the key.
	if hasFlag(args, "--create-key") {
		cfg.LogLevel = slog.LevelError
	}
	log := logging.New(cfg.LogLevel)

	// --mcp is an API client, so it must not open the database. This also keeps
	// the binary usable on a machine that only has MCP credentials.
	if hasFlag(args, "--mcp") {
		base := cfg.MCPAPIBase
		if v, ok := flagValue(args, "--api-base"); ok {
			base = v
		}
		api.RunMCP(base, cfg.MCPAPIKey)
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Two independent SQLite files. OpenStores migrates both, adopts the old
	// single-file layout on first boot and backfills conversation threads.
	stores, err := database.OpenStores(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer stores.Control.Close()
	defer stores.Telemetry.Close()

	server := api.New(stores.Control, stores.Telemetry, cfg, log)

	// --create-key bootstraps the first credential of an auth-required
	// deployment without opening an HTTP port. The key goes to stdout so it can
	// be captured; the diagnostic line goes to stderr.
	if hasFlag(args, "--create-key") {
		return createKey(server, args)
	}

	q := queue.New(stores.Control, server.WriteMutex(), log, cfg.JobLease, server.RunJob, server.RetireExperiment)
	server.AttachQueue(q)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	background := server.Start(ctx)

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      5 * time.Minute, // long dashboard/compare queries
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("server listening", "addr", cfg.Addr,
			"control_db", cfg.ControlDBPath, "telemetry_db", cfg.TelemetryDBPath, "auth", cfg.AuthMode)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case sig := <-sigCh:
		log.Info("shutdown signal received", "signal", sig.String())
	}

	// Stop accepting work, drain in-flight requests, then let the dispatcher
	// release its lease before the pool closes.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Warn("http shutdown", "err", err)
	}
	cancel()

	done := make(chan struct{})
	go func() { background.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		log.Warn("dispatcher still stopping after 15s")
	}
	log.Info("shutdown complete")
	return nil
}

// createKey implements `evalforge --create-key [name] [--owner X] [--project Y]`.
func createKey(server *api.Server, args []string) error {
	name, owner, projectID := "", "", ""
	scopes := map[string]bool{"read": true, "write": true, "admin": true}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--create-key", "--owner", "--project":
			if args[i] == "--create-key" {
				continue
			}
			if i+1 < len(args) {
				i++
				switch args[i-1] {
				case "--owner":
					owner = args[i]
				case "--project":
					projectID = args[i]
				}
			}
		default:
			if name == "" && !strings.HasPrefix(args[i], "--") {
				name = args[i]
			}
		}
	}
	if name == "" {
		name = "bootstrap"
	}
	raw, id, prefix, err := server.CreateAPIKey(name, owner, projectID, scopes, 0)
	if err != nil {
		return fmt.Errorf("create key: %w", err)
	}
	fmt.Fprintf(os.Stderr, "created %s (%s.) for owner %q; stdout carries the key\n", id, prefix, owner)
	fmt.Println(raw)
	return nil
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func flagValue(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}
