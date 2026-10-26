package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
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

	"github.com/hangar-hub/hangar/internal/blob"
	"github.com/hangar-hub/hangar/internal/config"
	"github.com/hangar-hub/hangar/internal/db"
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
	var largeRead string
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check the configuration, database, Blob store, presigned URLs, clock and git",
		Long: `Runs the same kind of checks as the readiness endpoint, with more detail:
the configuration, the database connection and schema version, the clock
against the database, a write, read and delete in the Blob store, presigned
downloads on S3, and the git version. Exits with 1 if any check fails.

With --large-read, it also writes an object of that size (1100 MiB if no
size is given) to the Blob store and reads it back in one request at
16 MiB/s, more slowly than the store sends, as Hangar does when it checks
a large upload on a small server. A proxy in front of the store that cuts
long reads, such as nginx with its default proxy_max_temp_file_size of
1024 MiB, fails this check. It takes over a minute at the default size
and deletes the object afterwards.`,
		Example: `  hangar admin doctor --config /etc/hangar/hangar.yaml
  hangar admin doctor --config /etc/hangar/hangar.yaml --large-read
  hangar admin doctor --config /etc/hangar/hangar.yaml --large-read=3GiB`,
		Args: args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			a := a.with(cmd)
			var large int64
			if largeRead != "" {
				n, err := config.ParseByteSize(largeRead)
				if err != nil || n <= 0 {
					return usagef("--large-read takes a size such as 1100MiB or 3GiB")
				}
				large = n.Int64()
			}
			cfg, err := config.Load(config.LoadOptions{File: f.file, Set: f.set})
			if err == nil {
				// The Instance key generated on first start lives in
				// server.data_dir, as the server reads it.
				err = config.LoadStoredSecrets(cfg)
			}
			results := a.doctor(cmd.Context(), cfg, err, large)
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
	cmd.Flags().StringVar(&largeRead, "large-read", "", "also write an object of this size and read it back slowly in one request, to find a proxy that cuts long reads")
	cmd.Flags().Lookup("large-read").NoOptDefVal = defaultLargeRead
	return cmd
}

// defaultLargeRead is the size --large-read uses without a value: just
// over nginx's default proxy_max_temp_file_size of 1024 MiB.
const defaultLargeRead = "1100MiB"

// doctor runs every check it can. A failed check that later checks depend
// on stops them. largeRead is the size of the large read check's object,
// or 0 to skip it.
func (a *app) doctor(ctx context.Context, cfg *config.Config, loadErr error, largeRead int64) []checkResult {
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
				return "", fmt.Errorf("the database schema is at %q and this binary expects %q; run hangar admin migrate", st.Current, st.Expected)
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
	if largeRead > 0 {
		run("large reads", func() (string, error) {
			return largeReadCheck(ctx, store, largeRead, largeReadRate)
		})
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
			resp, err := redactingDoer{a.http}.Do(req)
			if err != nil {
				return "", fmt.Errorf("fetch a presigned URL from this host: %w", err)
			}
			defer resp.Body.Close()
			got, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
			if resp.StatusCode != http.StatusOK || !bytes.Equal(got, body) {
				return "", fmt.Errorf("a presigned URL answered %d; clients cannot download files (check the endpoint, clock and bucket policy)", resp.StatusCode)
			}
			return "a presigned URL works from this host", nil
		})
	}
	return append(out, a.gitCheck())
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

// largeReadRate is how fast the large read check reads, in bytes per
// second: more slowly than a store usually sends, so a proxy that buffers
// the response fills its temporary file, as it does when Hangar checks a
// large upload on a small server (bellatrix finding 13). Tests set it to
// 0, which reads as fast as the store sends.
var largeReadRate int64 = 16 << 20

// largeReadCheck writes size bytes of random content to the Blob store,
// reads them back in one unranged request at rate bytes per second and
// checks every byte arrived. A read that ends early means something
// between this host and the store cuts long reads. Hangar resumes such
// reads, so uploads still complete, but each cut costs a request and a
// wait, and the proxy should be fixed.
func largeReadCheck(ctx context.Context, store blob.Store, size, rate int64) (string, error) {
	block := make([]byte, 1<<20+7)
	if _, err := rand.Read(block); err != nil {
		return "", err
	}
	content := io.NewSectionReader(repeatReader(block), 0, size)
	want := sha256.New()
	if _, err := io.Copy(want, content); err != nil {
		return "", err
	}
	if _, err := content.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	key := "tmp/doctor-large-" + uuid.NewString()
	if err := store.Put(ctx, key, content, size); err != nil {
		return "", fmt.Errorf("write %s: %w", formatBytes(size), err)
	}
	defer func() { _ = store.Delete(context.WithoutCancel(ctx), key) }()
	start := time.Now()
	rc, _, err := store.Get(ctx, key, nil)
	if err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	got := sha256.New()
	n, err := io.Copy(got, &pacedReader{ctx: ctx, r: rc, rate: rate, start: start})
	_ = rc.Close()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if n < size {
		cause := "the response ended early"
		if err != nil {
			cause = err.Error()
		}
		return "", fmt.Errorf("a read of %s stopped after %s (%s): a proxy between Hangar and the Blob store cuts long reads; "+
			"for nginx, set proxy_buffering off and proxy_max_temp_file_size 0 (see Large files through nginx in the proxies guide)",
			formatBytes(size), formatBytes(n), cause)
	}
	if err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	if n != size || !bytes.Equal(got.Sum(nil), want.Sum(nil)) {
		return "", fmt.Errorf("read back different content")
	}
	return fmt.Sprintf("read %s back in one request in %s", formatBytes(size), time.Since(start).Round(time.Second)), nil
}

// repeatReader reads as its block repeated without end.
type repeatReader []byte

func (r repeatReader) ReadAt(p []byte, off int64) (int, error) {
	n := 0
	for n < len(p) {
		n += copy(p[n:], r[(off+int64(n))%int64(len(r)):])
	}
	return n, nil
}

// pacedReader reads from r no faster than rate bytes per second since
// start; rate 0 does not limit it.
type pacedReader struct {
	ctx   context.Context
	r     io.Reader
	rate  int64
	start time.Time
	n     int64
}

func (p *pacedReader) Read(b []byte) (int, error) {
	if len(b) > 1<<20 {
		b = b[:1<<20]
	}
	n, err := p.r.Read(b)
	p.n += int64(n)
	if p.rate > 0 && n > 0 {
		due := p.start.Add(time.Duration(float64(p.n) / float64(p.rate) * float64(time.Second)))
		if wait := time.Until(due); wait > 0 {
			if werr := waitRetry(p.ctx, wait); werr != nil {
				return n, werr
			}
		}
	}
	return n, err
}
