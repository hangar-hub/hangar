package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/api/gen"
	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/config"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/httpx"
)

// SystemInfo is what /admin/system reports about the process.
type SystemInfo struct {
	Version string
	Commit  string
	// Role is all, web or worker.
	Role string
	// BlobBackend is fs or s3.
	BlobBackend string
	// DownloadMode is redirect or proxy; empty for the filesystem backend.
	DownloadMode string
	// UploadMode is direct or proxy; empty for the filesystem backend.
	UploadMode string
	// Telemetry is the exporter of each signal, from telemetry.Telemetry.Exporters.
	Telemetry map[string]string
}

// SystemCheck is a doctor check. Run returns a short message for the
// admin, or an error whose text explains what is wrong and how to fix it.
// Neither may contain credentials or presigned URLs.
type SystemCheck struct {
	Name string
	Run  func(ctx context.Context) (string, error)
}

// AdminSystem serves GET /admin/system: version, backends, telemetry and
// the doctor checks. It needs an Instance admin.
type AdminSystem struct {
	Authz  auth.Authz
	DB     *db.DB
	Info   SystemInfo
	Checks []SystemCheck
	// CheckTimeout bounds each check. Zero means 10 seconds.
	CheckTimeout time.Duration
}

// AdminGetSystem implements gen.ServerInterface.
func (h *AdminSystem) AdminGetSystem(w http.ResponseWriter, r *http.Request) {
	if _, ok := platformAdmin(w, r, h.Authz); !ok {
		return
	}
	out := gen.SystemInfo{Version: h.Info.Version, Role: h.Info.Role, Telemetry: map[string]string{}}
	if h.Info.Commit != "" {
		c := h.Info.Commit
		out.Commit = &c
	}
	for k, v := range h.Info.Telemetry {
		out.Telemetry[k] = v
	}
	out.BlobStore.Backend = h.Info.BlobBackend
	if h.Info.DownloadMode != "" {
		m := h.Info.DownloadMode
		out.BlobStore.DownloadMode = &m
	}
	if h.Info.UploadMode != "" {
		m := h.Info.UploadMode
		out.BlobStore.UploadMode = &m
	}
	out.Database.Driver = h.DB.Driver()
	if st, err := h.DB.Schema(r.Context()); err == nil {
		out.Database.SchemaVersion = st.Current
	}
	out.Checks = RunChecks(r.Context(), h.Checks, h.CheckTimeout)
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

// RunChecks runs the checks in parallel, each with its own timeout, and
// returns their results in order. `hangar admin doctor` uses it too.
func RunChecks(ctx context.Context, checks []SystemCheck, timeout time.Duration) []gen.CheckResult {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	out := make([]gen.CheckResult, len(checks))
	var wg sync.WaitGroup
	for i, c := range checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			start := time.Now()
			msg, err := runCheck(cctx, c)
			ms := int(time.Since(start).Milliseconds())
			name := c.Name
			res := gen.CheckResult{Name: &name, Ok: err == nil, Message: msg, LatencyMs: &ms}
			if err != nil {
				res.Message = err.Error()
				if errors.Is(err, context.DeadlineExceeded) {
					res.Message = fmt.Sprintf("The check did not finish within %s.", timeout)
				}
			}
			out[i] = res
		}()
	}
	wg.Wait()
	return out
}

func runCheck(ctx context.Context, c SystemCheck) (msg string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("the check failed unexpectedly: %v", r)
		}
	}()
	return c.Run(ctx)
}

// DoctorOptions configure DoctorChecks.
type DoctorOptions struct {
	// Store is the Blob store; nil leaves out the Blob store checks.
	Store blob.Store
	// Downloads is the download policy, for the presigned URL check.
	Downloads blob.Downloads
	// GitBinary is the git executable. Empty means "git"; "-" leaves the
	// check out, for worker-less test setups.
	GitBinary string
	// JobBacklog is how long a due Job may wait before the jobs check
	// fails. Zero means 10 minutes.
	JobBacklog time.Duration
	// HTTPClient fetches the presigned URL. Nil means a client with a
	// 10-second timeout.
	HTTPClient *http.Client
	// Config is the configuration the process runs with; nil leaves out the
	// config check.
	Config *config.Config
	// MaxClockSkew is how far this host's clock may differ from the
	// database's and the Blob store's before the clock check fails. Zero
	// means 10 seconds.
	MaxClockSkew time.Duration
	// Now is this host's clock. Nil means time.Now.
	Now func() time.Time
}

// MinGitVersion is the oldest git that Hangar supports (docs/spec/04-git.md).
var MinGitVersion = [3]int{2, 39, 0}

// DoctorChecks returns the standard checks: configuration, database,
// schema, clock skew, Blob store, presigned downloads, git and the Job
// backlog.
func DoctorChecks(d *db.DB, opts DoctorOptions) []SystemCheck {
	if opts.JobBacklog <= 0 {
		opts.JobBacklog = 10 * time.Minute
	}
	if opts.MaxClockSkew <= 0 {
		opts.MaxClockSkew = 10 * time.Second
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	checks := []SystemCheck{
		{Name: "database", Run: func(ctx context.Context) (string, error) {
			if err := d.PingContext(ctx); err != nil {
				return "", fmt.Errorf("the %s database is unreachable: %w", d.Driver(), err)
			}
			return "The " + d.Driver() + " database is reachable.", nil
		}},
		{Name: "schema", Run: func(ctx context.Context) (string, error) {
			st, err := d.Schema(ctx)
			if err != nil {
				return "", err
			}
			if !st.UpToDate() {
				return "", fmt.Errorf("%d migrations are pending; this binary expects %s. Run hangar admin migrate", len(st.Pending), st.Expected)
			}
			return "The schema is at " + st.Current + ".", nil
		}},
		{Name: "jobs", Run: func(ctx context.Context) (string, error) {
			late, err := d.NewSelect().Model((*db.Job)(nil)).Where("status = 'pending' AND run_after < ?", db.Now().Add(-opts.JobBacklog)).Count(ctx)
			if err != nil {
				return "", err
			}
			running, err := d.NewSelect().Model((*db.Job)(nil)).Where("status = 'running'").Count(ctx)
			if err != nil {
				return "", err
			}
			if late > 0 {
				return "", fmt.Errorf("%d Jobs have been due for more than %s. Check that a worker process (role all or worker) is running", late, opts.JobBacklog)
			}
			return fmt.Sprintf("%d Jobs are running and none are overdue.", running), nil
		}},
		{Name: "clock", Run: func(ctx context.Context) (string, error) {
			return checkClock(ctx, d, opts.Store, opts.Now, opts.MaxClockSkew)
		}},
	}
	if opts.Config != nil {
		cfg := opts.Config
		checks = append([]SystemCheck{{Name: "config", Run: func(context.Context) (string, error) {
			if err := cfg.Validate(); err != nil {
				return "", err
			}
			return "The configuration is valid.", nil
		}}}, checks...)
	}
	if opts.Store != nil {
		checks = append(checks,
			SystemCheck{Name: "blob_store", Run: func(ctx context.Context) (string, error) { return checkBlobStore(ctx, opts.Store) }},
			SystemCheck{Name: "presigned_downloads", Run: func(ctx context.Context) (string, error) {
				return checkPresign(ctx, opts.Store, opts.Downloads, opts.HTTPClient)
			}},
		)
	}
	if opts.GitBinary != "-" {
		bin := opts.GitBinary
		if bin == "" {
			bin = "git"
		}
		checks = append(checks, SystemCheck{Name: "git", Run: func(ctx context.Context) (string, error) { return checkGit(ctx, bin) }})
	}
	return checks
}

// skewOutside returns how far t lies outside the interval from before to
// after: negative when t is earlier, positive when later, zero inside.
func skewOutside(t, before, after time.Time) time.Duration {
	switch {
	case t.Before(before):
		return t.Sub(before)
	case t.After(after):
		return t.Sub(after)
	}
	return 0
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// checkClock compares this host's clock with the database server's
// (Postgres) and with the modification time the Blob store gives a new
// object (the S3 server, or the file server behind an NFS or SMB mount).
// Job leases, schedules, garbage collection grace periods and presigned
// URLs all need clocks that agree.
func checkClock(ctx context.Context, d *db.DB, s blob.Store, now func() time.Time, maxSkew time.Duration) (string, error) {
	var checked []string
	var problems []string
	if !d.IsSQLite() {
		before := now()
		var dbNow time.Time
		if err := d.NewRaw("SELECT CURRENT_TIMESTAMP").Scan(ctx, &dbNow); err != nil {
			return "", fmt.Errorf("reading the database clock failed: %w", err)
		}
		skew := skewOutside(dbNow, before, now())
		checked = append(checked, "the database")
		if absDuration(skew) > maxSkew {
			problems = append(problems, fmt.Sprintf("the database's clock is %s %s", absDuration(skew).Round(time.Second), aheadOrBehind(skew)))
		}
	}
	if s != nil {
		key := doctorKey()
		body := []byte("hangar clock check")
		before := now()
		if err := s.Put(ctx, key, bytes.NewReader(body), int64(len(body))); err != nil {
			return "", fmt.Errorf("writing to the Blob store failed: %w", err)
		}
		after := now()
		defer func() { _ = s.Delete(context.WithoutCancel(ctx), key) }()
		info, err := s.Stat(ctx, key)
		if err != nil {
			return "", fmt.Errorf("reading from the Blob store failed: %w", err)
		}
		// S3 and many file systems record whole seconds.
		skew := skewOutside(info.ModTime, before, after)
		checked = append(checked, "the Blob store")
		if absDuration(skew) > maxSkew+time.Second {
			problems = append(problems, fmt.Sprintf("the Blob store's clock is %s %s", absDuration(skew).Round(time.Second), aheadOrBehind(skew)))
		}
	}
	if len(problems) > 0 {
		return "", fmt.Errorf("%s this host's clock. Job leases, schedules, garbage collection and presigned URLs need synchronised clocks: run NTP on every host", strings.Join(problems, ", and "))
	}
	if len(checked) == 0 {
		return "SQLite uses this host's clock, and there is no Blob store to compare with.", nil
	}
	return fmt.Sprintf("This host's clock agrees with %s to within %s.", strings.Join(checked, " and "), maxSkew), nil
}

func aheadOrBehind(skew time.Duration) string {
	if skew > 0 {
		return "ahead of"
	}
	return "behind"
}

func doctorKey() string { return blob.PrefixTmp + "doctor-" + uuid.NewString() }

func checkBlobStore(ctx context.Context, s blob.Store) (string, error) {
	key := doctorKey()
	body := []byte("hangar doctor " + key)
	if err := s.Put(ctx, key, bytes.NewReader(body), int64(len(body))); err != nil {
		return "", fmt.Errorf("writing to the Blob store failed: %w", err)
	}
	defer func() { _ = s.Delete(context.WithoutCancel(ctx), key) }()
	rc, _, err := s.Get(ctx, key, nil)
	if err != nil {
		return "", fmt.Errorf("reading from the Blob store failed: %w", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		return "", fmt.Errorf("reading from the Blob store failed: %w", err)
	}
	if !bytes.Equal(got, body) {
		return "", errors.New("the Blob store returned different bytes from those written")
	}
	if err := s.Delete(ctx, key); err != nil {
		return "", fmt.Errorf("deleting from the Blob store failed: %w", err)
	}
	return "Writing, reading and deleting objects works.", nil
}

func checkPresign(ctx context.Context, s blob.Store, d blob.Downloads, client *http.Client) (string, error) {
	if !d.Redirects() {
		return "Downloads stream through Hangar (download_mode is proxy).", nil
	}
	d.Store = s
	key := doctorKey()
	body := []byte("hangar presign check")
	if err := s.Put(ctx, key, bytes.NewReader(body), int64(len(body))); err != nil {
		return "", fmt.Errorf("writing to the Blob store failed: %w", err)
	}
	defer func() { _ = s.Delete(context.WithoutCancel(ctx), key) }()
	u, ok, err := d.RedirectURL(ctx, key, "check.txt")
	if err != nil {
		return "", fmt.Errorf("presigning failed: %w", err)
	}
	if !ok {
		return "Downloads stream through Hangar: this Blob store cannot presign URLs.", nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", errors.New("the presigned URL is not valid")
	}
	resp, err := client.Do(req)
	if err != nil {
		// Never report the URL: it is a credential.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return "", fmt.Errorf("fetching a presigned URL from this server failed: %w. Clients must be able to reach the storage endpoint", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode != http.StatusOK || !bytes.Equal(got, body) {
		return "", fmt.Errorf("the storage endpoint answered a presigned URL with HTTP %d. Check the bucket's region, the clock and storage.s3.endpoint", resp.StatusCode)
	}
	return "Presigned download URLs work from this server.", nil
}

var gitVersionRe = regexp.MustCompile(`git version (\d+)\.(\d+)(?:\.(\d+))?`)

func checkGit(ctx context.Context, bin string) (string, error) {
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("could not run %s: %w. Install git 2.39 or later", bin, err)
	}
	m := gitVersionRe.FindStringSubmatch(string(out))
	if m == nil {
		return "", fmt.Errorf("could not read the git version from %q", strings.TrimSpace(string(out)))
	}
	var v [3]int
	for i := range 3 {
		v[i], _ = strconv.Atoi(m[i+1])
	}
	if versionLess(v, MinGitVersion) {
		return "", fmt.Errorf("git %d.%d.%d is too old. Install git %d.%d or later", v[0], v[1], v[2], MinGitVersion[0], MinGitVersion[1])
	}
	return fmt.Sprintf("git %d.%d.%d is installed.", v[0], v[1], v[2]), nil
}

func versionLess(a, b [3]int) bool {
	for i := range 3 {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}
