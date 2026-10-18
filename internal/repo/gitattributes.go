package repo

import (
	_ "embed"

	"github.com/hangar-hub/hangar/internal/model"
)

// The Hub's default .gitattributes for new Repos, copied byte for byte from
// Repos created on huggingface.co on 2026-10-13 (models: 35 rules; datasets:
// the same plus audio, image and video rules). New Repos get one as their
// first Commit (docs/spec/04-git.md "Server-side Commits").
var (
	//go:embed gitattributes_default.txt
	defaultModelGitAttributes []byte
	//go:embed gitattributes_dataset_default.txt
	defaultDatasetGitAttributes []byte
)

// DefaultGitAttributes returns the Hub's default .gitattributes for a Repo type.
func DefaultGitAttributes(t model.RepoType) []byte {
	if t == model.RepoTypeDataset {
		return append([]byte(nil), defaultDatasetGitAttributes...)
	}
	return append([]byte(nil), defaultModelGitAttributes...)
}
