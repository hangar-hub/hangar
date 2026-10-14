package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/blob/backends"
	"github.com/jamesagarside/hangar/internal/config"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/telemetry"
)

func newServerCommand(use, short, forceRole string) *cobra.Command {
	var flags configFlags
	var jsonLogs bool
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := flags.loadServer(forceRole, jsonLogs)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return serve(ctx, cfg, cmd.ErrOrStderr(), nil)
		},
	}
	flags.register(cmd)
	cmd.Flags().BoolVar(&jsonLogs, "json", false, "write logs as JSON, the default (shortcut for --set telemetry.log_format=json)")
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
	blobs, err := openBlobStore(ctx, cfg, tel)
	if err != nil {
		return err
	}
	health := newHealth(d, blobs)

	// Health answers at once; the rest of the Instance is built once the
	// schema is current, which web and worker processes wait for while
	// hangar admin migrate runs.
	var current atomic.Pointer[http.Handler]
	starting := httpx.RequestIDs(httpx.Dispatch(httpx.Protocols{Health: health}, cfg.Server.PublicURL))
	current.Store(&starting)
	srv := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { (*current.Load()).ServeHTTP(w, r) }),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       5 * time.Minute,
	}
	ln, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Server.Listen, err)
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	shutdown := func() error {
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

	if err := waitForSchema(ctx, d, log); err != nil {
		_ = shutdown()
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	in, err := newInstance(ctx, cfg, tel, d, blobs)
	if err != nil {
		_ = shutdown()
		return err
	}
	defer in.close(context.WithoutCancel(ctx))
	if err := in.registry.SyncConfig(ctx, cfg.Upstreams); err != nil {
		_ = shutdown()
		return fmt.Errorf("upstreams: %w", err)
	}
	workerOnly := cfg.Server.Role == config.RoleWorker
	if !workerOnly {
		if _, _, err := in.auth.Bootstrap(ctx); err != nil {
			_ = shutdown()
			return fmt.Errorf("bootstrap admin: %w", err)
		}
	}
	handler, err := newHandler(ctx, in, health, workerOnly)
	if err != nil {
		_ = shutdown()
		return err
	}
	current.Store(&handler)

	// Jobs run in roles all and worker. On shutdown the worker returns
	// running Jobs to the queue.
	workerCtx, stopWorker := context.WithCancel(context.WithoutCancel(ctx))
	workerDone := make(chan struct{})
	if cfg.Server.Role != config.RoleWeb {
		w := in.newWorker()
		go func() {
			defer close(workerDone)
			in.refreshStale(workerCtx)
			_ = w.Run(workerCtx)
		}()
	} else {
		close(workerDone)
	}
	log.InfoContext(ctx, "hangar started", "version", version, "role", cfg.Server.Role, "listen", ln.Addr().String(),
		"public_url", cfg.Server.PublicURL, "database", cfg.Database.Driver, "blob_store", blobBackend(cfg), "telemetry", tel.Exporters)
	if ready != nil {
		ready <- ln.Addr().String()
	}

	var serveErr error
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			serveErr = err
		}
	case <-ctx.Done():
		serveErr = shutdown()
	}
	stopWorker()
	<-workerDone
	return serveErr
}

// waitForSchema waits until the database schema is current, so a web or
// worker process can start while hangar admin migrate runs.
func waitForSchema(ctx context.Context, d *db.DB, log *slog.Logger) error {
	logged := false
	for {
		st, err := d.Schema(ctx)
		if err == nil && st.UpToDate() {
			return nil
		}
		if !logged {
			if err != nil {
				log.WarnContext(ctx, "waiting for the database", "error", err.Error())
			} else {
				log.WarnContext(ctx, "waiting for hangar admin migrate", "schema", st.Current, "expected", st.Expected)
			}
			logged = true
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// openBlobStore opens the configured Blob store with telemetry and writes
// the sentinel that /readyz reads.
func openBlobStore(ctx context.Context, cfg *config.Config, tel *telemetry.Telemetry) (blob.Store, error) {
	store, err := backends.Open(ctx, cfg.Storage)
	if err != nil {
		return nil, fmt.Errorf("blob store: %w", err)
	}
	blobs := blob.Instrument(store, blobBackend(cfg), tel.Tracer(), tel.Instruments.BlobOperationDuration)
	if err := ensureSentinel(ctx, blobs); err != nil {
		return nil, fmt.Errorf("blob store: %w", err)
	}
	return blobs, nil
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
