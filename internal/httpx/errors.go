package httpx

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/telemetry"
)

// Hub API error codes, sent in X-Error-Code (docs/research/hf-hub-api-contract.md §9).
const (
	CodeRepoNotFound     = "RepoNotFound"
	CodeRevisionNotFound = "RevisionNotFound"
	CodeEntryNotFound    = "EntryNotFound"
	CodeGatedRepo        = "GatedRepo"
)

// Hub API error messages that clients match exactly.
const (
	// MsgInvalidCredentials keeps huggingface_hub from mislabelling a bad
	// token as RepositoryNotFoundError.
	MsgInvalidCredentials = "Invalid credentials in Authorization header" //nolint:gosec // an error message, not a credential.
	// MsgDisabled makes huggingface_hub raise DisabledRepoError.
	MsgDisabled = "Access to this resource is disabled."
)

// Hub response headers.
const (
	HeaderErrorCode    = "X-Error-Code"
	HeaderErrorMessage = "X-Error-Message"
	HeaderRepoCommit   = "X-Repo-Commit"
)

// HubError is an error in the Hub API's format.
type HubError struct {
	Status int
	// Code is an X-Error-Code value, or "".
	Code    string
	Message string
	// RepoCommit is sent as X-Repo-Commit, for EntryNotFound from resolve.
	RepoCommit model.CommitSHA
	// Extra fields are added to the JSON body, such as `url` on a 409 from
	// /api/repos/create.
	Extra map[string]any
}

func (e *HubError) Error() string { return e.Message }

// WriteHubError writes a Hub API error: X-Error-Code (when set),
// X-Error-Message, X-Repo-Commit (when set) and a JSON body
// `{"error": "..."}`. HEAD requests get the headers and no body.
func WriteHubError(w http.ResponseWriter, r *http.Request, e *HubError) {
	h := w.Header()
	if e.Code != "" {
		h.Set(HeaderErrorCode, e.Code)
	}
	h.Set(HeaderErrorMessage, headerSafe(e.Message))
	if e.RepoCommit != "" {
		h.Set(HeaderRepoCommit, string(e.RepoCommit))
	}
	body := map[string]any{"error": e.Message}
	for k, v := range e.Extra {
		body[k] = v
	}
	writeJSON(w, r, e.Status, "application/json; charset=utf-8", body)
}

// HubRepoNotFound writes the Hub's RepoNotFound error: 401 when the caller
// sent no credentials, 404 otherwise, so private Repo names do not leak.
func HubRepoNotFound(w http.ResponseWriter, r *http.Request, id model.RepoID) {
	status := http.StatusNotFound
	if Principal(r.Context()).IsAnonymous() {
		status = http.StatusUnauthorized
	}
	WriteHubError(w, r, &HubError{
		Status:  status,
		Code:    CodeRepoNotFound,
		Message: "Repository Not Found for url: " + id.Path() + ". Please make sure you specified the correct `repo_id` and `repo_type`. If you are trying to access a private or gated repo, make sure you are authenticated.",
	})
}

// HubRevisionNotFound writes the Hub's RevisionNotFound error.
func HubRevisionNotFound(w http.ResponseWriter, r *http.Request, revision string) {
	WriteHubError(w, r, &HubError{Status: http.StatusNotFound, Code: CodeRevisionNotFound, Message: "Invalid rev id: " + revision})
}

// HubEntryNotFound writes the Hub's EntryNotFound error. Pass the resolved
// Commit so clients can cache the miss.
func HubEntryNotFound(w http.ResponseWriter, r *http.Request, path, revision string, commit model.CommitSHA) {
	WriteHubError(w, r, &HubError{
		Status:     http.StatusNotFound,
		Code:       CodeEntryNotFound,
		Message:    path + " does not exist on \"" + revision + "\"",
		RepoCommit: commit,
	})
}

// HubGated writes the Hub's GatedRepo error: 401 when the caller sent no
// credentials, so the datasets library asks them to sign in, and 403
// otherwise, so it asks them to request access.
func HubGated(w http.ResponseWriter, r *http.Request, id model.RepoID) {
	status := http.StatusForbidden
	if Principal(r.Context()).IsAnonymous() {
		status = http.StatusUnauthorized
	}
	WriteHubError(w, r, &HubError{
		Status:  status,
		Code:    CodeGatedRepo,
		Message: "Access to " + id.Path() + " is restricted. You must have access to it and be authenticated to access it. Please log in and request access on the Repo page.",
	})
}

// Problem is an RFC 9457 problem detail, the native API's error format.
type Problem struct {
	Type      string         `json:"type"`
	Title     string         `json:"title"`
	Status    int            `json:"status"`
	Detail    string         `json:"detail,omitempty"`
	Instance  string         `json:"instance,omitempty"`
	RequestID string         `json:"request_id,omitempty"`
	Errors    []ProblemError `json:"errors,omitempty"`
}

// ProblemError is one validation error in Problem.Errors.
type ProblemError struct {
	// Pointer is a JSON Pointer to the field, such as `/name`.
	Pointer string `json:"pointer,omitempty"`
	// Parameter names a query or path parameter.
	Parameter string `json:"parameter,omitempty"`
	Detail    string `json:"detail"`
}

func (p *Problem) Error() string {
	if p.Detail != "" {
		return p.Detail
	}
	return p.Title
}

// WriteProblem writes p as `application/problem+json`, filling in the
// title from the status, the type, and the request ID.
func WriteProblem(w http.ResponseWriter, r *http.Request, p *Problem) {
	if p.Type == "" {
		p.Type = "about:blank"
	}
	if p.Title == "" {
		p.Title = http.StatusText(p.Status)
	}
	if p.RequestID == "" {
		p.RequestID = RequestID(r.Context())
	}
	writeJSON(w, r, p.Status, "application/problem+json", p)
}

// WriteError writes an error in the format of the request's protocol:
// Hub-style for the Hub API, problem details for the native API, plain text
// for git, and JSON for the rest. code is an X-Error-Code for Hub requests.
func WriteError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	switch ProtocolOf(r.Context()) {
	case telemetry.ProtocolHub, telemetry.ProtocolXet:
		WriteHubError(w, r, &HubError{Status: status, Code: code, Message: message})
	case telemetry.ProtocolNative, telemetry.ProtocolUI, telemetry.ProtocolUnknown, telemetry.ProtocolHealth:
		WriteProblem(w, r, &Problem{Status: status, Detail: message})
	case telemetry.ProtocolGit:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(status)
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte(message + "\n"))
		}
	case telemetry.ProtocolLFS:
		// git-lfs reads `message` from a JSON body (docs/api/batch.md).
		writeJSON(w, r, status, "application/vnd.git-lfs+json", map[string]any{"message": message, "request_id": RequestID(r.Context())})
	case telemetry.ProtocolOllama:
		// The Docker registry error format that Ollama reads.
		writeJSON(w, r, status, "application/json", map[string]any{"errors": []map[string]string{{"code": registryCode(status), "message": message}}})
	default:
		writeJSON(w, r, status, "application/json", map[string]any{"error": message})
	}
}

func registryCode(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "UNAUTHORIZED"
	case http.StatusForbidden:
		return "DENIED"
	case http.StatusNotFound:
		return "NAME_UNKNOWN"
	default:
		return "UNKNOWN"
	}
}

// WriteUnauthorized writes the protocol's response to a bad credential:
// for the Hub API, 401 with the exact message huggingface_hub expects; for
// git and LFS, 401 with a Basic challenge so git asks for a password.
func WriteUnauthorized(w http.ResponseWriter, r *http.Request) {
	switch ProtocolOf(r.Context()) {
	case telemetry.ProtocolHub:
		WriteHubError(w, r, &HubError{Status: http.StatusUnauthorized, Message: MsgInvalidCredentials})
	case telemetry.ProtocolGit, telemetry.ProtocolLFS:
		w.Header().Set("WWW-Authenticate", `Basic realm="Hangar"`)
		WriteError(w, r, http.StatusUnauthorized, "", "Authentication failed: the Token is invalid, expired or revoked.")
	default:
		WriteError(w, r, http.StatusUnauthorized, "", "The credentials in the request are invalid, expired or revoked.")
	}
}

// WriteJSON writes v as JSON with the given status.
func WriteJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	writeJSON(w, r, status, "application/json; charset=utf-8", v)
}

func writeJSON(w http.ResponseWriter, r *http.Request, status int, contentType string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		status = http.StatusInternalServerError
		b = []byte(`{"error":"Internal server error"}`)
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if r == nil || r.Method != http.MethodHead {
		_, _ = w.Write(b)
	}
}

// headerSafe removes characters that cannot appear in a header value.
func headerSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r < 0x20 && r != '\t' {
			return ' '
		}
		return r
	}, s)
}
