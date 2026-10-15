package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/jobs"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/org"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/upstream/deps"
	"github.com/jamesagarside/hangar/internal/webhook"
)

// Pull phases, recorded in the pull Job's result.
const (
	phaseTransfer = "transferring"
	phaseDeps     = "dependencies"
	phaseDone     = "done"
	phasePartial  = "partial"
	phaseFailed   = "failed"
)

// refConflictMessage is the progress message of a Pull that plans again
// because the Mirror's history changed while it stored it.
const refConflictMessage = "Planning again: the Mirror's history changed while this Pull stored it"

// maxRefConflicts bounds how many times a Pull plans again at once after
// a ref conflict.
const maxRefConflicts = 5

// Failure reasons in a failed Pull's result.
const (
	reasonUpstreamNotFound = "upstream_not_found"
	reasonConflict         = "conflict"
)

// fileState tracks one selected LFS file of a Pull.
type fileState struct {
	Path      string    `json:"path"`
	OID       string    `json:"oid"`
	Size      int64     `json:"size"`
	Job       uuid.UUID `json:"job,omitempty"`
	Status    string    `json:"status"`
	BytesDone int64     `json:"bytes_done,omitempty"`
	Error     string    `json:"error,omitempty"`
	// Reason is reasonQuotaExceeded when the Namespace quota refused the file.
	Reason string `json:"reason,omitempty"`
}

// depState tracks a Dependency Pull started by a Pull.
type depState struct {
	Repo   string         `json:"repo"`
	Type   model.RepoType `json:"type"`
	Kind   deps.Kind      `json:"kind"`
	Job    uuid.UUID      `json:"job,omitempty"`
	Status string         `json:"status"`
	Error  string         `json:"error,omitempty"`
}

// pullState is the pull Job's result: where the Pull is and per-file detail.
type pullState struct {
	Phase    string          `json:"phase"`
	Reason   string          `json:"reason,omitempty"`
	Repo     string          `json:"repo,omitempty"`
	RepoUUID uuid.UUID       `json:"repo_uuid,omitempty"`
	Commit   model.CommitSHA `json:"commit,omitempty"`
	// SourceCommit is the Revision's Commit; Commit differs from it for
	// Parquet-only Pulls, whose files come from refs/convert/parquet.
	SourceCommit model.CommitSHA `json:"source_commit,omitempty"`
	Files        []fileState     `json:"files,omitempty"`
	// AlreadyPresent counts selected LFS files that needed no transfer.
	AlreadyPresent int        `json:"already_present"`
	Dependencies   []depState `json:"dependencies,omitempty"`
	Warnings       []string   `json:"warnings,omitempty"`
	// RefConflicts counts the times planning met another run storing the
	// Mirror's history and planned again at once.
	RefConflicts int `json:"ref_conflicts,omitempty"`
}

// File statuses in pullState.
const (
	fileQueued    = "queued"
	fileRunning   = "running"
	fileDone      = "done"
	fileFailed    = "failed"
	filePresent   = "present"
	fileCancelled = "cancelled"
)

// runPull is the pull Job handler. A Pull plans once, then re-runs every
// PollInterval (snoozed, so no attempt is used) until its file transfers
// and Dependency Pulls finish, so one Job is the whole Pull: cancelling it
// cancels its transfers, and GET /pulls/{id} shows its progress.
func (s *Service) runPull(ctx context.Context, j *jobs.Job, prog jobs.Progress) error {
	var p pullPayload
	if err := j.DecodePayload(&p); err != nil {
		return jobs.Permanent(err)
	}
	var st pullState
	if len(j.Result) > 0 {
		if err := json.Unmarshal(j.Result, &st); err != nil {
			return jobs.Permanent(fmt.Errorf("decode pull state: %w", err))
		}
	}
	if st.Phase == "" || st.Phase == phaseFailed {
		st = pullState{RefConflicts: st.RefConflicts}
		if err := s.plan(ctx, j, prog, &p, &st); err != nil {
			if errors.Is(err, repo.ErrRefConflict) && st.RefConflicts < maxRefConflicts {
				// Another run stored history in this Mirror at the same
				// time: a sync, another Pull of it, or a second run of
				// this Job after a lost lease. Planning again sees that
				// history, so it is not a failure to back off from. The
				// count stops a conflict that keeps coming back from
				// planning again for ever: past it, the Job's backoff
				// applies.
				st = pullState{RefConflicts: st.RefConflicts + 1}
				if serr := prog.SetResult(ctx, st); serr != nil {
					return serr
				}
				return s.snooze(s.opts.Now().Add(s.opts.PollInterval), refConflictMessage)
			}
			return s.failPull(ctx, prog, &p, &st, err)
		}
	}
	return s.advance(ctx, j, prog, &p, &st)
}

// failPull records why a Pull failed. Errors the Upstream or the request
// caused are permanent; others are retried with backoff.
func (s *Service) failPull(ctx context.Context, prog jobs.Progress, p *pullPayload, st *pullState, err error) error {
	permanent := jobs.IsPermanent(err)
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrGated), errors.Is(err, ErrUnauthorized):
		st.Reason, permanent = reasonUpstreamNotFound, true
	case errors.Is(err, ErrConflict), errors.Is(err, ErrInvalidSelection), errors.Is(err, ErrUpstreamDisabled), errors.Is(err, ErrUpstreamNotFound):
		st.Reason, permanent = reasonConflict, true
	case errors.Is(err, ErrQuotaExceeded):
		st.Reason, permanent = reasonQuotaExceeded, true
	}
	if !permanent {
		return err
	}
	st.Phase = phaseFailed
	if serr := prog.SetResult(ctx, st); serr != nil {
		s.log.WarnContext(ctx, "record pull failure", "error", serr.Error())
	}
	if st.Reason == reasonQuotaExceeded {
		u := s.pullProgress(ctx, st)
		u.Message = quotaMessage
		_ = prog.Report(ctx, u)
	}
	var r *repo.Repo
	if st.RepoUUID != uuid.Nil {
		r, _ = s.opts.Repos.GetByUUID(ctx, st.RepoUUID)
	}
	s.emit(ctx, webhook.PullFailed, model.System(), r, map[string]any{"upstream": p.Upstream, "upstream_repo": p.UpstreamRepo, "error": err.Error()})
	if r != nil {
		_, _ = s.mirrors.update(ctx, r.UUID, r.ID.Type, func(m *Mirror) error {
			m.Status, m.LastError = MirrorFailed, truncate(err.Error(), 1000)
			return nil
		})
	}
	return jobs.Permanent(err)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// plan is step 1 of 06 "Pull lifecycle": resolve the Revision, fetch
// history, list and select files, mark what the Instance already holds,
// and queue a pull.file Job for each missing object.
func (s *Service) plan(ctx context.Context, j *jobs.Job, prog jobs.Progress, p *pullPayload, st *pullState) error {
	upID, err := p.upstreamID()
	if err != nil {
		return jobs.Permanent(err)
	}
	up, err := s.opts.Registry.Get(ctx, p.UpstreamID)
	if err != nil {
		return err
	}
	if !up.Enabled {
		return fmt.Errorf("%w: %s", ErrUpstreamDisabled, up.Name)
	}
	cred, err := s.credentials(ctx, up, p.RequestedBy)
	if err != nil {
		return err
	}
	_ = prog.Report(ctx, jobs.ProgressUpdate{Message: "Resolving " + upID.Path() + " on " + up.Host()})
	info, err := s.opts.Client.RepoInfo(ctx, *up, cred, upID, p.Revision)
	if err != nil {
		return err
	}
	r, err := s.ensureMirror(ctx, up, p, upID, info)
	if err != nil {
		return err
	}
	st.Repo, st.RepoUUID, st.SourceCommit = r.ID.String(), r.UUID, info.SHA
	s.emit(ctx, webhook.PullStarted, model.System(), r, map[string]any{"upstream": up.Name, "upstream_repo": upID.String(), "revision": p.Revision,
		"commit": string(info.SHA), "job_id": j.ID.String()})
	_ = prog.Report(ctx, jobs.ProgressUpdate{Message: "Fetching history"})
	if err := s.fetchHistory(ctx, up, cred, r, upID, p, info.SHA); err != nil {
		return err
	}
	commit := info.SHA
	if p.Selection.ParquetOnly {
		commit, err = s.opts.Repos.Resolve(ctx, r.ID, "refs/convert/parquet")
		if err != nil {
			return fmt.Errorf("%w: %s has no Parquet export on the Upstream (refs/convert/parquet)", ErrInvalidSelection, upID.Path())
		}
	}
	st.Commit = commit
	entries, err := s.opts.Repos.Tree(ctx, r.ID, commit, "", repo.TreeOpts{Recursive: true})
	if err != nil {
		return fmt.Errorf("list files at %s: %w", commit.Short(), err)
	}
	sizes := map[string]int64{}
	for _, e := range entries {
		if e.Type == model.EntryFile {
			sizes[e.Path] = e.Size
		}
	}
	for _, path := range p.Selection.Paths {
		if _, ok := sizes[path]; !ok {
			return fmt.Errorf("%w: %s does not exist at %s", ErrInvalidSelection, path, info.SHA.Short())
		}
	}
	pats, err := DatasetPatterns(ctx, s.opts.Layout, r.ID, info.SHA, p.Selection)
	if err != nil {
		return err
	}
	matcher, err := NewMatcher(p.Selection, sizes, pats)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidSelection, err)
	}
	var selected []model.Entry
	var oids []string
	for _, e := range entries {
		if e.Type == model.EntryFile && e.LFS != nil && matcher.Match(e.Path) {
			selected = append(selected, e)
			oids = append(oids, e.LFS.OID)
		}
	}
	// Everything selected that the Namespace does not hold yet must fit in
	// its quota, whether it is transferred or already in the Instance.
	objects := make(map[string]int64, len(selected))
	for _, e := range selected {
		objects[e.LFS.OID] = e.LFS.Size
	}
	if err := s.checkQuota(ctx, r.NamespaceID, objects, 0); err != nil {
		return err
	}
	stored, err := s.opts.LFS.StatMany(ctx, oids)
	if err != nil {
		return err
	}
	var present []repo.LFSRef
	for _, e := range selected {
		if o, ok := stored[e.LFS.OID]; ok && o.Verified && o.Size == e.LFS.Size {
			present = append(present, repo.LFSRef{OID: e.LFS.OID, Size: e.LFS.Size, Present: true})
			st.AlreadyPresent++
			s.countFile(ctx, up.Name, "deduplicated")
			continue
		}
		st.Files = append(st.Files, fileState{Path: e.Path, OID: e.LFS.OID, Size: e.LFS.Size, Status: fileQueued})
	}
	if len(present) > 0 {
		if err := s.opts.Repos.SetLFSPresence(ctx, r.ID, present); err != nil {
			return err
		}
	}
	for i := range st.Files {
		if err := s.queueTransfer(ctx, j.ID, p, up, upID, r, commit, &st.Files[i], st.Files[i].OID); err != nil {
			return err
		}
	}
	if _, err := s.mirrors.update(ctx, r.UUID, r.ID.Type, func(m *Mirror) error {
		m.Selection = m.Selection.Union(p.Selection)
		m.Status, m.LastError = MirrorSyncing, ""
		if p.Track && !m.Track {
			m.Track, m.TrackUser = true, p.RequestedBy
			if p.Revision != "" && !model.IsFullSHA(p.Revision) {
				m.TrackRef = p.Revision
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if p.Track {
		if err := s.scheduleSync(ctx, r); err != nil {
			return err
		}
	}
	st.Phase = phaseTransfer
	return prog.SetResult(ctx, st)
}

// queueTransfer queues the pull.file Job that transfers f into r at
// commit, or joins the active Job with the same dedupe key. Pulls first
// queue under the bare OID, so Pulls of the same object share one Job
// whatever their Namespace; ownTransferKey keeps a transfer to one
// Namespace.
func (s *Service) queueTransfer(ctx context.Context, parent jobs.JobID, p *pullPayload, up *Upstream, upID model.RepoID, r *repo.Repo, commit model.CommitSHA, f *fileState, key string) error {
	id, err := s.opts.Queue.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindPullFile, DedupeKey: key, ParentID: parent, CreatedBy: p.RequestedBy, Repo: r.UUID,
		Payload: filePayload{UpstreamID: up.ID, Upstream: up.Name, UpstreamRepo: upID.String(), Type: r.ID.Type, Repo: r.ID.String(),
			RepoUUID: r.UUID, Commit: commit, Path: f.Path, OID: f.OID, Size: f.Size, RequestedBy: p.RequestedBy, Now: p.Now}})
	if err != nil {
		return fmt.Errorf("queue the transfer of %s: %w", f.Path, err)
	}
	f.Job = id
	return nil
}

// ensureMirror returns the Mirror Repo for the Pull, creating it (and its
// reserved Namespace) on the first Pull.
func (s *Service) ensureMirror(ctx context.Context, up *Upstream, p *pullPayload, upID model.RepoID, info *RepoInfo) (*repo.Repo, error) {
	local, err := p.localID()
	if err != nil {
		return nil, jobs.Permanent(err)
	}
	r, err := s.opts.Repos.Get(ctx, local)
	if err == nil {
		m, err := s.mirrors.active(ctx, r.UUID, r.ID.Type)
		if errors.Is(err, ErrNotMirror) {
			return nil, fmt.Errorf("%w: %s is a local Repo, not a Mirror", ErrConflict, r.ID.Path())
		}
		if err != nil {
			return nil, err
		}
		if m.UpstreamID != up.ID || !m.UpstreamRepo.Equal(upID) {
			return nil, fmt.Errorf("%w: %s is a Mirror of %s from another Upstream", ErrConflict, r.ID.Path(), m.UpstreamRepo)
		}
		return r, nil
	}
	if !errors.Is(err, repo.ErrNotFound) {
		return nil, err
	}
	ns, err := s.opts.Orgs.EnsureMirrorNamespace(ctx, local.Namespace)
	if errors.Is(err, org.ErrExists) {
		return nil, fmt.Errorf("%w: the Namespace %s is a local Namespace; an admin can set a namespace_prefix for the %s Upstream", ErrConflict, local.Namespace, up.Name)
	}
	if err != nil {
		return nil, err
	}
	vis := model.VisibilityPublic
	if info.Private || info.Gated != "" {
		vis = model.VisibilityPrivate
	}
	actor := model.System()
	actor.NamespaceID = ns.ID
	r, err = s.opts.Repos.Create(ctx, repo.CreateParams{ID: local, Visibility: vis, Actor: actor, Mirror: true,
		Description: "Mirrored from " + up.Host() + "/" + upID.Path()})
	if errors.Is(err, repo.ErrExists) {
		if r, err = s.opts.Repos.Get(ctx, local); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if _, err := s.mirrors.get(ctx, r.UUID, r.ID.Type); errors.Is(err, ErrNotMirror) {
		if err := s.mirrors.put(ctx, &Mirror{RepoUUID: r.UUID, UpstreamID: up.ID, UpstreamRepo: upID, Selection: Selection{None: true},
			Status: MirrorPending}); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	return r, nil
}

// fetchHistory brings the Mirror's refs level with the Upstream, keeping
// SHAs (04-git.md "Mirrors"). It skips the fetch when the Revision's
// Commit is already the local tip of the same name, unless this is a sync.
func (s *Service) fetchHistory(ctx context.Context, up *Upstream, cred Credentials, r *repo.Repo, upID model.RepoID, p *pullPayload, want model.CommitSHA) error {
	if !p.Sync {
		rev := p.Revision
		if rev == "" {
			rev = model.DefaultBranch
		}
		if got, err := s.opts.Repos.Resolve(ctx, r.ID, rev); err == nil && got == want {
			return nil
		}
		if model.IsFullSHA(rev) {
			if _, err := s.opts.Repos.Resolve(ctx, r.ID, string(want)); err == nil {
				return nil
			}
		}
	}
	refs, err := s.opts.Repos.Refs(ctx, r.ID)
	if err != nil {
		return err
	}
	local := map[string]model.CommitSHA{}
	var have []model.CommitSHA
	for _, group := range [][]repo.Ref{refs.Branches, refs.Tags, refs.PullRequests, refs.Converts} {
		for _, ref := range group {
			local[ref.Name] = ref.Target
			if !slices.Contains(have, ref.Target) {
				have = append(have, ref.Target)
			}
		}
	}
	res, err := s.opts.Client.FetchPack(ctx, *up, cred, upID, have)
	if err != nil {
		return err
	}
	if res.Pack != nil {
		defer func() { _ = res.Pack.Close() }()
	}
	var updates []repo.RefUpdate
	names := make([]string, 0, len(res.Refs))
	for name := range res.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		target := res.Refs[name]
		old := local[name]
		if old == target || (res.Peeled[name] != "" && old == res.Peeled[name]) {
			continue
		}
		if strings.HasPrefix(name, model.RefTags) && !old.IsZero() && res.Peeled[name] != "" {
			// The local ref's stored value of an annotated tag is not
			// known here; a moved annotated tag is rare and is skipped.
			s.log.WarnContext(ctx, "skipping a moved annotated tag in a Mirror", "repo", r.ID.Path(), "ref", name)
			continue
		}
		updates = append(updates, repo.RefUpdate{Name: name, Old: old, New: target})
	}
	var gone []repo.RefUpdate
	for name, old := range local {
		if _, ok := res.Refs[name]; !ok && mirrorRef(name) {
			gone = append(gone, repo.RefUpdate{Name: name, Old: old})
		}
	}
	sort.Slice(gone, func(i, k int) bool { return gone[i].Name < gone[k].Name })
	updates = append(updates, gone...)
	if len(updates) == 0 && res.Pack == nil {
		return nil
	}
	var pack io.Reader
	if res.Pack != nil {
		pack = res.Pack
		if s.opts.Quota != nil {
			// The pack's size is only known once it has arrived; it must
			// fit in the Namespace's quota before anything is stored.
			sp, err := s.spoolPack(res.Pack)
			if err != nil {
				return err
			}
			defer func() { _ = sp.Close() }()
			if err := s.checkQuota(ctx, r.NamespaceID, nil, sp.size); err != nil {
				return err
			}
			pack = sp
		}
	}
	actor := model.System()
	if err := s.opts.Repos.ApplyPack(ctx, repo.ApplyPackParams{Repo: r.ID, Source: repo.SourceMirror, Actor: actor, Pack: pack, Updates: updates, AllowForce: true}); err != nil {
		return fmt.Errorf("store the upstream history: %w", err)
	}
	return s.keepReplaced(ctx, r, updates)
}

// keepReplaced keeps branch tips that an upstream force-push or branch
// deletion replaced reachable through refs/hangar/previous/<timestamp>/...
// so nothing pinned to them breaks (06 "Tracking").
func (s *Service) keepReplaced(ctx context.Context, r *repo.Repo, updates []repo.RefUpdate) error {
	var check []repo.RefUpdate
	for _, u := range updates {
		if !u.Old.IsZero() && strings.HasPrefix(u.Name, model.RefHeads) {
			check = append(check, u)
		}
	}
	if len(check) == 0 {
		return nil
	}
	view, err := s.opts.Repos.OpenView(ctx, r.ID)
	if err != nil {
		return err
	}
	defer func() { _ = view.Close() }()
	stamp := s.opts.Now().UTC().Format("20060102T150405Z")
	var keep []repo.RefUpdate
	for _, u := range check {
		if !u.New.IsZero() {
			cmd := exec.CommandContext(ctx, "git", "merge-base", "--is-ancestor", string(u.Old), string(u.New))
			cmd.Dir = view.Dir
			cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
			if err := cmd.Run(); err == nil {
				continue
			}
		}
		keep = append(keep, repo.RefUpdate{Name: "refs/hangar/previous/" + stamp + "/" + strings.TrimPrefix(u.Name, "refs/"), New: u.Old})
	}
	if len(keep) == 0 {
		return nil
	}
	return s.opts.Repos.ApplyPack(ctx, repo.ApplyPackParams{Repo: r.ID, Source: repo.SourceMirror, Actor: model.System(), Updates: keep})
}

// advance checks transfers and Dependency Pulls and finishes the Pull
// when everything is done.
func (s *Service) advance(ctx context.Context, j *jobs.Job, prog jobs.Progress, p *pullPayload, st *pullState) error {
	r, err := s.opts.Repos.GetByUUID(ctx, st.RepoUUID)
	if err != nil {
		return err
	}
	if st.Phase == phaseTransfer {
		active, err := s.checkFiles(ctx, j, p, r, st)
		if err != nil {
			return err
		}
		if err := prog.SetResult(ctx, st); err != nil {
			return err
		}
		if err := prog.Report(ctx, s.pullProgress(ctx, st)); err != nil {
			return err
		}
		if active > 0 {
			return s.snooze(s.opts.Now().Add(s.opts.PollInterval), "Transferring files")
		}
		if err := quotaFailure(st); err != nil {
			return s.failPull(ctx, prog, p, st, err)
		}
		if err := s.startDependencies(ctx, j, p, r, st); err != nil {
			return err
		}
		st.Phase = phaseDeps
		if err := prog.SetResult(ctx, st); err != nil {
			return err
		}
	}
	if st.Phase == phaseDeps {
		active := 0
		for i := range st.Dependencies {
			d := &st.Dependencies[i]
			if d.Job == uuid.Nil || d.Status == fileDone || d.Status == fileFailed {
				continue
			}
			dj, err := s.opts.Queue.Get(ctx, d.Job)
			if err != nil {
				return err
			}
			switch dj.Status {
			case jobs.StatusSucceeded:
				d.Status = fileDone
			case jobs.StatusFailed, jobs.StatusCancelled:
				d.Status, d.Error = fileFailed, dj.Error
			default:
				active++
			}
		}
		if active > 0 {
			if err := prog.SetResult(ctx, st); err != nil {
				return err
			}
			u := s.pullProgress(ctx, st)
			u.Message = "Pulling Dependencies"
			_ = prog.Report(ctx, u)
			return s.snooze(s.opts.Now().Add(s.opts.PollInterval), "Pulling Dependencies")
		}
	}
	return s.finish(ctx, j, prog, p, r, st)
}

// checkFiles updates file states from their Jobs and marks finished
// objects present in this Repo. It returns how many are still active.
func (s *Service) checkFiles(ctx context.Context, j *jobs.Job, p *pullPayload, r *repo.Repo, st *pullState) (int, error) {
	active := 0
	var present []repo.LFSRef
	for i := range st.Files {
		f := &st.Files[i]
		if f.Status == fileDone || f.Status == fileFailed || f.Status == fileCancelled || f.Status == filePresent {
			continue
		}
		fj, err := s.opts.Queue.Get(ctx, f.Job)
		if err != nil {
			return 0, err
		}
		switch fj.Status {
		case jobs.StatusSucceeded:
			joined, err := s.markJoinedTransfer(ctx, r, fj, f)
			if errors.Is(err, ErrQuotaExceeded) {
				f.Status, f.Error, f.Reason = fileFailed, err.Error(), reasonQuotaExceeded
				continue
			}
			if err != nil {
				return 0, err
			}
			f.Status, f.BytesDone = fileDone, f.Size
			if !joined {
				present = append(present, repo.LFSRef{OID: f.OID, Size: f.Size, Present: true})
			}
		case jobs.StatusFailed:
			var fr fileResult
			refused := len(fj.Result) > 0 && json.Unmarshal(fj.Result, &fr) == nil && fr.Reason == reasonQuotaExceeded
			if refused {
				queued, err := s.requeueRefusedTransfer(ctx, j, p, r, st, fj, f)
				if err != nil {
					return 0, err
				}
				if queued {
					active++
					continue
				}
			}
			f.Status, f.Error = fileFailed, fj.Error
			if refused {
				f.Reason = reasonQuotaExceeded
			}
		case jobs.StatusCancelled:
			f.Status, f.Error = fileCancelled, "cancelled"
		case jobs.StatusRunning:
			f.Status, f.BytesDone = fileRunning, fj.Progress.BytesDone
			active++
		default:
			f.Status = fileQueued
			active++
		}
	}
	if len(present) > 0 {
		if err := s.opts.Repos.SetLFSPresence(ctx, r.ID, present); err != nil {
			return 0, err
		}
	}
	return active, nil
}

// markJoinedTransfer handles a finished transfer that a pull.file Job for
// another Repo ran (Pulls of the same object share one Job). Such an
// object is new to this Repo's Namespace, so it is marked present only
// once the quota allows it. joined is false for this Repo's own Jobs,
// which checked the quota and marked the object themselves.
func (s *Service) markJoinedTransfer(ctx context.Context, r *repo.Repo, fj *jobs.Job, f *fileState) (joined bool, err error) {
	if s.opts.Quota == nil {
		return false, nil
	}
	var fp filePayload
	if err := fj.DecodePayload(&fp); err != nil {
		return false, err
	}
	if fp.RepoUUID == r.UUID {
		return false, nil
	}
	if err := s.checkQuota(ctx, r.NamespaceID, map[string]int64{f.OID: f.Size}, 0); err != nil {
		return true, err
	}
	return true, s.opts.Repos.SetLFSPresence(ctx, r.ID, []repo.LFSRef{{OID: f.OID, Size: f.Size, Present: true}})
}

// ownTransferKey is the dedupe key of a transfer of oid that only Pulls
// into the Namespace ns can join. A Pull uses it after another
// Namespace's quota refused the shared transfer it joined, so no other
// Namespace's Pull, and no other Namespace's quota, can hold it up again.
func ownTransferKey(oid string, ns uuid.UUID) string {
	return oid + "/namespace/" + ns.String()
}

// requeueRefusedTransfer handles a transfer that the Namespace quota
// refused. When the refused pull.file Job was a shared one (keyed by the
// bare OID) that a Pull into another Namespace started, the refusal was
// that Namespace's quota, not this one's: this Pull queues a transfer of
// its own under ownTransferKey, whose quota check is its own Namespace's,
// and never takes the other Namespace's reason or message. queued is false
// when the refusal stands: the refused Job stored into this Namespace, or
// it was already a transfer of this Namespace's own. So a Pull requeues
// each file at most once, however many other Namespaces' Pulls are
// refused for the same object meanwhile.
func (s *Service) requeueRefusedTransfer(ctx context.Context, j *jobs.Job, p *pullPayload, r *repo.Repo, st *pullState, fj *jobs.Job, f *fileState) (queued bool, err error) {
	if fj.DedupeKey != f.OID {
		return false, nil
	}
	var fp filePayload
	if err := fj.DecodePayload(&fp); err != nil {
		return false, err
	}
	if fp.RepoUUID == r.UUID {
		return false, nil
	}
	other, err := s.opts.Repos.GetByUUID(ctx, fp.RepoUUID)
	switch {
	case err == nil && other.NamespaceID == r.NamespaceID:
		return false, nil
	case err != nil && !errors.Is(err, repo.ErrNotFound):
		return false, err
	}
	upID, err := p.upstreamID()
	if err != nil {
		return false, jobs.Permanent(err)
	}
	up, err := s.opts.Registry.Get(ctx, p.UpstreamID)
	if err != nil {
		return false, err
	}
	if err := s.queueTransfer(ctx, j.ID, p, up, upID, r, st.Commit, f, ownTransferKey(f.OID, r.NamespaceID)); err != nil {
		return false, err
	}
	f.Status, f.BytesDone, f.Error, f.Reason = fileQueued, 0, "", ""
	return true, nil
}

// quotaFailure returns the error that fails a Pull whose files the
// Namespace quota refused, or nil when it refused none.
func quotaFailure(st *pullState) error {
	var (
		paths []string
		first string
	)
	for _, f := range st.Files {
		if f.Reason != reasonQuotaExceeded {
			continue
		}
		if first == "" {
			first = f.Error
		}
		paths = append(paths, f.Path)
	}
	if len(paths) == 0 {
		return nil
	}
	list := strings.Join(paths[:min(len(paths), 5)], ", ")
	if len(paths) > 5 {
		list += fmt.Sprintf(" and %d more", len(paths)-5)
	}
	verb := "was"
	if len(paths) > 1 {
		verb = "were"
	}
	return QuotaExceeded(fmt.Errorf("%s %s not stored: %s", list, verb, first))
}

// pullProgress sums the files of a Pull for the UI.
func (s *Service) pullProgress(ctx context.Context, st *pullState) jobs.ProgressUpdate {
	u := jobs.ProgressUpdate{FilesTotal: len(st.Files)}
	for _, f := range st.Files {
		u.BytesTotal += f.Size
		done := f.BytesDone
		if f.Status == fileDone {
			done = f.Size
			u.FilesDone++
		}
		u.BytesDone += done
		u.Files = append(u.Files, jobs.FileProgress{Path: f.Path, BytesDone: done, BytesTotal: f.Size, Status: f.Status, Error: f.Error})
		if f.Status == fileRunning {
			if fj, err := s.opts.Queue.Get(ctx, f.Job); err == nil {
				u.Rate += fj.Progress.Rate
				if fj.Progress.Message != "" {
					u.Message = fj.Progress.Message
				}
			}
		}
	}
	return u
}

// startDependencies resolves the Repo's Dependencies at the Pull's Commit,
// stores them and queues Pulls of missing code Dependencies (and base
// models when auto_pull_base_models is on), recursively up to max_depth
// with cycle detection (06 "Dependencies").
func (s *Service) startDependencies(ctx context.Context, j *jobs.Job, p *pullPayload, r *repo.Repo, st *pullState) error {
	ds, err := s.resolveAndStore(ctx, r, st.SourceCommit)
	if err != nil {
		return err
	}
	up, err := s.opts.Registry.Get(ctx, p.UpstreamID)
	if err != nil {
		return err
	}
	chain := append(slices.Clone(p.Chain), strings.ToLower(string(r.ID.Type)+":"+r.ID.String()))
	for _, d := range ds {
		auto := (d.Kind == deps.KindCode && d.Required && s.opts.Dependencies.AutoPullCode) ||
			(d.Kind == deps.KindAdapterBase && s.opts.Dependencies.AutoPullBaseModels)
		if !auto {
			continue
		}
		local := up.LocalID(d.Target)
		key := strings.ToLower(string(local.Type) + ":" + local.String())
		ds := depState{Repo: local.String(), Type: local.Type, Kind: d.Kind}
		switch {
		case slices.Contains(chain, key):
			continue
		case p.Depth+1 > s.opts.Dependencies.MaxDepth:
			st.Warnings = append(st.Warnings, fmt.Sprintf("%s is needed but was not pulled: Dependencies are pulled at most %d levels deep", d.Target, s.opts.Dependencies.MaxDepth))
			continue
		}
		state, _, err := s.localState(ctx, model.System(), local, "")
		if err != nil {
			return err
		}
		if state == ArsenalFull {
			continue
		}
		child := pullPayload{UpstreamID: up.ID, Upstream: up.Name, UpstreamRepo: d.Target.String(), Type: d.Target.Type, Repo: local.String(),
			Revision: d.Revision, RequestedBy: p.RequestedBy, Now: p.Now, Depth: p.Depth + 1, Chain: chain}
		id, err := s.enqueuePull(ctx, child, j.ID, jobs.PriorityNormal)
		if err != nil {
			return err
		}
		ds.Job, ds.Status = id, fileQueued
		st.Dependencies = append(st.Dependencies, ds)
	}
	return nil
}

// resolveAndStore parses a Repo's Dependencies at a Commit from its files
// and replaces the stored rows.
func (s *Service) resolveAndStore(ctx context.Context, r *repo.Repo, commit model.CommitSHA) ([]deps.Dependency, error) {
	entries, err := s.opts.Repos.Tree(ctx, r.ID, commit, "", repo.TreeOpts{Recursive: true})
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if e.Type == model.EntryFile {
			files = append(files, e.Path)
		}
	}
	ds, err := deps.Parse(ctx, r.ID, files, func(ctx context.Context, path string) ([]byte, error) {
		rc, e, err := s.opts.Repos.OpenFile(ctx, r.ID, commit, path, nil)
		if errors.Is(err, repo.ErrEntryNotFound) || errors.Is(err, repo.ErrNotPresent) {
			return nil, fs.ErrNotExist
		}
		if err != nil {
			return nil, err
		}
		defer func() { _ = rc.Close() }()
		if e.Size > deps.MaxFileSize {
			return nil, fs.ErrNotExist
		}
		return io.ReadAll(io.LimitReader(rc, deps.MaxFileSize+1))
	})
	if err != nil {
		return nil, err
	}
	if err := s.deps.Replace(ctx, r.UUID, commit, ds); err != nil {
		return nil, err
	}
	return ds, nil
}

// finish is step 3: index and scan the Commit, record the Mirror's state,
// fire webhooks and end the Pull, as partial when files failed.
func (s *Service) finish(ctx context.Context, j *jobs.Job, prog jobs.Progress, p *pullPayload, r *repo.Repo, st *pullState) error {
	payload := commitJob{RepoUUID: r.UUID, Repo: r.ID.String(), Type: r.ID.Type, Commit: st.SourceCommit}
	key := r.UUID.String() + ":" + string(st.SourceCommit)
	for _, k := range []jobs.Kind{jobs.KindIndex, jobs.KindScan} {
		if _, err := s.opts.Queue.Enqueue(ctx, jobs.NewJob{Kind: k, DedupeKey: key, Payload: payload, Repo: r.UUID}); err != nil {
			return err
		}
	}
	var failed []string
	for _, f := range st.Files {
		if f.Status == fileFailed || f.Status == fileCancelled {
			failed = append(failed, f.Path)
		}
	}
	var depFailed []string
	for _, d := range st.Dependencies {
		if d.Status == fileFailed {
			depFailed = append(depFailed, d.Repo)
		}
	}
	status := MirrorOK
	lastErr := ""
	st.Phase = phaseDone
	if len(failed) > 0 || len(depFailed) > 0 {
		status, st.Phase = MirrorPartial, phasePartial
		lastErr = partialMessage(failed, depFailed)
	}
	now := s.opts.Now().UTC()
	if _, err := s.mirrors.update(ctx, r.UUID, r.ID.Type, func(m *Mirror) error {
		m.Status, m.LastError = status, lastErr
		if p.Sync || m.LastSyncedCommit.IsZero() || p.Revision == "" || p.Revision == m.TrackRef || p.Revision == model.DefaultBranch {
			m.LastSyncedCommit, m.LastSyncedAt = st.SourceCommit, &now
		}
		return nil
	}); err != nil && !errors.Is(err, ErrNotMirror) {
		return err
	}
	if err := prog.SetResult(ctx, st); err != nil {
		return err
	}
	_ = prog.Report(ctx, s.pullProgress(ctx, st))
	data := map[string]any{"upstream": p.Upstream, "upstream_repo": p.UpstreamRepo, "commit": string(st.SourceCommit), "job_id": j.ID.String(),
		"files": len(st.Files) + st.AlreadyPresent, "failed": failed}
	if p.Sync {
		s.emit(ctx, webhook.MirrorSynced, model.System(), r, data)
	}
	if lastErr != "" {
		s.emit(ctx, webhook.PullFailed, model.System(), r, data)
		return jobs.Permanent(errors.New(lastErr))
	}
	s.emit(ctx, webhook.PullCompleted, model.System(), r, data)
	return nil
}

func partialMessage(files, depRepos []string) string {
	var parts []string
	if len(files) > 0 {
		parts = append(parts, strconv.Itoa(len(files))+" file(s) failed: "+strings.Join(head(files, 5), ", "))
	}
	if len(depRepos) > 0 {
		parts = append(parts, "Dependency Pulls failed: "+strings.Join(head(depRepos, 5), ", "))
	}
	return "The Pull is partial. " + strings.Join(parts, "; ") + ". Retry the Pull to transfer the failed files again."
}

func head(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return append(slices.Clone(s[:n]), fmt.Sprintf("and %d more", len(s)-n))
}

func (s *Service) countFile(ctx context.Context, upstreamName, outcome string) {
	if s.opts.Instruments == nil || s.opts.Instruments.PullFiles == nil {
		return
	}
	s.opts.Instruments.PullFiles.Add(ctx, 1, upstreamAttrs(upstreamName, outcome))
}
