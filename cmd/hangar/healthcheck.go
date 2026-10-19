package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/hangar-hub/hangar/internal/httpx"
)

// readiness is the --json output of hangar healthcheck.
type readiness struct {
	Ready bool   `json:"ready"`
	Error string `json:"error,omitempty"`
}

func newHealthcheckCommand() *cobra.Command {
	var flags configFlags
	var asJSON bool
	var retryFor time.Duration
	cmd := &cobra.Command{
		Use:   "healthcheck",
		Short: "Call /readyz on this host and exit 0 when ready",
		Long: "Calls /readyz on the local listen address. The container image uses it as its HEALTHCHECK.\n\n" +
			"With --retry-for, it tries again while the connection fails, for up to that long. A check\n" +
			"through a Kubernetes Service needs this: the Service starts routing to a new Pod a moment\n" +
			"after the Pod is ready, and until then it refuses connections. An answer from /readyz,\n" +
			"ready or not, is never retried.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := flags.load("")
			if err != nil {
				return err
			}
			err = checkReadyRetrying(cmd.Context(), cfg.Server.Listen, retryFor)
			if asJSON {
				r := readiness{Ready: err == nil}
				if err != nil {
					r.Error = err.Error()
				}
				if jerr := writeJSON(cmd.OutOrStdout(), r); jerr != nil {
					return jerr
				}
			}
			return err
		},
	}
	flags.register(cmd)
	addJSONFlag(cmd, &asJSON)
	cmd.Flags().DurationVar(&retryFor, "retry-for", 0, "keep trying for this long while the connection fails, such as 30s (0 tries once)")
	return cmd
}

// healthcheckRetryInterval is how long hangar healthcheck --retry-for waits
// between tries.
const healthcheckRetryInterval = 500 * time.Millisecond

// errNotReadyAnswer is a /readyz answer other than 200: the server was
// reached and said it is not ready, so --retry-for does not try again.
type errNotReadyAnswer struct{ status string }

func (e *errNotReadyAnswer) Error() string { return "not ready: /readyz answered " + e.status }

// checkReadyRetrying calls checkReady, and while the request fails without
// an answer from /readyz (connection refused, no route, a timeout), tries
// again every healthcheckRetryInterval until retryFor has passed. With
// retryFor 0 it tries once, as the image HEALTHCHECK needs.
func checkReadyRetrying(ctx context.Context, listen string, retryFor time.Duration) error {
	if ctx == nil {
		ctx = context.Background()
	}
	deadline := time.Now().Add(retryFor)
	for {
		err := checkReady(ctx, listen)
		var answered *errNotReadyAnswer
		if err == nil || errors.As(err, &answered) || time.Now().Add(healthcheckRetryInterval).After(deadline) {
			return err
		}
		t := time.NewTimer(healthcheckRetryInterval)
		select {
		case <-ctx.Done():
			t.Stop()
			return err
		case <-t.C:
		}
	}
}

// readyzWait is how long hangar healthcheck waits for /readyz. It is longer
// than httpx.ReadyTimeout, so a slow but healthy dependency still answers,
// and shorter than the image HEALTHCHECK --timeout, so the command reports
// rather than being killed.
const readyzWait = httpx.ReadyTimeout + 2*time.Second

// checkReady calls /readyz on the loopback address of listen.
func checkReady(ctx context.Context, listen string) error {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("server.listen %q: %w", listen, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, readyzWait)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(host, port)+"/readyz", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("not ready: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &errNotReadyAnswer{status: resp.Status}
	}
	return nil
}
