package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/jamesagarside/hangar/internal/blob/blobtest"
	"github.com/jamesagarside/hangar/internal/config"
	"github.com/jamesagarside/hangar/internal/telemetry"
)

func newServerCommand(use, short, forceRole string) *cobra.Command {
	var flags configFlags
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := flags.load(forceRole)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return serve(ctx, cfg, cmd.ErrOrStderr(), nil)
		},
	}
	flags.register(cmd)
	return cmd
}

// serve runs an Instance until ctx ends, then shuts down gracefully. ready,
// if not nil, receives the listening address once the server accepts
// connections.
func serve(ctx context.Context, cfg *config.Config, stderr io.Writer, ready chan<- string) error {
	// Validate before EnsureSecrets, which creates server.data_dir, so a bad
	// configuration reports every problem instead of a file system error.
	if err := cfg.ValidateBeforeSecrets(); err != nil {
		return err
	}
	gen, err := config.EnsureSecrets(cfg)
	if err != nil {
		return err
	}
	if _, err := cfg.InstanceKeyBytes(); err != nil {
		return fmt.Errorf("security.instance_key in %s: %w", filepath.Join(cfg.Server.DataDir, config.SecretsFileName), err)
	}
	if gen.Any() {
		printGenerated(stderr, gen, cfg)
	}

	tel, err := telemetry.Setup(ctx, telemetry.Options{Config: cfg.Telemetry, Version: version, Role: cfg.Server.Role})
	if err != nil {
		return fmt.Errorf("telemetry: %w", err)
	}
	defer func() { _ = tel.Shutdown(context.WithoutCancel(ctx)) }()
	log := tel.Logger

	d, err := openDatabase(ctx, cfg, true)
	if err != nil {
		return err
	}
	defer d.Close()
	if cfg.Server.Role == config.RoleAll {
		if err := d.Migrate(ctx); err != nil {
			return err
		}
	}

	// Phase 0: an in-memory Blob store until internal/blob's backends exist.
	blobs := blobtest.NewFake()
	if err := ensureSentinel(ctx, blobs); err != nil {
		return fmt.Errorf("blob store: %w", err)
	}

	deps := deps{cfg: cfg, log: log, db: d, blobs: blobs, authn: stubAuthenticator()}
	health := newHealth(deps)
	handler, err := newHandler(deps, health)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       5 * time.Minute,
	}
	ln, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Server.Listen, err)
	}
	log.InfoContext(ctx, "hangar started", "version", version, "role", cfg.Server.Role, "listen", ln.Addr().String(),
		"public_url", cfg.Server.PublicURL, "database", cfg.Database.Driver, "telemetry", tel.Exporters)

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	if ready != nil {
		ready <- ln.Addr().String()
	}

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
	}
	log.Info("shutting down", "timeout", cfg.Server.ShutdownTimeout.String())
	health.SetDraining()
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.Server.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		_ = srv.Close()
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

// printGenerated prints secrets generated on first start once, to stderr
// and not to the log, which may be exported.
func printGenerated(w io.Writer, gen *config.Generated, cfg *config.Config) {
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Hangar generated these secrets on first start and saved them in "+gen.Path+".")
	fmt.Fprintln(w, "They are shown once. Store them somewhere safe.")
	if gen.AdminPassword != "" {
		fmt.Fprintf(w, "  Admin username: %s\n  Admin password: %s\n", cfg.Security.BootstrapAdmin.Username, gen.AdminPassword)
	}
	if gen.InstanceKey != "" {
		fmt.Fprintf(w, "  Instance key:   %s\n", gen.InstanceKey)
	}
	fmt.Fprintln(w, "")
}
