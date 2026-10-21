// Package community holds discussions and pull requests (REPO-6), likes
// and download counts (REPO-7). The Hub API, the native API and the UI all
// use Service; communitytest has an in-memory fake.
package community

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/hangar-hub/hangar/internal/model"
)

// Errors.
var (
	ErrNotFound = errors.New("discussion not found")
	// ErrNotOpen means the change needs an open discussion or pull request.
	ErrNotOpen = errors.New("discussion is not open")
	// ErrNotPullRequest means the action only applies to pull requests.
	ErrNotPullRequest = errors.New("discussion is not a pull request")
	ErrInvalid        = errors.New("invalid request")
	// ErrConflict means a pull request cannot be merged because its base
	// changed the same files.
	ErrConflict = errors.New("pull request conflicts with its base branch")
	// ErrNotDiscardable means the pull request has commits or comments, so
	// it can only be closed.
	ErrNotDiscardable = errors.New("pull request has activity and cannot be discarded")
)

// Discussion kinds and statuses, as the Hub names them.
const (
	KindDiscussion  = "discussion"
	KindPullRequest = "pull_request"

	StatusDraft  = "draft"
	StatusOpen   = "open"
	StatusClosed = "closed"
	StatusMerged = "merged"
)

// Event types in a discussion thread.
const (
	EventComment      = "comment"
	EventStatusChange = "status-change"
	EventTitleChange  = "title-change"
	EventCommit       = "commit"
)

// Event is one entry in a thread.
type Event struct {
	ID     uuid.UUID
	Type   string
	Author string
	Body   string
	Data   map[string]any
	Hidden bool
	// Edited is true once a comment has been edited.
	Edited    bool
	CreatedAt time.Time
}

// Discussion is a discussion or a pull request.
type Discussion struct {
	Num    int
	Kind   string
	Title  string
	Status string
	Author string
	// PRRef is `refs/pr/N` for pull requests.
	PRRef string
	// PRBase is the branch the pull request targets.
	PRBase    string
	Events    []Event
	CreatedAt time.Time
}

// IsPullRequest reports whether the discussion is a pull request.
func (d *Discussion) IsPullRequest() bool { return d.Kind == KindPullRequest }

// ListOptions filters discussions, as the Hub API's get_repo_discussions does.
type ListOptions struct {
	// Type is "", "discussion" or "pull_request".
	Type string
	// Status is "", "open" or "closed".
	Status string
	Author string
	Page   int
	Limit  int
}

// CreateParams opens a discussion or pull request.
type CreateParams struct {
	RepoUUID    uuid.UUID
	Repo        model.RepoID
	Title       string
	Description string
	PullRequest bool
	// Base is the branch a pull request targets; empty means the default branch.
	Base   string
	Author model.Principal
}

// Service is the community service.
type Service interface {
	List(ctx context.Context, repoUUID uuid.UUID, opts ListOptions) ([]Discussion, int, error)
	Get(ctx context.Context, repoUUID uuid.UUID, num int) (*Discussion, error)
	// Create opens a discussion, or a pull request with PRRef `refs/pr/N`.
	// The ref itself is created by the first Commit to it.
	Create(ctx context.Context, p CreateParams) (*Discussion, error)
	Comment(ctx context.Context, repoUUID uuid.UUID, num int, body string, author model.Principal) (*Event, error)
	SetStatus(ctx context.Context, repoUUID uuid.UUID, num int, status, comment string, actor model.Principal) (*Event, error)
	Rename(ctx context.Context, repoUUID uuid.UUID, num int, title string, actor model.Principal) (*Event, error)
	// Merge merges a pull request into its base through repo.Service and
	// marks it merged.
	Merge(ctx context.Context, repoUUID uuid.UUID, num int, comment string, actor model.Principal) (*Event, error)
	// EditComment replaces a comment's text. Handlers check that the actor
	// wrote it or administers the Repo.
	EditComment(ctx context.Context, repoUUID uuid.UUID, num int, eventID uuid.UUID, body string, actor model.Principal) (*Event, error)
	// HideComment hides a comment's text.
	HideComment(ctx context.Context, repoUUID uuid.UUID, num int, eventID uuid.UUID, actor model.Principal) (*Event, error)
	// Discard removes a pull request that has no ref and no events but its
	// description, for a create_pr Commit that failed after opening it.
	Discard(ctx context.Context, repoUUID uuid.UUID, num int) error

	Like(ctx context.Context, userID, repoUUID uuid.UUID) error
	Unlike(ctx context.Context, userID, repoUUID uuid.UUID) error
	Likes(ctx context.Context, repoUUID uuid.UUID) (int64, error)
	// Liked reports whether a user likes a Repo.
	Liked(ctx context.Context, userID, repoUUID uuid.UUID) (bool, error)
	// Likers returns the usernames of the users who like a Repo, in name order.
	Likers(ctx context.Context, repoUUID uuid.UUID) ([]string, error)
	// LikedBy returns the Repos a user likes, newest like first.
	LikedBy(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error)
	// RecordDownload counts one download on a day (UTC).
	RecordDownload(ctx context.Context, repoUUID uuid.UUID, at time.Time) error
	// Downloads returns daily counts from since (inclusive), oldest first.
	Downloads(ctx context.Context, repoUUID uuid.UUID, since time.Time) ([]DayCount, error)
}

// Counter is implemented by services that count the likes and downloads
// of many Repos at once, for pages of Repos.
type Counter interface {
	// LikesOf returns what Likes returns for each Repo.
	LikesOf(ctx context.Context, repoUUIDs []uuid.UUID) (map[uuid.UUID]int64, error)
	// DownloadsOf returns each Repo's downloads on the days from since's
	// day (UTC) on, the total of what Downloads returns. A zero since
	// counts every day.
	DownloadsOf(ctx context.Context, repoUUIDs []uuid.UUID, since time.Time) (map[uuid.UUID]int64, error)
}

// DayCount is a day's download count.
type DayCount struct {
	Day   time.Time
	Count int64
}
