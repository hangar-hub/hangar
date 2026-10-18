package main

import (
	"context"
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
	cmd := &cobra.Command{
		Use:   "healthcheck",
		Short: "Call /readyz on this host and exit 0 when ready",
		Long:  "Calls /readyz on the local listen address. The container image uses it as its HEALTHCHECK.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := flags.load("")
			if err != nil {
				return err
			}
			err = checkReady(cmd.Context(), cfg.Server.Listen)
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
	return cmd
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
		return fmt.Errorf("not ready: /readyz answered %s", resp.Status)
	}
	return nil
}
