package index

import (
	"github.com/google/uuid"

	"github.com/hangar-hub/hangar/internal/model"
)

// CommitJob is the payload of the index and scan Jobs that run for every
// Commit that becomes the head of a branch (and of the scan Jobs for tag
// and pull request tips). Its JSON matches repo.CommitJob, which the Repo
// service enqueues.
type CommitJob struct {
	RepoUUID uuid.UUID       `json:"repo_uuid"`
	Repo     string          `json:"repo"`
	Type     model.RepoType  `json:"type"`
	Commit   model.CommitSHA `json:"commit"`
}

// DedupeKey is the Job dedupe key the Repo service uses: `<uuid>:<commit>`.
func (j CommitJob) DedupeKey() string { return j.RepoUUID.String() + ":" + string(j.Commit) }
