package model

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// RepoType is the kind of content a Repo holds.
type RepoType string

// Repo types. Hangar does not support Spaces or kernels.
const (
	RepoTypeModel   RepoType = "model"
	RepoTypeDataset RepoType = "dataset"
)

// ErrInvalidRepoType is returned when a string does not name a supported Repo type.
var ErrInvalidRepoType = errors.New("invalid repo type")

// ParseRepoType accepts the singular and plural forms used by the Hub API and
// the native API ("model", "models", "dataset", "datasets"). An empty string
// means a model, as in the Hub's create endpoint.
func ParseRepoType(s string) (RepoType, error) {
	switch strings.ToLower(s) {
	case "", "model", "models":
		return RepoTypeModel, nil
	case "dataset", "datasets":
		return RepoTypeDataset, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrInvalidRepoType, s)
	}
}

// Valid reports whether t is a supported Repo type.
func (t RepoType) Valid() bool { return t == RepoTypeModel || t == RepoTypeDataset }

// Plural returns the plural form used in API paths: "models" or "datasets".
func (t RepoType) Plural() string { return string(t) + "s" }

// URLPrefix returns the prefix the Hub uses in front of `ns/name` in web,
// resolve and git URLs: "" for models and "datasets/" for datasets.
func (t RepoType) URLPrefix() string {
	if t == RepoTypeDataset {
		return "datasets/"
	}
	return ""
}

// RepoID identifies a Repo by type and `namespace/name`. A model and a
// dataset may share the same `namespace/name`.
type RepoID struct {
	Type      RepoType
	Namespace string
	Name      string
}

// ErrInvalidRepoID is returned by ParseRepoID and RepoID.Validate.
var ErrInvalidRepoID = errors.New("invalid repo id")

// repoNameRe follows the Hub's rule in huggingface_hub utils/_validators.py:
// letters, digits, '-', '_' and '.', at most 96 characters.
var repoNameRe = regexp.MustCompile(`^[A-Za-z0-9_.\-]{1,96}$`)

// ParseRepoID parses `namespace/name` with the given type.
func ParseRepoID(t RepoType, s string) (RepoID, error) {
	ns, name, ok := strings.Cut(s, "/")
	if !ok {
		return RepoID{}, fmt.Errorf("%w: %q must have the form namespace/name", ErrInvalidRepoID, s)
	}
	id := RepoID{Type: t, Namespace: ns, Name: name}
	if err := id.Validate(); err != nil {
		return RepoID{}, err
	}
	return id, nil
}

// Validate checks the type, the Namespace and the name.
func (id RepoID) Validate() error {
	if !id.Type.Valid() {
		return fmt.Errorf("%w: %q", ErrInvalidRepoType, id.Type)
	}
	if err := ValidateNamespaceName(id.Namespace); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRepoID, err)
	}
	return ValidateRepoName(id.Name)
}

// ValidateRepoName checks a Repo name against the Hub's rules: letters,
// digits, '-', '_' and '.', at most 96 characters, no "--" or "..", and not
// starting or ending with '-' or '.', or ending with ".git".
func ValidateRepoName(name string) error {
	switch {
	case !repoNameRe.MatchString(name):
		return fmt.Errorf("%w: name %q may only contain letters, digits, '-', '_' and '.', up to 96 characters", ErrInvalidRepoID, name)
	case strings.Contains(name, "--") || strings.Contains(name, ".."):
		return fmt.Errorf("%w: name %q cannot contain '--' or '..'", ErrInvalidRepoID, name)
	case strings.HasPrefix(name, "-") || strings.HasPrefix(name, ".") ||
		strings.HasSuffix(name, "-") || strings.HasSuffix(name, "."):
		return fmt.Errorf("%w: name %q cannot start or end with '-' or '.'", ErrInvalidRepoID, name)
	case strings.HasSuffix(strings.ToLower(name), ".git"):
		return fmt.Errorf("%w: name %q cannot end with .git", ErrInvalidRepoID, name)
	}
	return nil
}

// String returns `namespace/name`.
func (id RepoID) String() string { return id.Namespace + "/" + id.Name }

// Path returns the URL path of the Repo without a leading slash, as the Hub
// uses it in web and resolve URLs: `ns/name` or `datasets/ns/name`.
func (id RepoID) Path() string { return id.Type.URLPrefix() + id.String() }

// IsZero reports whether id is the zero value.
func (id RepoID) IsZero() bool { return id == RepoID{} }

// Equal compares two IDs, ignoring case in the Namespace and name, as Hangar
// treats names case-insensitively.
func (id RepoID) Equal(o RepoID) bool {
	return id.Type == o.Type && strings.EqualFold(id.Namespace, o.Namespace) && strings.EqualFold(id.Name, o.Name)
}

// Visibility controls who can read a Repo or Collection.
type Visibility string

// Visibility values, from REPO-2.
const (
	// VisibilityPublic means anyone, including anonymous users when the
	// Instance allows anonymous reads.
	VisibilityPublic Visibility = "public"
	// VisibilityInternal means any signed-in user.
	VisibilityInternal Visibility = "internal"
	// VisibilityPrivate means Namespace members, collaborators and Instance admins.
	VisibilityPrivate Visibility = "private"
)

// ParseVisibility parses a Visibility value.
func ParseVisibility(s string) (Visibility, error) {
	v := Visibility(strings.ToLower(s))
	if !v.Valid() {
		return "", fmt.Errorf("invalid visibility %q: use public, internal or private", s)
	}
	return v, nil
}

// Valid reports whether v is a known Visibility.
func (v Visibility) Valid() bool {
	return v == VisibilityPublic || v == VisibilityInternal || v == VisibilityPrivate
}

// Private reports whether the Hub API should show the Repo as private. The
// Hub has no internal visibility, so internal Repos are reported as private.
func (v Visibility) Private() bool { return v != VisibilityPublic }
