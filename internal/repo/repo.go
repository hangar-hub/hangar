// Package repo is the Repo service: creating Repos, reading trees, files,
// Commits and refs, writing Commits and moving refs, renames, transfers,
// deletion and settings. It is the only package that writes Commits or refs
// (CLAUDE.md); the Hub API, git server, Pulls and the UI all go through it.
//
// The contract is in docs/spec/02-architecture.md "Core interfaces" and
// docs/spec/04-git.md. repotest has a fake backed by a real git repository,
// so Commit SHAs from the fake are real git SHAs.
package repo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"

	"github.com/hangar-hub/hangar/internal/blob"
	"github.com/hangar-hub/hangar/internal/model"
)

// Errors. Handlers map them to each protocol's error format.
var (
	// ErrNotFound means the Repo does not exist (or is deleted).
	ErrNotFound = errors.New("repo not found")
	// ErrExists means a Repo with that ID already exists.
	ErrExists = errors.New("repo already exists")
	// ErrRevisionNotFound means the Revision does not name a Commit.
	ErrRevisionNotFound = errors.New("revision not found")
	// ErrEntryNotFound means the path does not exist at the Commit, or is a
	// file where a directory was expected.
	ErrEntryNotFound = errors.New("entry not found")
	// ErrNotPresent means the file's LFS object is not in the Arsenal, as in
	// a Partial mirror.
	ErrNotPresent = errors.New("file not in arsenal")
	// ErrRefConflict means a compare-and-swap on a ref failed because the
	// ref moved (04-git.md "Ref updates").
	ErrRefConflict = errors.New("reference has changed concurrently, fetch first")
	// ErrParentMismatch means the client's parent Commit is not the branch
	// tip; the Hub API answers 412.
	ErrParentMismatch = errors.New("parent commit is not the head of the branch")
	// ErrRefExists means a branch or tag with that name already exists.
	ErrRefExists = errors.New("reference already exists")
	// ErrReadOnly means the Repo is a Mirror and refuses direct writes.
	ErrReadOnly = errors.New("repo is a mirror and is read-only; detach it to write to it")
	// ErrProtectedRef means the change would delete or rewrite a protected
	// ref, such as the default branch.
	ErrProtectedRef = errors.New("reference is protected")
	// ErrLFSObjectMissing means a Commit refers to an LFS object that is not
	// stored and verified.
	ErrLFSObjectMissing = errors.New("lfs object is missing or not verified")
	// ErrInvalid wraps validation failures such as bad paths or names.
	ErrInvalid = errors.New("invalid request")
)

// Gating describes gated access (REPO-8). Mode is "", "auto" or "manual".
type Gating struct {
	Mode string `json:"mode,omitempty"`
	// Prompt and Fields describe the access request form.
	Prompt string        `json:"prompt,omitempty"`
	Fields []GatingField `json:"fields,omitempty"`
}

// Enabled reports whether the Repo is gated.
func (g Gating) Enabled() bool { return g.Mode != "" }

// GatingField is one field of the access request form.
type GatingField struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Required bool   `json:"required,omitempty"`
}

// Repo is a Repo's metadata.
type Repo struct {
	UUID          uuid.UUID
	ID            model.RepoID
	NamespaceID   uuid.UUID
	Visibility    model.Visibility
	DefaultBranch string
	HeadRef       string
	Description   string
	Tags          []string
	Licence       string
	Gating        Gating
	// Mirror is true when the Repo tracks an Upstream and has not been detached.
	Mirror    bool
	CreatedBy uuid.UUID
	CreatedAt time.Time
	UpdatedAt time.Time
	// LastModified is the time of the newest Commit on any branch.
	LastModified time.Time
	DeletedAt    *time.Time
	// SizeBytes is git data plus LFS objects; LFSBytes counts LFS objects only.
	SizeBytes int64
	LFSBytes  int64
}

// CreateParams describes a new Repo.
type CreateParams struct {
	ID          model.RepoID
	Visibility  model.Visibility
	Description string
	Licence     string
	Actor       model.Principal
	// InitialReadme adds a starter README.md to the first Commit, as the UI does.
	InitialReadme bool
	// Mirror creates an empty Repo with no Commits, for a Pull to fill.
	Mirror bool
}

// ListOptions filters and orders a Repo listing. Metadata filters use the
// index results of each Repo's default branch head.
type ListOptions struct {
	// Viewer limits results to Repos the Principal can read.
	Viewer     model.Principal
	Type       model.RepoType // "" means both
	Namespaces []string
	// Search matches a substring of `namespace/name`, case-insensitively.
	Search string
	Mirror *bool
	// Metadata filters (BROWSE-1 and DATA-11).
	Formats    []string
	Tasks      []string
	Libraries  []string
	Quants     []string
	Licences   []string
	ParamsMin  int64
	ParamsMax  int64
	ContextMin int64
	SizeMax    int64
	Tags       []string
	// Dataset filters (DATA-11). RowsMin and RowsMax bound a dataset's
	// row count, known once its Parquet export exists; zero means no bound.
	Modalities     []string
	Languages      []string
	SizeCategories []string
	RowsMin        int64
	RowsMax        int64
	IncludeFacets  bool
	// Sort is one of updated, created, name, size, parameters, downloads, likes.
	Sort string
	// Desc reverses the order. The Hub sorts descending by default.
	Desc   bool
	Cursor string
	Limit  int
}

// ListResult is a page of Repos.
type ListResult struct {
	Repos      []*Repo
	NextCursor string
	// Facets maps a facet name to value counts when IncludeFacets is set.
	Facets map[string]map[string]int
}

// TreeOpts controls Tree.
type TreeOpts struct {
	// Recursive lists every descendant, files and directories, with full paths.
	Recursive bool
	// Expand fills Entry.LastCommit.
	Expand bool
	// After is an exclusive path cursor: only entries after it are returned.
	After string
	// Limit caps the number of entries; 0 means no limit.
	Limit int
}

// OpKind is the kind of a Commit operation.
type OpKind string

// Commit operations (Hub NDJSON `file`, `lfsFile`, `deletedFile`, `deletedFolder`, and copies).
const (
	OpAdd          OpKind = "add"
	OpAddLFS       OpKind = "add_lfs"
	OpDelete       OpKind = "delete"
	OpDeleteFolder OpKind = "delete_folder"
	OpCopy         OpKind = "copy"
)

// Operation is one change in a Commit.
type Operation struct {
	Kind OpKind
	Path string
	// Content is the file content for OpAdd. The service decides whether the
	// path is stored in LFS from the Repo's .gitattributes.
	Content io.Reader
	// OID and Size name an already-uploaded LFS object for OpAddLFS. Size
	// may be zero for copies, in which case the stored size is used.
	OID  string
	Size int64
	// SrcPath and SrcRevision name the source of OpCopy, in the same Repo.
	SrcPath     string
	SrcRevision string
}

// CommitParams describes a server-side Commit.
type CommitParams struct {
	Repo model.RepoID
	// Revision is the branch name, or a full ref such as `refs/pr/3`.
	Revision string
	// ParentCommit, when set, must equal the branch tip (Hub `parentCommit`).
	ParentCommit model.CommitSHA
	// CreateFrom creates Revision at this Commit if it does not exist, as
	// pull request refs need.
	CreateFrom  model.CommitSHA
	Summary     string
	Description string
	Operations  []Operation
	Actor       model.Principal
	// AllowMirror lets the Pull machinery and hangar-bot write to Mirrors.
	AllowMirror bool
	// Time sets the author and committer time; zero means now (UTC).
	Time time.Time
}

// CommitResult is the outcome of a Commit.
type CommitResult struct {
	SHA    model.CommitSHA
	Parent model.CommitSHA
	Ref    string
	// NoOp is true when no operation changed the tree; no Commit was made
	// and SHA is the unchanged tip.
	NoOp bool
}

// Signature is a git author or committer.
type Signature struct {
	Name  string
	Email string
	When  time.Time
}

// CommitInfo describes a Commit.
type CommitInfo struct {
	SHA       model.CommitSHA
	Parents   []model.CommitSHA
	Tree      string
	Author    Signature
	Committer Signature
	Title     string
	Message   string
}

// LogOpts controls Log.
type LogOpts struct {
	// Path limits the log to Commits that touch it.
	Path  string
	Skip  int
	Limit int
	// From, when set, starts the walk at these Commits instead of the
	// Revision, which is then ignored. Each must be a full SHA of a Commit
	// in the Repo, or Log returns ErrRevisionNotFound. Listings pass the
	// Commits a previous page's walk had reached but not shown, so the next
	// page continues it without walking the earlier pages again.
	From []model.CommitSHA
}

// ChangeStatus is how a file changed in a Commit.
type ChangeStatus string

// Change statuses.
const (
	ChangeAdded    ChangeStatus = "added"
	ChangeModified ChangeStatus = "modified"
	ChangeDeleted  ChangeStatus = "deleted"
	ChangeRenamed  ChangeStatus = "renamed"
)

// FileChange is one file in a Commit's diff summary (REPO-3).
type FileChange struct {
	Path    string
	OldPath string
	Status  ChangeStatus
	OldOID  string
	NewOID  string
	OldSize int64
	NewSize int64
	// LFS is true when either side is an LFS file; then only sizes and
	// SHA changes are shown.
	LFS bool
	// Binary is true when the file is not text.
	Binary bool
}

// FileDiff is a text diff of one file.
type FileDiff struct {
	FileChange
	// Patch is a unified diff, empty for LFS, binary or files of 1 MiB or more.
	Patch string
}

// Ref is a named ref.
type Ref struct {
	// Name is the full ref name, such as `refs/heads/main`.
	Name string
	// Target is the Commit the ref points at, after peeling annotated tags.
	Target model.CommitSHA
}

// Refs lists a Repo's refs by kind, as the Hub API groups them.
type Refs struct {
	Branches     []Ref
	Tags         []Ref
	Converts     []Ref
	PullRequests []Ref
	// Other holds refs outside those namespaces, such as refs/hangar/*.
	Other []Ref
}

// RefUpdate is one compare-and-swap. Old zero creates the ref; New zero
// deletes it.
type RefUpdate struct {
	Name string
	Old  model.CommitSHA
	New  model.CommitSHA
}

// TagParams describes a new tag.
type TagParams struct {
	Repo     model.RepoID
	Tag      string
	Revision string
	// Message makes an annotated tag; empty makes a lightweight tag.
	Message string
	Actor   model.Principal
}

// PackSource says where a pack came from, which decides the policy checks.
type PackSource string

// Pack sources.
const (
	SourcePush   PackSource = "push"
	SourceMirror PackSource = "mirror"
)

// ApplyPackParams describes received git objects and the ref updates that
// make them visible. ApplyPack stores the pack, then runs every ref update
// as one compare-and-swap transaction: all succeed or none do.
type ApplyPackParams struct {
	Repo    model.RepoID
	Source  PackSource
	Actor   model.Principal
	Pack    io.Reader
	Updates []RefUpdate
	// AllowForce allows non-fast-forward updates of the default branch for
	// pushes by Namespace admins or when git.allow_force_push is set.
	AllowForce bool
}

// PolicyError explains why a push was refused, for git's report-status.
type PolicyError struct {
	Ref     string
	Message string
}

func (e *PolicyError) Error() string {
	if e.Ref == "" {
		return e.Message
	}
	return e.Ref + ": " + e.Message
}

// GitView is a temporary bare repository on local disk, holding a Repo's
// objects and a snapshot of its refs, that `git upload-pack` can serve.
// Close removes it.
type GitView struct {
	Dir   string
	close func() error
}

// NewGitView returns a GitView that calls closeFn on Close.
func NewGitView(dir string, closeFn func() error) *GitView {
	return &GitView{Dir: dir, close: closeFn}
}

// Close removes the view.
func (v *GitView) Close() error {
	if v == nil || v.close == nil {
		return nil
	}
	return v.close()
}

// SettingsUpdate changes Repo settings (REPO-9). Nil fields are unchanged.
type SettingsUpdate struct {
	Description   *string
	Visibility    *model.Visibility
	DefaultBranch *string
	Tags          *[]string
	Licence       *string
	Gating        *Gating
}

// LFSRef records an LFS object referenced by a Repo, and whether it is present.
type LFSRef struct {
	OID     string
	Size    int64
	Present bool
}

// Service is the Repo service.
type Service interface {
	// Create makes a Repo with an initial Commit on main holding the Hub's
	// default .gitattributes (and a README.md when asked), unless
	// CreateParams.Mirror is set. ErrExists if the ID is taken.
	Create(ctx context.Context, p CreateParams) (*Repo, error)
	// Get returns a Repo by ID, following renames and transfers; Repo.ID is
	// the canonical ID, so callers can redirect when it differs.
	Get(ctx context.Context, id model.RepoID) (*Repo, error)
	// GetByUUID returns a Repo by its UUID, including soft-deleted Repos.
	GetByUUID(ctx context.Context, id uuid.UUID) (*Repo, error)
	// List returns a page of Repos.
	List(ctx context.Context, opts ListOptions) (*ListResult, error)

	// Resolve turns a Revision (branch, tag, full SHA, refs/pr/N or
	// refs/convert/<name>) into a Commit SHA. Branches win over tags.
	Resolve(ctx context.Context, id model.RepoID, revision string) (model.CommitSHA, error)
	// Tree lists a directory at a Commit; path "" is the root. A missing
	// path or a file path gives ErrEntryNotFound.
	Tree(ctx context.Context, id model.RepoID, commit model.CommitSHA, path string, opts TreeOpts) ([]model.Entry, error)
	// Entry returns one file or directory.
	Entry(ctx context.Context, id model.RepoID, commit model.CommitSHA, path string) (model.Entry, error)
	// Entries returns the entries that exist among paths, omitting missing ones.
	Entries(ctx context.Context, id model.RepoID, commit model.CommitSHA, paths []string, expand bool) ([]model.Entry, error)
	// OpenFile opens a file's content (for LFS files, the object, not the
	// pointer), or a range of it. ErrNotPresent for objects not in the Arsenal.
	OpenFile(ctx context.Context, id model.RepoID, commit model.CommitSHA, path string, rng *blob.Range) (io.ReadCloser, model.Entry, error)

	// Commit makes a server-side Commit.
	Commit(ctx context.Context, p CommitParams) (*CommitResult, error)
	// Log lists Commits reachable from revision, newest first.
	Log(ctx context.Context, id model.RepoID, revision string, opts LogOpts) ([]CommitInfo, error)
	// GetCommit returns one Commit.
	GetCommit(ctx context.Context, id model.RepoID, sha model.CommitSHA) (*CommitInfo, error)
	// Diff summarises the files a Commit changed against its first parent.
	Diff(ctx context.Context, id model.RepoID, sha model.CommitSHA) ([]FileChange, error)
	// FileDiff returns the diff of one file in a Commit.
	FileDiff(ctx context.Context, id model.RepoID, sha model.CommitSHA, path string) (*FileDiff, error)
	// CompareFile returns the diff of one file from Commit base to Commit
	// head, as a pull request with several Commits shows it against the
	// fork point. ErrEntryNotFound when the file is the same in both.
	CompareFile(ctx context.Context, id model.RepoID, base, head model.CommitSHA, path string) (*FileDiff, error)

	// Refs lists every ref.
	Refs(ctx context.Context, id model.RepoID) (*Refs, error)
	// UpdateRefs applies every update atomically with compare-and-swap, or
	// none of them, returning ErrRefConflict.
	UpdateRefs(ctx context.Context, id model.RepoID, updates []RefUpdate) error
	// CreateBranch creates a branch at a Revision. ErrRefExists if it exists.
	CreateBranch(ctx context.Context, id model.RepoID, branch, from string, actor model.Principal) error
	// DeleteBranch deletes a branch. ErrProtectedRef for the default branch.
	DeleteBranch(ctx context.Context, id model.RepoID, branch string, actor model.Principal) error
	// CreateTag tags a Revision. ErrRefExists if the tag exists.
	CreateTag(ctx context.Context, p TagParams) error
	// DeleteTag deletes a tag.
	DeleteTag(ctx context.Context, id model.RepoID, tag string, actor model.Principal) error

	// ApplyPack stores a received pack and moves refs (git push, Mirror sync).
	ApplyPack(ctx context.Context, p ApplyPackParams) error
	// OpenView materialises a GitView for git upload-pack and receive-pack.
	OpenView(ctx context.Context, id model.RepoID) (*GitView, error)

	// Rename gives a Repo a new name in the same Namespace, keeping a redirect.
	Rename(ctx context.Context, id model.RepoID, newName string, actor model.Principal) (*Repo, error)
	// Transfer moves a Repo to another Namespace, keeping a redirect.
	Transfer(ctx context.Context, id model.RepoID, newNamespace string, actor model.Principal) (*Repo, error)
	// Delete soft-deletes a Repo; it can be restored within repos.restore_window.
	Delete(ctx context.Context, id model.RepoID, actor model.Principal) error
	// Restore undoes a soft delete. ErrExists if the ID was reused meanwhile.
	Restore(ctx context.Context, id model.RepoID, actor model.Principal) (*Repo, error)
	// Purge removes a soft-deleted Repo and its git data for good.
	Purge(ctx context.Context, repoUUID uuid.UUID) error
	// UpdateSettings changes settings and returns the updated Repo.
	UpdateSettings(ctx context.Context, id model.RepoID, u SettingsUpdate, actor model.Principal) (*Repo, error)
	// SetMirrorState marks a Repo as a Mirror, which refuses direct writes,
	// or detaches it into an ordinary Repo.
	SetMirrorState(ctx context.Context, id model.RepoID, mirror bool) error
	// CountByNamespace counts the live Repos of a Namespace that viewer can see.
	CountByNamespace(ctx context.Context, namespaceID uuid.UUID, viewer model.Principal) (int, error)
	// SetLFSPresence records LFS objects a Repo references and whether
	// they are present, for Partial mirrors and Pulls.
	SetLFSPresence(ctx context.Context, id model.RepoID, refs []LFSRef) error
	// LFSRefs returns which of oids the Repo references, and whether each is
	// present. The git-lfs download batch uses it so a Repo can only fetch
	// objects it references, and to answer "not in arsenal" for Partial mirrors.
	LFSRefs(ctx context.Context, id model.RepoID, oids []string) (map[string]LFSRef, error)
	// Referable returns which of oids p may add pointers to in a Commit to
	// r without uploading them: objects r already holds, objects p uploaded
	// and objects in another Repo whose files p can read. Commits and
	// pushes refuse a pointer to any other object as not uploaded, and the
	// git-lfs batch and upload plans ask for its bytes even when they are
	// stored, so neither says whether an object p cannot read exists.
	Referable(ctx context.Context, p model.Principal, r *Repo, oids []string) (map[string]bool, error)
}

// LFSObject describes a stored LFS object.
type LFSObject struct {
	OID        string
	Size       int64
	StorageKey string
	// Verified is true once the SHA-256 and size have been checked.
	Verified bool
}

// UploadParams starts an LFS or native upload.
type UploadParams struct {
	OID      string
	Size     int64
	RepoUUID uuid.UUID
	UserID   uuid.UUID
	// Multipart splits the upload into parts of PartSize bytes.
	Multipart bool
	PartSize  int64
	TTL       time.Duration
	// Resume returns the user's unexpired multipart upload of the same
	// object, Repo and part size, if there is one, with its expiry moved to
	// TTL from now, instead of starting another. The native API's upload
	// plan sets it, so a client that plans again after a reload continues
	// the upload that holds its parts. With no UserID and no RepoUUID it
	// resumes an upload Hangar made itself of the same object and part
	// size, so a retried Pull continues a transfer that failed part way.
	Resume bool
}

// Upload is an in-progress upload.
type Upload struct {
	ID        uuid.UUID
	OID       string
	Size      int64
	PartSize  int64
	PartCount int
	// Done lists the parts uploaded so far.
	Done      []blob.Part
	ExpiresAt time.Time
}

// ErrChecksumMismatch means uploaded bytes do not match the declared
// SHA-256 or size; nothing is stored.
var ErrChecksumMismatch = errors.New("uploaded content does not match its sha256 or size")

// ErrRetryable means a request failed for a reason that may pass, such as
// a Blob store read that broke part way, and that it kept its state, so
// the same request can be sent again.
var ErrRetryable = errors.New("temporary failure, try again")

// LFSStore stores LFS objects once per Instance, addressed by SHA-256, and
// runs uploads (basic, multipart and presigned). The git-lfs API, the Hub
// API, native uploads, Xet reassembly and Pulls all store objects through it.
type LFSStore interface {
	// Stat returns a stored object, or ErrNotFound.
	Stat(ctx context.Context, oid string) (LFSObject, error)
	// StatMany returns the stored objects among oids, keyed by OID.
	StatMany(ctx context.Context, oids []string) (map[string]LFSObject, error)
	// Ingest streams r into the store, hashing as it goes, and records the
	// object as verified only if the SHA-256 and size match. When the
	// object is already stored, r is still read and checked but not stored
	// again, so a successful Ingest always means r held the object's bytes.
	Ingest(ctx context.Context, oid string, size int64, r io.Reader) (LFSObject, error)
	// RecordUploader records that a User sent the bytes of a stored object,
	// so their Commits may add pointers to it (Service.Referable).
	// uuid.Nil, for uploads Hangar makes itself, is ignored.
	RecordUploader(ctx context.Context, oid string, userID uuid.UUID) error
	// Uploaded returns which of oids the User sent the bytes of.
	Uploaded(ctx context.Context, userID uuid.UUID, oids []string) (map[string]bool, error)
	// CreateUpload starts an upload.
	CreateUpload(ctx context.Context, p UploadParams) (*Upload, error)
	// GetUpload returns an upload and the parts done so far.
	GetUpload(ctx context.Context, id uuid.UUID) (*Upload, error)
	// UploadRepo returns the Repo an upload was started for (uuid.Nil if
	// none), so handlers can refuse parts sent through another Repo.
	UploadRepo(ctx context.Context, id uuid.UUID) (uuid.UUID, error)
	// PresignPart returns a direct URL for a part, or blob.ErrNotSupported.
	PresignPart(ctx context.Context, id uuid.UUID, part int, ttl time.Duration) (string, error)
	// PutPart uploads a part through Hangar and returns its ETag.
	PutPart(ctx context.Context, id uuid.UUID, part int, r io.Reader, size int64) (string, error)
	// CompleteUpload assembles the parts, verifies the object and records it,
	// with the upload's User as an uploader. When the object is already
	// stored, the parts are verified the same way and then discarded.
	// A failure wrapped in ErrRetryable keeps the upload and its assembled
	// parts, so the same completion can be sent again.
	CompleteUpload(ctx context.Context, id uuid.UUID, parts []blob.Part) (LFSObject, error)
	// AbortUpload discards an upload.
	AbortUpload(ctx context.Context, id uuid.UUID) error
	// PresignPut returns a direct single-PUT upload by userID, or
	// blob.ErrNotSupported.
	PresignPut(ctx context.Context, oid string, size int64, ttl time.Duration, userID uuid.UUID) (blob.PresignedRequest, error)
	// Verify checks an object userID wrote through a presigned PUT (anyone,
	// for uuid.Nil), marks it verified and records the uploader. An object
	// that is already stored needs such an upload too: with none, Verify
	// gives ErrNotFound, as for an object that does not exist.
	Verify(ctx context.Context, oid string, size int64, userID uuid.UUID) (LFSObject, error)
	// Open reads an object, or a range of it.
	Open(ctx context.Context, oid string, rng *blob.Range) (io.ReadCloser, LFSObject, error)
	// PresignGet returns a direct download URL, or blob.ErrNotSupported.
	PresignGet(ctx context.Context, oid string, ttl time.Duration, filename string) (string, error)
}

// ValidateCommitParams checks the parts of a Commit request that do not
// need the Repo: paths, operation fields and the summary.
func ValidateCommitParams(p CommitParams) error {
	if p.Summary == "" {
		return fmt.Errorf("%w: the commit summary is empty", ErrInvalid)
	}
	if len(p.Operations) == 0 {
		return fmt.Errorf("%w: the commit has no operations", ErrInvalid)
	}
	for i, op := range p.Operations {
		clean, err := model.CleanRepoPath(op.Path)
		if err != nil {
			return fmt.Errorf("%w: operation %d: %w", ErrInvalid, i, err)
		}
		if clean == "" && op.Kind != OpDeleteFolder {
			return fmt.Errorf("%w: operation %d has an empty path", ErrInvalid, i)
		}
		switch op.Kind {
		case OpAdd:
			if op.Content == nil {
				return fmt.Errorf("%w: operation %d (%s) has no content", ErrInvalid, i, op.Path)
			}
		case OpAddLFS:
			if !model.IsSHA256(op.OID) {
				return fmt.Errorf("%w: operation %d (%s) has an invalid sha256 oid", ErrInvalid, i, op.Path)
			}
		case OpCopy:
			if _, err := model.CleanRepoPath(op.SrcPath); err != nil || op.SrcPath == "" {
				return fmt.Errorf("%w: operation %d has an invalid copy source", ErrInvalid, i)
			}
		case OpDelete, OpDeleteFolder:
		default:
			return fmt.Errorf("%w: operation %d has unknown kind %q", ErrInvalid, i, op.Kind)
		}
	}
	return nil
}
