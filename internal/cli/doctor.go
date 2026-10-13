package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/config"
	"github.com/jamesagarside/hangar/internal/db"
)

// maxClockSkew is the skew doctor reports as a problem. Presigned URLs and
// Job leases tolerate less than a minute.
const maxClockSkew = 30 * time.Second

// checkResult is one doctor check.
type checkResult struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Message string `json:"message"`
	Latency int64  `json:"latency_ms"`
}

func (a *app) doctorCommand() *cobra.Command {
	var f adminFlags
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check the configuration, database, Blob store, presigned URLs, clock and git",
		Long: `Runs the same kind of checks as the readiness endpoint, with more detail:
the configuration, the database connection and schema version, the clock
against the database, a write, read and delete in the Blob store, presigned
downloads on S3, and the git version. Exits with 1 if any check fails.`,
		Args: args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			a := a.with(cmd)
			cfg, err := config.Load(config.LoadOptions{File: f.file, Set: f.set})
			results := a.doctor(cmd.Context(), cfg, err)
			failed := 0
			for _, r := range results {
				if !r.OK {
					failed++
				}
			}
			if a.json {
				if err := a.printJSON(results); err != nil {
					return err
				}
			} else {
				rows := make([][]string, 0, len(results))
				for _, r := range results {
					state := "ok"
					if !r.OK {
						state = "FAIL"
					}
					rows = append(rows, []string{r.Name, state, r.Message})
				}
				if err := a.table([]string{"CHECK", "RESULT", "DETAIL"}, rows); err != nil {
					return err
				}
			}
			if failed > 0 {
				return fmt.Errorf("%d of %d checks failed", failed, len(results))
			}
			return nil
		},
	}
	f.register(cmd, a)
	return cmd
}

// doctor runs every check it can. A failed check that later checks depend
// on stops them.
func (a *app) doctor(ctx context.Context, cfg *config.Config, loadErr error) []checkResult {
	var out []checkResult
	run := func(name string, fn func() (string, error)) bool {
		start := time.Now()
		msg, err := fn()
		r := checkResult{Name: name, OK: err == nil, Message: msg, Latency: time.Since(start).Milliseconds()}
		if err != nil {
			r.Message = err.Error()
		}
		out = append(out, r)
		return err == nil
	}
	if !run("configuration", func() (string, error) {
		if loadErr != nil {
			return "", loadErr
		}
		if err := cfg.Validate(); err != nil {
			return "", err
		}
		return "valid", nil
	}) {
		return out
	}
	var d *db.DB
	if run("database", func() (string, error) {
		var err error
		d, err = openDB(ctx, cfg)
		if err != nil {
			return "", err
		}
		return cfg.Database.Driver + " reachable", nil
	}) {
		defer d.Close()
		run("schema", func() (string, error) {
			st, err := d.Schema(ctx)
			if err != nil {
				return "", err
			}
			if !st.UpToDate() {
				return "", fmt.Errorf("at %q, %d migrations pending; run hangar admin migrate", st.Current, len(st.Pending))
			}
			return "at " + st.Current, nil
		})
		if !d.IsSQLite() {
			run("clock (database)", func() (string, error) {
				var now time.Time
				before := time.Now()
				if err := d.NewRaw("SELECT CURRENT_TIMESTAMP").Scan(ctx, &now); err != nil {
					return "", err
				}
				return skewMessage(now, before.Add(time.Since(before)/2))
			})
		}
	}
	store, err := a.openStore(ctx, cfg)
	if !run("blob store", func() (string, error) {
		if err != nil {
			return "", err
		}
		key := "tmp/doctor-" + uuid.NewString()
		body := []byte("hangar doctor")
		if err := store.Put(ctx, key, bytes.NewReader(body), int64(len(body))); err != nil {
			return "", fmt.Errorf("write: %w", err)
		}
		defer func() { _ = store.Delete(context.WithoutCancel(ctx), key) }()
		r, _, err := store.Get(ctx, key, nil)
		if err != nil {
			return "", fmt.Errorf("read: %w", err)
		}
		got, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil || !bytes.Equal(got, body) {
			return "", fmt.Errorf("read back different content")
		}
		info, err := store.Stat(ctx, key)
		if err == nil && !info.ModTime.IsZero() {
			if msg, err := skewMessage(info.ModTime, time.Now()); err != nil {
				return "", fmt.Errorf("write, read and delete work, but %w", err)
			} else if cfg.Storage.Backend == "s3" {
				return cfg.Storage.Backend + ": write, read and delete work; " + msg, nil
			}
		}
		return cfg.Storage.Backend + ": write, read and delete work", nil
	}) {
		return append(out, a.gitCheck())
	}
	if cfg.Storage.Backend == "s3" && cfg.Storage.S3.DownloadMode == "redirect" {
		run("presigned downloads", func() (string, error) {
			key := "tmp/doctor-presign-" + uuid.NewString()
			body := []byte("hangar doctor presign")
			if err := store.Put(ctx, key, bytes.NewReader(body), int64(len(body))); err != nil {
				return "", err
			}
			defer func() { _ = store.Delete(context.WithoutCancel(ctx), key) }()
			u, err := store.PresignGet(ctx, key, time.Minute, "doctor.txt")
			if errors.Is(err, blob.ErrNotSupported) {
				return "", fmt.Errorf("the store cannot presign; set storage.s3.download_mode to proxy")
			}
			if err != nil {
				return "", err
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
			if err != nil {
				return "", fmt.Errorf("the presigned URL is not valid")
			}
			resp, err := a.http.Do(req)
			if err != nil {
				return "", fmt.Errorf("fetch a presigned URL from this host: %w", redactURLError(err))
			}
			defer resp.Body.Close()
			got, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
			if resp.StatusCode != http.StatusOK || !bytes.Equal(got, body) {
				return "", fmt.Errorf("a presigned URL answered %d; clients will fail to download (check the endpoint, clock and bucket policy)", resp.StatusCode)
			}
			return "a presigned URL works from this host", nil
		})
	}
	return append(out, a.gitCheck())
}

// redactURLError drops the URL from an HTTP client error, so presigned
// URLs never reach the output.
func redactURLError(err error) error {
	var ue interface{ Unwrap() error }
	if errors.As(err, &ue) {
		if inner := ue.Unwrap(); inner != nil {
			return inner
		}
	}
	return errors.New("request failed")
}

// skewMessage compares a remote time with the local one.
func skewMessage(remote, local time.Time) (string, error) {
	skew := remote.Sub(local)
	if skew < 0 {
		skew = -skew
	}
	msg := "clock within " + skew.Round(time.Millisecond).String()
	if skew > maxClockSkew {
		return "", fmt.Errorf("clock skew of %s; synchronise the clocks with NTP", skew.Round(time.Second))
	}
	return msg, nil
}

var gitVersionRe = regexp.MustCompile(`git version (\d+)\.(\d+)`)

// gitCheck checks git 2.39 or later is installed (04-git.md).
func (a *app) gitCheck() checkResult {
	r := checkResult{Name: "git"}
	out, err := exec.Command("git", "--version").Output()
	if err != nil {
		r.Message = "git is not installed or not on PATH; the server needs git 2.39 or later"
		return r
	}
	m := gitVersionRe.FindSubmatch(out)
	if m == nil {
		r.Message = "cannot read the git version"
		return r
	}
	major, _ := strconv.Atoi(string(m[1]))
	minor, _ := strconv.Atoi(string(m[2]))
	v := string(bytes.TrimSpace(out))
	if major < 2 || major == 2 && minor < 39 {
		r.Message = v + "; the server needs 2.39 or later"
		return r
	}
	r.OK, r.Message = true, v
	return r
}
