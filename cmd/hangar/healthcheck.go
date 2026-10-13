package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/spf13/cobra"
)

func newHealthcheckCommand() *cobra.Command {
	var flags configFlags
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
			return checkReady(cmd.Context(), cfg.Server.Listen)
		},
	}
	flags.register(cmd)
	return cmd
}

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
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
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
