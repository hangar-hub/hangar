package repo

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/jobs"
	"github.com/jamesagarside/hangar/internal/model"
)

// ZipJob is the payload of a zip.export Job (DOWN-5).
type ZipJob struct {
	RepoUUID uuid.UUID `json:"repo_uuid"`
	// Repo is the Repo ID, for Job listings.
	Repo   string          `json:"repo,omitempty"`
	Commit model.CommitSHA `json:"commit"`
	// Path limits the zip to one folder; "" is the whole Revision.
	Path string `json:"path,omitempty"`
}

// ZipResult is the result of a zip.export Job.
type ZipResult struct {
	Key      string `json:"key"`
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
	Files    int    `json:"files"`
	// URL downloads the zip; it expires with the export.
	URL string `json:"url,omitempty"`
}

// PurgeJob is the payload of a repo.purge Job. DeletedAt ties the Job to
// one deletion, so a Repo that was restored and deleted again is purged by
// the newer Job only.
type PurgeJob struct {
	RepoUUID  uuid.UUID `json:"repo_uuid"`
	DeletedAt time.Time `json:"deleted_at"`
}

// ExportKey returns the Blob store key of a zip export.
func ExportKey(jobID uuid.UUID) string { return blob.PrefixExports + jobID.String() + ".zip" }

// ExportTTL is how long zip exports are kept (02-architecture.md "Blob
// store layout").
const ExportTTL = 24 * time.Hour

// JobOptions configure the Repo service's Job handlers.
type JobOptions struct {
	// Blobs stores zip exports.
	Blobs blob.Store
	// ExportURL returns the download URL of a finished export, such as a
	// presigned URL or a signed Hangar URL.
	ExportURL func(ctx context.Context, key, filename string, ttl time.Duration) (string, error)
	// PackGracePeriod is git.pack_grace_period, for the pack sweep after
	// compaction. Zero means one hour.
	PackGracePeriod time.Duration
	// UploadsExpiredElsewhere leaves upload.expire out, for processes where
	// internal/gc registers it (gc.Register, with its own schedule). A
	// Registry takes each kind once.
	UploadsExpiredElsewhere bool
}

// RegisterJobs registers the handlers for zip.export, repo.purge,
// git.compact and, unless o.UploadsExpiredElsewhere is set, upload.expire.
func (s *Store) RegisterJobs(reg *jobs.Registry, lfs *LFS, o JobOptions) error {
	if o.Blobs == nil {
		return errors.New("repo: JobOptions.Blobs is required")
	}
	if o.PackGracePeriod <= 0 {
		o.PackGracePeriod = time.Hour
	}
	regs := []jobs.Registration{
		{Kind: jobs.KindZipExport, Handler: func(ctx context.Context, j *jobs.Job, p jobs.Progress) error {
			return s.runZipExport(ctx, j, p, o)
		}, Options: jobs.HandlerOptions{MaxAttempts: 3}},
		{Kind: jobs.KindRepoPurge, Handler: s.runPurge, Options: jobs.HandlerOptions{MaxAttempts: 10}},
		{Kind: jobs.KindGitCompact, Handler: func(ctx context.Context, j *jobs.Job, _ jobs.Progress) error {
			var pl RepoJob
			if err := j.DecodePayload(&pl); err != nil {
				return jobs.Permanent(err)
			}
			if _, err := s.Compact(ctx, pl.RepoUUID); err != nil {
				return err
			}
			_, err := s.git.Sweep(ctx, o.PackGracePeriod)
			return err
		}, Options: jobs.HandlerOptions{MaxAttempts: 3, Concurrency: 1}},
	}
	if !o.UploadsExpiredElsewhere {
		regs = append(regs, jobs.Registration{Kind: jobs.KindUploadExpire, Handler: func(ctx context.Context, _ *jobs.Job, _ jobs.Progress) error {
			_, err := lfs.ExpireUploads(ctx)
			return err
		}})
	}
	for _, r := range regs {
		if err := reg.Register(r.Kind, r.Handler, r.Options); err != nil {
			return err
		}
	}
	return nil
}

// Schedules returns the recurring Jobs the Repo service needs.
func Schedules() []jobs.Schedule {
	return []jobs.Schedule{{Kind: jobs.KindUploadExpire, Key: "uploads", Cron: "17 * * * *", Enabled: true}}
}

// Compact repacks a Repo's packs when it has more than git.compact_threshold.
func (s *Store) Compact(ctx context.Context, repoUUID uuid.UUID) (bool, error) {
	row, err := s.byUUID(ctx, s.db, repoUUID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return s.git.Compact(ctx, row.ID, row.HeadRef, s.opts.CompactThreshold)
}

func (s *Store) runPurge(ctx context.Context, j *jobs.Job, _ jobs.Progress) error {
	var pl PurgeJob
	if err := j.DecodePayload(&pl); err != nil {
		return jobs.Permanent(err)
	}
	row, err := s.byUUID(ctx, s.db, pl.RepoUUID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if row.DeletedAt == nil || !row.DeletedAt.Equal(pl.DeletedAt) {
		// Restored, or deleted again with a newer Job.
		return nil
	}
	if due := row.DeletedAt.Add(s.opts.RestoreWindow); s.now().Before(due) {
		return fmt.Errorf("repo %s cannot be purged before %s", pl.RepoUUID, due.Format(time.RFC3339))
	}
	return s.Purge(ctx, pl.RepoUUID)
}

// runZipExport writes the files of a Commit (or one folder of it) into a
// zip in the Blob store, streaming: no file or zip is held in memory or
// on local disk.
func (s *Store) runZipExport(ctx context.Context, j *jobs.Job, prog jobs.Progress, o JobOptions) error {
	var pl ZipJob
	if err := j.DecodePayload(&pl); err != nil {
		return jobs.Permanent(err)
	}
	row, err := s.byUUID(ctx, s.db, pl.RepoUUID)
	if err != nil {
		return jobs.Permanent(err)
	}
	r := toRepo(row)
	dir, err := model.CleanRepoPath(pl.Path)
	if err != nil {
		return jobs.Permanent(fmt.Errorf("%w: %w", ErrInvalid, err))
	}
	entries, err := s.Tree(ctx, r.ID, pl.Commit, dir, TreeOpts{Recursive: true})
	if err != nil {
		return jobs.Permanent(err)
	}
	var files []model.Entry
	var total int64
	for _, e := range entries {
		if e.Type != model.EntryFile {
			continue
		}
		if e.LFS != nil && !e.Present {
			continue
		}
		// Pushes and Pulls refuse such paths (checkTrees), but a Commit
		// stored before that check could hold one, and an entry named
		// "../x" or "a\\..\\x" would extract outside the folder.
		if clean, err := model.CleanRepoPath(e.Path); err != nil || clean != e.Path {
			return jobs.Permanent(fmt.Errorf("%w: the Commit has a file whose path cannot be extracted safely: %q", ErrInvalid, e.Path))
		}
		files = append(files, e)
		total += e.Size
	}
	filename := r.ID.Name + "-" + pl.Commit.Short()
	if dir != "" {
		filename += "-" + strings.ReplaceAll(dir, "/", "-")
	}
	filename += ".zip"
	key := ExportKey(j.ID)

	pr, pw := io.Pipe()
	counter := &countingReader{r: pr}
	putErr := make(chan error, 1)
	go func() {
		err := o.Blobs.Put(ctx, key, counter, -1)
		_ = pr.CloseWithError(err)
		putErr <- err
	}()
	var done int64
	werr := func() error {
		zw := zip.NewWriter(pw)
		last := time.Now()
		for i, e := range files {
			name := e.Path
			if dir != "" {
				name = strings.TrimPrefix(e.Path, dir+"/")
			}
			name = path.Join(r.ID.Name, name)
			w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: s.now()})
			if err != nil {
				return err
			}
			rc, _, err := s.openFile(ctx, row.ID, pl.Commit, e.Path, nil)
			if err != nil {
				return err
			}
			n, err := io.Copy(w, rc)
			_ = rc.Close()
			if err != nil {
				return err
			}
			done += n
			if time.Since(last) > 2*time.Second || i == len(files)-1 {
				last = time.Now()
				if err := prog.Report(ctx, jobs.ProgressUpdate{BytesDone: done, BytesTotal: total, FilesDone: i + 1, FilesTotal: len(files)}); err != nil {
					return err
				}
			}
		}
		return zw.Close()
	}()
	_ = pw.CloseWithError(werr)
	if err := <-putErr; err != nil {
		if werr != nil {
			return werr
		}
		return fmt.Errorf("repo: store zip export: %w", err)
	}
	if werr != nil {
		_ = o.Blobs.Delete(context.WithoutCancel(ctx), key)
		return werr
	}
	res := ZipResult{Key: key, Filename: filename, Size: counter.n, Files: len(files)}
	if o.ExportURL != nil {
		u, err := o.ExportURL(ctx, key, filename, ExportTTL)
		if err != nil {
			return err
		}
		res.URL = u
	}
	return prog.SetResult(ctx, res)
}

// purgeDedupe is the repo.purge dedupe key of one deletion.
func purgeDedupe(id uuid.UUID, deletedAt time.Time) string {
	return id.String() + ":" + strconv.FormatInt(deletedAt.UnixNano(), 10)
}
