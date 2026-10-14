package main

import (
	"github.com/spf13/cobra"

	"github.com/jamesagarside/hangar/internal/config"
)

// configFlags are the flags every command that reads the configuration takes.
type configFlags struct {
	file      string
	set       []string
	role      string
	listen    string
	publicURL string
}

func (f *configFlags) register(cmd *cobra.Command) {
	fl := cmd.Flags()
	fl.StringVarP(&f.file, "config", "c", "", "YAML configuration file (or set HANGAR_CONFIG)")
	fl.StringArrayVar(&f.set, "set", nil, "set a configuration key, such as --set storage.backend=s3 (repeatable)")
	fl.StringVar(&f.role, "role", "", "shortcut for --set server.role=...")
	fl.StringVar(&f.listen, "listen", "", "shortcut for --set server.listen=...")
	fl.StringVar(&f.publicURL, "public-url", "", "shortcut for --set server.public_url=...")
}

// loadServer is load for hangar server and worker, whose output is their
// logs: --json selects JSON logs, overriding telemetry.log_format.
func (f *configFlags) loadServer(forceRole string, jsonLogs bool) (*config.Config, error) {
	cfg, err := f.load(forceRole)
	if err != nil || !jsonLogs {
		return cfg, err
	}
	cfg.Telemetry.LogFormat = "json"
	return cfg, nil
}

// load reads the configuration with flag overrides applied last.
func (f *configFlags) load(forceRole string) (*config.Config, error) {
	set := append([]string{}, f.set...)
	if f.role != "" {
		set = append(set, "server.role="+f.role)
	}
	if forceRole != "" {
		set = append(set, "server.role="+forceRole)
	}
	if f.listen != "" {
		set = append(set, "server.listen="+f.listen)
	}
	if f.publicURL != "" {
		set = append(set, "server.public_url="+f.publicURL)
	}
	return config.Load(config.LoadOptions{File: f.file, Set: set})
}
