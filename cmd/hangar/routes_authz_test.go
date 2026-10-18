package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/api/gen"
	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/config"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
)

// rule is the row of the permission table in docs/spec/05-auth.md that a
// route applies, or one of the rules for routes outside that table.
type rule string

const (
	// ruleOpen needs no credential: health, the static UI, sign-in, Hub
	// listings, device-code login and Ollama's token endpoint, which
	// verifies its own signature.
	ruleOpen rule = "open"
	// ruleNotFound routes answer 404 or 405 to everyone.
	ruleNotFound rule = "not_found"
	// ruleSignedIn needs any signed-in user and acts on their own data.
	ruleSignedIn rule = "signed_in"
	// ruleSignedInWrite is ruleSignedIn for changes: a read Token is refused.
	ruleSignedInWrite rule = "signed_in_write"
	// ruleProfile is a user's or Organisation's profile or member list
	// (read on a Namespace): any signed-in user while
	// security.anonymous_read is off, as on the matrix's Instance, and
	// anyone while it is on, which TestRouteAuthorisationMatrix also
	// probes. Decided by James on 2026-10-18.
	ruleProfile rule = "profile"
	// Rows of the permission table, on a private Repo of alice's.
	ruleRead      rule = "read"
	ruleReadFiles rule = "read_files"
	rulePull      rule = "pull_mirror"
	ruleWrite     rule = "write"
	ruleDiscuss   rule = "discuss"
	ruleAdmin     rule = "admin"
	ruleHubAdmin  rule = "hub_admin"
	// ruleCreateRepo creates a Repo in alice's Namespace.
	ruleCreateRepo rule = "create_repo"
	// ruleManageNamespace manages an Organisation alice administers.
	ruleManageNamespace rule = "manage_namespace"
	// ruleInstanceAdmin needs an Instance admin with an unrestricted admin Token.
	ruleInstanceAdmin rule = "instance_admin"
	// ruleSessionOnly needs a UI session; every Token is refused, so a
	// leaked Token cannot link sign-in providers or approve device logins.
	ruleSessionOnly rule = "session_only"
	// ruleCredentialExchange takes a password or a code and answers 401 or
	// 403 for a wrong one, whoever asks.
	ruleCredentialExchange rule = "credential_exchange"
	// ruleOwnCredential routes authorise themselves with a signature or a
	// protocol token (signed links, Xet CAS tokens); a Hangar Token alone is
	// not enough.
	ruleOwnCredential rule = "own_credential"
)

// personas are the callers every route is probed as.
var personas = []string{"anonymous", "outsider", "alice-read", "alice-write", "alice-admin", "root"}

// allowedBy lists, per rule, the personas that must not be refused. Every
// other persona must get 401, 403 or 404. alice owns the Repo (admin Role)
// and administers the Organisation; outsider is a signed-in user with a
// write Token and no Role; root is an Instance admin with an admin Token.
var allowedBy = map[rule][]string{
	ruleOpen:            personas,
	ruleSignedIn:        {"outsider", "alice-read", "alice-write", "alice-admin", "root"},
	ruleSignedInWrite:   {"outsider", "alice-write", "alice-admin", "root"},
	ruleProfile:         {"outsider", "alice-read", "alice-write", "alice-admin", "root"},
	ruleRead:            {"alice-read", "alice-write", "alice-admin", "root"},
	ruleReadFiles:       {"alice-read", "alice-write", "alice-admin", "root"},
	rulePull:            {"alice-write", "alice-admin", "root"},
	ruleWrite:           {"alice-write", "alice-admin", "root"},
	ruleDiscuss:         {"alice-write", "alice-admin", "root"},
	ruleAdmin:           {"alice-admin", "root"},
	ruleHubAdmin:        {"alice-write", "alice-admin", "root"},
	ruleCreateRepo:      {"alice-write", "alice-admin", "root"},
	ruleManageNamespace: {"alice-admin", "root"},
	ruleInstanceAdmin:   {"root"},
	ruleSessionOnly:     {},
}

// routeCase is one route and a request that reaches it. Placeholders in
// path and body: {repo} is a fresh private model Repo of alice's for this
// case, {dataset} a fresh private dataset Repo, {org} a fresh Organisation
// alice administers, {sha} the head Commit of {repo}, {webhook}, {job},
// {collection} and {discussion} fixtures on {repo}, and {uuid} a random ID.
type routeCase struct {
	method, route string
	rule          rule
	path, body    string
	header        map[string]string
}

func (c routeCase) key() string { return c.method + " " + c.route }

const sha64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// routeMatrix is the authorisation decision for every route Hangar serves.
// TestRouteAuthorisationMatrix fails when a router gains a route that is
// not here, so every new route needs a decision.
var routeMatrix = []routeCase{
	// Health and the web UI.
	{method: "GET", route: "/healthz", rule: ruleOpen, path: "/healthz"},
	{method: "GET", route: "/readyz", rule: ruleOpen, path: "/readyz"},
	{method: "GET", route: "/{ui}", rule: ruleOpen, path: "/alice/somewhere"},
	{method: "GET", route: "/assets/{file}", rule: ruleOpen, path: "/assets/missing.js"},

	// Sign-in routes outside the native API.
	{method: "GET", route: "/auth/oidc/{provider}", rule: ruleOpen, path: "/auth/oidc/nope"},
	{method: "GET", route: "/auth/oidc/{provider}/callback", rule: ruleOpen, path: "/auth/oidc/nope/callback?state=x&code=y"},
	{method: "POST", route: "/auth/oidc/{provider}/link", rule: ruleSessionOnly, path: "/auth/oidc/nope/link"},
	{method: "DELETE", route: "/auth/oidc/{provider}/link", rule: ruleSessionOnly, path: "/auth/oidc/nope/link"},
	{method: "POST", route: "/auth/register", rule: ruleOpen, path: "/auth/register", body: `{}`},
	{method: "POST", route: "/auth/invite/{code}", rule: ruleOpen, path: "/auth/invite/nope", body: `{}`},
	{method: "POST", route: "/auth/reset/{code}", rule: ruleOpen, path: "/auth/reset/nope", body: `{}`},
	// The code in a confirmation link is the credential, as for a reset.
	{method: "POST", route: "/auth/email/{code}", rule: ruleOpen, path: "/auth/email/nope"},

	// Signed links.
	{method: "GET", route: "/api/hangar/v1/exports/{file}", rule: ruleOwnCredential, path: "/api/hangar/v1/exports/x.zip"},
	{method: "GET", route: "/api/hangar/v1/pull-through/{repo}/{oid}", rule: ruleOwnCredential, path: "/api/hangar/v1/pull-through/{uuid}/" + sha64 + "?size=1"},

	// Hub API.
	{method: "GET", route: "/api/whoami-v2", rule: ruleSignedIn, path: "/api/whoami-v2"},
	{method: "POST", route: "/api/validate-yaml", rule: ruleOpen, path: "/api/validate-yaml", body: `{"content":"a: 1","repoType":"model"}`},
	{method: "GET", route: "/api/users/{user}/likes", rule: ruleProfile, path: "/api/users/alice/likes"},
	{method: "GET", route: "/api/models", rule: ruleOpen, path: "/api/models"},
	{method: "GET", route: "/api/datasets", rule: ruleOpen, path: "/api/datasets"},
	{method: "GET", route: "/api/models/{name}", rule: ruleOpen, path: "/api/models/gpt2"},
	{method: "GET", route: "/{name}/resolve/{rev}/{path}", rule: ruleOpen, path: "/gpt2/resolve/main/config.json"},
	{method: "GET", route: "/{hub-unknown}", rule: ruleNotFound, path: "/api/nothing-here"},
	{method: "POST", route: "/oauth/device", rule: ruleOpen, path: "/oauth/device", body: "client_id=hf"},
	{method: "POST", route: "/oauth/token", rule: ruleOpen, path: "/oauth/token", body: "grant_type=urn:ietf:params:oauth:grant-type:device_code&device_code=nope"},
	{method: "POST", route: "/api/repos/create", rule: ruleCreateRepo, path: "/api/repos/create", body: `{"name":"created-{n}","organization":"alice","type":"model","private":true}`}, //nolint:misspell // Hub API field name.
	{method: "DELETE", route: "/api/repos/delete", rule: ruleHubAdmin, path: "/api/repos/delete", body: `{"name":"{name}","organization":"alice","type":"model"}`},                     //nolint:misspell // Hub API field name.
	{method: "POST", route: "/api/repos/move", rule: ruleHubAdmin, path: "/api/repos/move", body: `{"fromRepo":"{repo}","toRepo":"{repo}-moved","type":"model"}`},
	{method: "GET", route: "/api/collections", rule: ruleOpen, path: "/api/collections"},
	{method: "POST", route: "/api/collections", rule: ruleSignedInWrite, path: "/api/collections", body: `{"title":"Probe {n}","private":true}`},
	{method: "GET", route: "/api/collections/{ns}/{slug}", rule: ruleRead, path: "/api/collections/{collection}"},
	{method: "PATCH", route: "/api/collections/{ns}/{slug}", rule: ruleWrite, path: "/api/collections/{collection}", body: `{"title":"Renamed"}`},
	{method: "DELETE", route: "/api/collections/{ns}/{slug}", rule: ruleWrite, path: "/api/collections/{collection}"},
	{method: "POST", route: "/api/collections/{ns}/{slug}/items", rule: ruleWrite, path: "/api/collections/{collection}/items", body: `{"item":{"type":"model","id":"{repo}"}}`},
	{method: "PATCH", route: "/api/collections/{ns}/{slug}/items/{id}", rule: ruleWrite, path: "/api/collections/{collection}/items/{uuid}", body: `{"note":"x"}`},
	{method: "DELETE", route: "/api/collections/{ns}/{slug}/items/{id}", rule: ruleWrite, path: "/api/collections/{collection}/items/{uuid}"},
	{method: "GET", route: "/api/models/{ns}/{name}", rule: ruleRead, path: "/api/models/{repo}"},
	{method: "GET", route: "/api/models/{ns}/{name}/revision/{rev}", rule: ruleRead, path: "/api/models/{repo}/revision/main"},
	{method: "GET", route: "/api/models/{ns}/{name}/tree/{rev}/{path}", rule: ruleRead, path: "/api/models/{repo}/tree/main"},
	{method: "POST", route: "/api/models/{ns}/{name}/paths-info/{rev}", rule: ruleRead, path: "/api/models/{repo}/paths-info/main", body: "paths=README.md",
		header: map[string]string{"Content-Type": "application/x-www-form-urlencoded"}},
	{method: "GET", route: "/api/models/{ns}/{name}/refs", rule: ruleRead, path: "/api/models/{repo}/refs"},
	{method: "GET", route: "/api/models/{ns}/{name}/commits/{rev}", rule: ruleRead, path: "/api/models/{repo}/commits/main"},
	{method: "POST", route: "/api/models/{ns}/{name}/branch/{branch}", rule: ruleWrite, path: "/api/models/{repo}/branch/probe", body: `{}`},
	{method: "DELETE", route: "/api/models/{ns}/{name}/branch/{branch}", rule: ruleWrite, path: "/api/models/{repo}/branch/probe"},
	{method: "POST", route: "/api/models/{ns}/{name}/tag/{rev}", rule: ruleWrite, path: "/api/models/{repo}/tag/main", body: `{"tag":"v{n}"}`},
	{method: "DELETE", route: "/api/models/{ns}/{name}/tag/{rev}", rule: ruleWrite, path: "/api/models/{repo}/tag/v1"},
	{method: "PUT", route: "/api/models/{ns}/{name}/settings", rule: ruleHubAdmin, path: "/api/models/{repo}/settings", body: `{"private":true}`},
	{method: "POST", route: "/api/models/{ns}/{name}/preupload/{rev}", rule: ruleWrite, path: "/api/models/{repo}/preupload/main", body: `{"files":[{"path":"a.txt","size":1,"sample":"YQ=="}]}`},
	{method: "POST", route: "/api/models/{ns}/{name}/commit/{rev}", rule: ruleWrite, path: "/api/models/{repo}/commit/main",
		body:   `{"key":"header","value":{"summary":"probe"}}` + "\n" + `{"key":"file","value":{"path":"p{n}.txt","content":"eA==","encoding":"base64"}}` + "\n",
		header: map[string]string{"Content-Type": "application/x-ndjson"}},
	{method: "GET", route: "/api/models/{ns}/{name}/discussions", rule: ruleRead, path: "/api/models/{repo}/discussions"},
	{method: "POST", route: "/api/models/{ns}/{name}/discussions", rule: ruleDiscuss, path: "/api/models/{repo}/discussions", body: `{"title":"Probe"}`},
	{method: "GET", route: "/api/models/{ns}/{name}/discussions/{num}", rule: ruleRead, path: "/api/models/{repo}/discussions/{discussion}"},
	{method: "POST", route: "/api/models/{ns}/{name}/discussions/{num}/comment", rule: ruleDiscuss, path: "/api/models/{repo}/discussions/{discussion}/comment", body: `{"comment":"Hi"}`},
	{method: "POST", route: "/api/models/{ns}/{name}/discussions/{num}/comment/{id}/edit", rule: ruleDiscuss, path: "/api/models/{repo}/discussions/{discussion}/comment/{comment}/edit", body: `{"content":"Edited"}`},
	{method: "POST", route: "/api/models/{ns}/{name}/discussions/{num}/comment/{id}/hide", rule: ruleWrite, path: "/api/models/{repo}/discussions/{discussion}/comment/{comment}/hide"},
	{method: "POST", route: "/api/models/{ns}/{name}/discussions/{num}/merge", rule: ruleWrite, path: "/api/models/{repo}/discussions/{discussion}/merge"},
	{method: "POST", route: "/api/models/{ns}/{name}/discussions/{num}/status", rule: ruleDiscuss, path: "/api/models/{repo}/discussions/{discussion}/status", body: `{"status":"closed"}`},
	{method: "POST", route: "/api/models/{ns}/{name}/discussions/{num}/title", rule: ruleDiscuss, path: "/api/models/{repo}/discussions/{discussion}/title", body: `{"title":"New"}`},
	{method: "GET", route: "/api/models/{ns}/{name}/likers", rule: ruleRead, path: "/api/models/{repo}/likers"},
	{method: "POST", route: "/api/models/{ns}/{name}/like", rule: ruleDiscuss, path: "/api/models/{repo}/like"},
	{method: "DELETE", route: "/api/models/{ns}/{name}/like", rule: ruleDiscuss, path: "/api/models/{repo}/like"},
	{method: "GET", route: "/api/models/{ns}/{name}/xet-write-token/{rev}", rule: ruleWrite, path: "/api/models/{repo}/xet-write-token/main"},
	{method: "GET", route: "/api/datasets/{ns}/{name}/parquet/{config}/{split}/{n}", rule: ruleReadFiles, path: "/api/datasets/{dataset}/parquet"},
	{method: "GET", route: "/api/datasets/{ns}/{name}/croissant", rule: ruleRead, path: "/api/datasets/{dataset}/croissant"},
	{method: "GET", route: "/{ns}/{name}/resolve/{rev}/{path}", rule: ruleReadFiles, path: "/{repo}/resolve/main/README.md"},
	{method: "GET", route: "/datasets/{ns}/{name}/resolve/{rev}/{path}", rule: ruleReadFiles, path: "/datasets/{dataset}/resolve/main/README.md"},

	// git smart HTTP.
	{method: "GET", route: "/{ns}/{name}/info/refs", rule: ruleReadFiles, path: "/{repo}.git/info/refs?service=git-upload-pack"},
	{method: "POST", route: "/{ns}/{name}/git-upload-pack", rule: ruleReadFiles, path: "/{repo}.git/git-upload-pack", body: "0000",
		header: map[string]string{"Content-Type": "application/x-git-upload-pack-request"}},
	{method: "POST", route: "/{ns}/{name}/git-receive-pack", rule: ruleWrite, path: "/{repo}.git/git-receive-pack", body: "0000",
		header: map[string]string{"Content-Type": "application/x-git-receive-pack-request"}},
	{method: "GET", route: "/{ns}/{name}.git/{path}", rule: ruleNotFound, path: "/{repo}.git/objects/info/packs"},
	{method: "GET", route: "/datasets/{ns}/{name}/info/refs", rule: ruleReadFiles, path: "/datasets/{dataset}.git/info/refs?service=git-upload-pack"},
	{method: "GET", route: "/{git}", rule: ruleNotFound, path: "/ab/x.git/./info/refs"},

	// git-lfs.
	{method: "POST", route: "/{ns}/{name}.git/info/lfs/objects/batch", rule: ruleReadFiles, path: "/{repo}.git/info/lfs/objects/batch",
		body:   `{"operation":"download","objects":[{"oid":"` + sha64 + `","size":1}]}`,
		header: map[string]string{"Content-Type": "application/vnd.git-lfs+json", "Accept": "application/vnd.git-lfs+json"}},
	{method: "GET", route: "/{ns}/{name}.git/info/lfs/objects/{oid}", rule: ruleOwnCredential, path: "/{repo}.git/info/lfs/objects/" + sha64},
	{method: "PUT", route: "/{ns}/{name}.git/info/lfs/objects/{oid}", rule: ruleOwnCredential, path: "/{repo}.git/info/lfs/objects/" + sha64, body: "x"},
	{method: "POST", route: "/{ns}/{name}.git/info/lfs/verify", rule: ruleOwnCredential, path: "/{repo}.git/info/lfs/verify", body: `{"oid":"` + sha64 + `","size":1}`,
		header: map[string]string{"Content-Type": "application/vnd.git-lfs+json"}},
	{method: "POST", route: "/{ns}/{name}.git/info/lfs/uploads/{id}", rule: ruleOwnCredential, path: "/{repo}.git/info/lfs/uploads/{uuid}", body: `{}`},
	{method: "PUT", route: "/{ns}/{name}.git/info/lfs/uploads/{id}/{part}", rule: ruleOwnCredential, path: "/{repo}.git/info/lfs/uploads/{uuid}/1", body: "x"},
	{method: "GET", route: "/{ns}/{name}.git/info/lfs/locks", rule: ruleNotFound, path: "/{repo}.git/info/lfs/locks"},
	{method: "GET", route: "/{ns}/{name}.git/info/lfs/{path}", rule: ruleNotFound, path: "/{repo}.git/info/lfs/other"},
	{method: "GET", route: "/{lfs}", rule: ruleNotFound, path: "/ab/x.git/info/lfs/./batch"},

	// Ollama registry.
	{method: "GET", route: "/v2/", rule: ruleOpen, path: "/v2/"},
	{method: "GET", route: "/v2/token", rule: ruleOwnCredential, path: "/v2/token?scope=repository:alice/x:pull"},
	{method: "GET", route: "/v2/{ns}/{name}/manifests/{tag}", rule: ruleReadFiles, path: "/v2/{repo}/manifests/latest"},
	{method: "GET", route: "/v2/{ns}/{name}/blobs/{digest}", rule: ruleReadFiles, path: "/v2/{repo}/blobs/sha256:" + sha64},
	{method: "PUT", route: "/v2/{ns}/{name}/manifests", rule: ruleNotFound, path: "/v2/{repo}/manifests/latest", body: `{}`},
	{method: "POST", route: "/v2/{ns}/{name}/blobs", rule: ruleNotFound, path: "/v2/{repo}/blobs/uploads/", body: `{}`},

	// Xet CAS.
	{method: "POST", route: "/v1/telemetry", rule: ruleOpen, path: "/v1/telemetry", body: `{}`},
	{method: "GET", route: "/xet-cas/v1/chunks/{prefix}/{hash}", rule: ruleOwnCredential, path: "/xet-cas/v1/chunks/default/" + sha64},
	{method: "POST", route: "/xet-cas/v1/xorbs/{prefix}/{hash}", rule: ruleOwnCredential, path: "/xet-cas/v1/xorbs/default/" + sha64, body: "x"},
	{method: "POST", route: "/xet-cas/v1/shards", rule: ruleOwnCredential, path: "/xet-cas/v1/shards", body: "x"},
	{method: "POST", route: "/xet-cas/v2/shards", rule: ruleOwnCredential, path: "/xet-cas/v2/shards", body: "x"},

	// Dataset Viewer. Row routes check read_files on the dataset.
	{method: "GET", route: "/datasets-server/healthcheck", rule: ruleOpen, path: "/datasets-server/healthcheck"},
	{method: "GET", route: "/datasets-server/assets/...", rule: ruleOwnCredential, path: "/datasets-server/assets/{dataset}/--/main/--/default/train/0/image/image.jpg"},
	{method: "GET", route: "/datasets-server/is-valid", rule: ruleRead, path: "/datasets-server/is-valid?dataset={dataset}"},
	{method: "GET", route: "/datasets-server/splits", rule: ruleRead, path: "/datasets-server/splits?dataset={dataset}"},
	{method: "GET", route: "/datasets-server/first-rows", rule: ruleReadFiles, path: "/datasets-server/first-rows?dataset={dataset}&config=default&split=train"},
	{method: "GET", route: "/datasets-server/rows", rule: ruleReadFiles, path: "/datasets-server/rows?dataset={dataset}&config=default&split=train"},
	{method: "GET", route: "/datasets-server/search", rule: ruleReadFiles, path: "/datasets-server/search?dataset={dataset}&config=default&split=train&query=x"},
	{method: "GET", route: "/datasets-server/filter", rule: ruleReadFiles, path: "/datasets-server/filter?dataset={dataset}&config=default&split=train&where=%22text%22%3D%27x%27"},
	{method: "GET", route: "/datasets-server/parquet", rule: ruleReadFiles, path: "/datasets-server/parquet?dataset={dataset}"},
	{method: "GET", route: "/datasets-server/info", rule: ruleRead, path: "/datasets-server/info?dataset={dataset}"},
	{method: "GET", route: "/datasets-server/size", rule: ruleRead, path: "/datasets-server/size?dataset={dataset}"},
	{method: "GET", route: "/datasets-server/statistics", rule: ruleReadFiles, path: "/datasets-server/statistics?dataset={dataset}&config=default&split=train"},
	{method: "GET", route: "/datasets-server/opt-in-out-urls", rule: ruleRead, path: "/datasets-server/opt-in-out-urls?dataset={dataset}"},
	{method: "GET", route: "/datasets-server/presidio-entities", rule: ruleRead, path: "/datasets-server/presidio-entities?dataset={dataset}"},
	{method: "GET", route: "/datasets-server/{endpoint}", rule: ruleNotFound, path: "/datasets-server/nothing"},

	// Native API.
	{method: "GET", route: "/api/hangar/v1/me", rule: ruleOpen, path: "/api/hangar/v1/me"},
	// Self-service profile and password (W8 8, decided by James on
	// 2026-10-18): a write Token may change the profile, as for the other
	// /me changes, but only a UI session may change a password.
	{method: "PATCH", route: "/api/hangar/v1/me", rule: ruleSignedInWrite, path: "/api/hangar/v1/me", body: `{"display_name":"Probe {n}"}`},
	{method: "POST", route: "/api/hangar/v1/me/password", rule: ruleSessionOnly, path: "/api/hangar/v1/me/password", body: `{"current_password":"wrong password 1","new_password":"a new password 2"}`},
	{method: "POST", route: "/api/hangar/v1/auth/login", rule: ruleCredentialExchange, path: "/api/hangar/v1/auth/login", body: `{"username":"nobody","password":"wrong password 1"}`},
	{method: "POST", route: "/api/hangar/v1/auth/logout", rule: ruleOpen, path: "/api/hangar/v1/auth/logout"},
	{method: "GET", route: "/api/hangar/v1/auth/providers", rule: ruleOpen, path: "/api/hangar/v1/auth/providers"},
	{method: "GET", route: "/api/hangar/v1/me/sessions", rule: ruleSignedIn, path: "/api/hangar/v1/me/sessions"},
	{method: "DELETE", route: "/api/hangar/v1/me/sessions/{id}", rule: ruleSignedInWrite, path: "/api/hangar/v1/me/sessions/{uuid}"},
	{method: "GET", route: "/api/hangar/v1/me/tokens", rule: ruleSignedIn, path: "/api/hangar/v1/me/tokens"},
	{method: "POST", route: "/api/hangar/v1/me/tokens", rule: ruleSignedInWrite, path: "/api/hangar/v1/me/tokens", body: `{"name":"probe-{n}","scope":"read"}`},
	{method: "DELETE", route: "/api/hangar/v1/me/tokens/{id}", rule: ruleSignedInWrite, path: "/api/hangar/v1/me/tokens/{uuid}"},
	{method: "POST", route: "/api/hangar/v1/me/device", rule: ruleSessionOnly, path: "/api/hangar/v1/me/device", body: `{"user_code":"ABCD-EFGH","approve":false}`},
	{method: "GET", route: "/api/hangar/v1/me/ollama-keys", rule: ruleSignedIn, path: "/api/hangar/v1/me/ollama-keys"},
	{method: "POST", route: "/api/hangar/v1/me/ollama-keys", rule: ruleSignedInWrite, path: "/api/hangar/v1/me/ollama-keys", body: `{"public_key":"ssh-ed25519 nope"}`},
	{method: "DELETE", route: "/api/hangar/v1/me/ollama-keys/{id}", rule: ruleSignedInWrite, path: "/api/hangar/v1/me/ollama-keys/{uuid}"},
	{method: "GET", route: "/api/hangar/v1/me/upstream-tokens/{upstream}", rule: ruleSignedIn, path: "/api/hangar/v1/me/upstream-tokens/huggingface"},
	{method: "PUT", route: "/api/hangar/v1/me/upstream-tokens/{upstream}", rule: ruleSignedInWrite, path: "/api/hangar/v1/me/upstream-tokens/huggingface", body: `{"token":"hf_probe"}`},
	{method: "DELETE", route: "/api/hangar/v1/me/upstream-tokens/{upstream}", rule: ruleSignedInWrite, path: "/api/hangar/v1/me/upstream-tokens/huggingface"},
	{method: "GET", route: "/api/hangar/v1/search", rule: ruleOpen, path: "/api/hangar/v1/search?q=probe"},
	{method: "GET", route: "/api/hangar/v1/repos", rule: ruleOpen, path: "/api/hangar/v1/repos"},
	{method: "POST", route: "/api/hangar/v1/repos", rule: ruleCreateRepo, path: "/api/hangar/v1/repos", body: `{"type":"model","namespace":"alice","name":"native-{n}","visibility":"private"}`},
	{method: "GET", route: "/api/hangar/v1/repos/{type}/{ns}/{name}", rule: ruleRead, path: "/api/hangar/v1/repos/models/{repo}"},
	{method: "PATCH", route: "/api/hangar/v1/repos/{type}/{ns}/{name}", rule: ruleAdmin, path: "/api/hangar/v1/repos/models/{repo}", body: `{"description":"x"}`},
	{method: "DELETE", route: "/api/hangar/v1/repos/{type}/{ns}/{name}", rule: ruleAdmin, path: "/api/hangar/v1/repos/models/{repo}"},
	{method: "POST", route: "/api/hangar/v1/repos/{type}/{ns}/{name}/restore", rule: ruleAdmin, path: "/api/hangar/v1/repos/models/{repo}/restore"},
	{method: "POST", route: "/api/hangar/v1/repos/{type}/{ns}/{name}/rename", rule: ruleAdmin, path: "/api/hangar/v1/repos/models/{repo}/rename", body: `{"name":"{name}-renamed"}`},
	{method: "POST", route: "/api/hangar/v1/repos/{type}/{ns}/{name}/transfer", rule: ruleAdmin, path: "/api/hangar/v1/repos/models/{repo}/transfer", body: `{"namespace":"{org}"}`},
	{method: "GET", route: "/api/hangar/v1/repos/{type}/{ns}/{name}/refs", rule: ruleRead, path: "/api/hangar/v1/repos/models/{repo}/refs"},
	{method: "GET", route: "/api/hangar/v1/repos/{type}/{ns}/{name}/commits", rule: ruleRead, path: "/api/hangar/v1/repos/models/{repo}/commits"},
	{method: "POST", route: "/api/hangar/v1/repos/{type}/{ns}/{name}/commits", rule: ruleWrite, path: "/api/hangar/v1/repos/models/{repo}/commits",
		body: `{"branch":"main","summary":"probe","operations":[{"op":"add","path":"n{n}.txt","content_base64":"eA=="}]}`},
	{method: "GET", route: "/api/hangar/v1/repos/{type}/{ns}/{name}/commits/{sha}", rule: ruleRead, path: "/api/hangar/v1/repos/models/{repo}/commits/{sha}"},
	{method: "GET", route: "/api/hangar/v1/repos/{type}/{ns}/{name}/commits/{sha}/diff", rule: ruleReadFiles, path: "/api/hangar/v1/repos/models/{repo}/commits/{sha}/diff?path=README.md"},
	{method: "GET", route: "/api/hangar/v1/repos/{type}/{ns}/{name}/tree/{rev}", rule: ruleRead, path: "/api/hangar/v1/repos/models/{repo}/tree/main"},
	{method: "GET", route: "/api/hangar/v1/repos/{type}/{ns}/{name}/tree/{rev}/{path}", rule: ruleRead, path: "/api/hangar/v1/repos/models/{repo}/tree/main/sub"},
	{method: "GET", route: "/api/hangar/v1/repos/{type}/{ns}/{name}/metadata", rule: ruleRead, path: "/api/hangar/v1/repos/models/{repo}/metadata"},
	{method: "GET", route: "/api/hangar/v1/repos/{type}/{ns}/{name}/files/{rev}/{path}/header", rule: ruleReadFiles, path: "/api/hangar/v1/repos/models/{repo}/files/main/README.md/header"},
	{method: "GET", route: "/api/hangar/v1/repos/{type}/{ns}/{name}/files/{rev}/{path}/scan", rule: ruleRead, path: "/api/hangar/v1/repos/models/{repo}/files/main/README.md/scan"},
	{method: "GET", route: "/api/hangar/v1/repos/{type}/{ns}/{name}/quants", rule: ruleRead, path: "/api/hangar/v1/repos/models/{repo}/quants"},
	{method: "GET", route: "/api/hangar/v1/repos/{type}/{ns}/{name}/dependencies", rule: ruleRead, path: "/api/hangar/v1/repos/models/{repo}/dependencies"},
	{method: "POST", route: "/api/hangar/v1/repos/{type}/{ns}/{name}/like", rule: ruleDiscuss, path: "/api/hangar/v1/repos/models/{repo}/like"},
	{method: "DELETE", route: "/api/hangar/v1/repos/{type}/{ns}/{name}/like", rule: ruleDiscuss, path: "/api/hangar/v1/repos/models/{repo}/like"},
	{method: "GET", route: "/api/hangar/v1/repos/{type}/{ns}/{name}/stats", rule: ruleRead, path: "/api/hangar/v1/repos/models/{repo}/stats"},
	{method: "GET", route: "/api/hangar/v1/repos/{type}/{ns}/{name}/collaborators", rule: ruleAdmin, path: "/api/hangar/v1/repos/models/{repo}/collaborators"},
	{method: "PUT", route: "/api/hangar/v1/repos/{type}/{ns}/{name}/collaborators", rule: ruleAdmin, path: "/api/hangar/v1/repos/models/{repo}/collaborators", body: `{"items":[]}`},
	{method: "GET", route: "/api/hangar/v1/repos/{type}/{ns}/{name}/access-requests", rule: ruleAdmin, path: "/api/hangar/v1/repos/models/{repo}/access-requests"},
	{method: "POST", route: "/api/hangar/v1/repos/{type}/{ns}/{name}/access-requests", rule: ruleRead, path: "/api/hangar/v1/repos/models/{repo}/access-requests", body: `{"fields":{}}`},
	{method: "PATCH", route: "/api/hangar/v1/repos/{type}/{ns}/{name}/access-requests/{id}", rule: ruleAdmin, path: "/api/hangar/v1/repos/models/{repo}/access-requests/{uuid}", body: `{"status":"accepted"}`},
	{method: "POST", route: "/api/hangar/v1/repos/{type}/{ns}/{name}/uploads", rule: ruleWrite, path: "/api/hangar/v1/repos/models/{repo}/uploads",
		body: `{"revision":"main","files":[{"path":"big.bin","size":1,"sha256":"` + sha64 + `"}]}`},
	{method: "GET", route: "/api/hangar/v1/repos/{type}/{ns}/{name}/uploads/{id}", rule: ruleWrite, path: "/api/hangar/v1/repos/models/{repo}/uploads/{uuid}"},
	{method: "PUT", route: "/api/hangar/v1/repos/{type}/{ns}/{name}/uploads/{id}/parts/{n}", rule: ruleWrite, path: "/api/hangar/v1/repos/models/{repo}/uploads/{uuid}/parts/1", body: "x",
		header: map[string]string{"Content-Type": "application/octet-stream"}},
	{method: "POST", route: "/api/hangar/v1/repos/{type}/{ns}/{name}/uploads/{id}/complete", rule: ruleWrite, path: "/api/hangar/v1/repos/models/{repo}/uploads/{uuid}/complete", body: `{}`},
	{method: "POST", route: "/api/hangar/v1/repos/{type}/{ns}/{name}/zip", rule: ruleReadFiles, path: "/api/hangar/v1/repos/models/{repo}/zip"},
	{method: "GET", route: "/api/hangar/v1/repos/datasets/{ns}/{name}/configs", rule: ruleRead, path: "/api/hangar/v1/repos/datasets/{dataset}/configs"},
	{method: "GET", route: "/api/hangar/v1/repos/{type}/{ns}/{name}/mirror", rule: ruleRead, path: "/api/hangar/v1/repos/models/{repo}/mirror"},
	{method: "PATCH", route: "/api/hangar/v1/repos/{type}/{ns}/{name}/mirror", rule: ruleAdmin, path: "/api/hangar/v1/repos/models/{repo}/mirror", body: `{"track":false}`},
	{method: "POST", route: "/api/hangar/v1/repos/{type}/{ns}/{name}/mirror/sync", rule: rulePull, path: "/api/hangar/v1/repos/models/{repo}/mirror/sync"},
	{method: "POST", route: "/api/hangar/v1/repos/{type}/{ns}/{name}/mirror/detach", rule: ruleAdmin, path: "/api/hangar/v1/repos/models/{repo}/mirror/detach"},
	{method: "POST", route: "/api/hangar/v1/repos/{type}/{ns}/{name}/mirror/pull", rule: rulePull, path: "/api/hangar/v1/repos/models/{repo}/mirror/pull", body: `{"paths":["a.gguf"]}`},
	{method: "GET", route: "/api/hangar/v1/upstreams", rule: ruleSignedIn, path: "/api/hangar/v1/upstreams"},
	{method: "GET", route: "/api/hangar/v1/discover/{type}", rule: ruleSignedIn, path: "/api/hangar/v1/discover/models?upstream=nope&q=x"},
	{method: "GET", route: "/api/hangar/v1/discover/{type}/{ns}/{name}", rule: ruleSignedIn, path: "/api/hangar/v1/discover/models/a/b?upstream=nope"},
	{method: "POST", route: "/api/hangar/v1/pulls", rule: ruleSignedInWrite, path: "/api/hangar/v1/pulls", body: `{"upstream":"nope","repo":"ab/cd","type":"model"}`},
	{method: "GET", route: "/api/hangar/v1/pulls/{id}", rule: ruleSignedIn, path: "/api/hangar/v1/pulls/{uuid}"},
	{method: "GET", route: "/api/hangar/v1/jobs", rule: ruleSignedIn, path: "/api/hangar/v1/jobs"},
	{method: "GET", route: "/api/hangar/v1/jobs/{id}", rule: ruleReadFiles, path: "/api/hangar/v1/jobs/{job}"},
	{method: "POST", route: "/api/hangar/v1/jobs/{id}/cancel", rule: ruleWrite, path: "/api/hangar/v1/jobs/{job}/cancel"},
	{method: "POST", route: "/api/hangar/v1/jobs/{id}/retry", rule: ruleWrite, path: "/api/hangar/v1/jobs/{job}/retry"},
	{method: "GET", route: "/api/hangar/v1/namespaces/{ns}", rule: ruleProfile, path: "/api/hangar/v1/namespaces/{org}"},
	{method: "POST", route: "/api/hangar/v1/orgs", rule: ruleSignedInWrite, path: "/api/hangar/v1/orgs", body: `{"name":"probe-org-{n}"}`},
	{method: "PATCH", route: "/api/hangar/v1/orgs/{ns}", rule: ruleManageNamespace, path: "/api/hangar/v1/orgs/{org}", body: `{"display_name":"X"}`},
	{method: "DELETE", route: "/api/hangar/v1/orgs/{ns}", rule: ruleManageNamespace, path: "/api/hangar/v1/orgs/{org}"},
	{method: "GET", route: "/api/hangar/v1/orgs/{ns}/members", rule: ruleProfile, path: "/api/hangar/v1/orgs/{org}/members"},
	{method: "GET", route: "/api/hangar/v1/orgs/{ns}/members/{user}", rule: ruleProfile, path: "/api/hangar/v1/orgs/{org}/members/alice"},
	{method: "PUT", route: "/api/hangar/v1/orgs/{ns}/members/{user}", rule: ruleManageNamespace, path: "/api/hangar/v1/orgs/{org}/members/outsider", body: `{"role":"read"}`},
	{method: "DELETE", route: "/api/hangar/v1/orgs/{ns}/members/{user}", rule: ruleManageNamespace, path: "/api/hangar/v1/orgs/{org}/members/outsider"},
	{method: "GET", route: "/api/hangar/v1/collections", rule: ruleOpen, path: "/api/hangar/v1/collections"},
	{method: "POST", route: "/api/hangar/v1/collections", rule: ruleCreateRepo, path: "/api/hangar/v1/collections", body: `{"namespace":"alice","title":"Probe {n}","visibility":"private"}`},
	{method: "GET", route: "/api/hangar/v1/collections/{ns}/{slug}", rule: ruleRead, path: "/api/hangar/v1/collections/{collection}"},
	{method: "PATCH", route: "/api/hangar/v1/collections/{ns}/{slug}", rule: ruleWrite, path: "/api/hangar/v1/collections/{collection}", body: `{"title":"Renamed"}`},
	{method: "DELETE", route: "/api/hangar/v1/collections/{ns}/{slug}", rule: ruleWrite, path: "/api/hangar/v1/collections/{collection}"},
	{method: "POST", route: "/api/hangar/v1/collections/{ns}/{slug}/items", rule: ruleWrite, path: "/api/hangar/v1/collections/{collection}/items", body: `{"type":"model","item_id":"{repo}"}`},
	{method: "PATCH", route: "/api/hangar/v1/collections/{ns}/{slug}/items/{id}", rule: ruleWrite, path: "/api/hangar/v1/collections/{collection}/items/{uuid}", body: `{"note":"x"}`},
	{method: "DELETE", route: "/api/hangar/v1/collections/{ns}/{slug}/items/{id}", rule: ruleWrite, path: "/api/hangar/v1/collections/{collection}/items/{uuid}"},
	{method: "GET", route: "/api/hangar/v1/repos/{type}/{ns}/{name}/discussions", rule: ruleRead, path: "/api/hangar/v1/repos/models/{repo}/discussions"},
	{method: "POST", route: "/api/hangar/v1/repos/{type}/{ns}/{name}/discussions", rule: ruleDiscuss, path: "/api/hangar/v1/repos/models/{repo}/discussions", body: `{"title":"Probe"}`},
	{method: "GET", route: "/api/hangar/v1/repos/{type}/{ns}/{name}/discussions/{num}", rule: ruleRead, path: "/api/hangar/v1/repos/models/{repo}/discussions/{discussion}"},
	{method: "PATCH", route: "/api/hangar/v1/repos/{type}/{ns}/{name}/discussions/{num}", rule: ruleDiscuss, path: "/api/hangar/v1/repos/models/{repo}/discussions/{discussion}", body: `{"title":"New"}`},
	{method: "GET", route: "/api/hangar/v1/repos/{type}/{ns}/{name}/discussions/{num}/diff", rule: ruleReadFiles, path: "/api/hangar/v1/repos/models/{repo}/discussions/{discussion}/diff?path=README.md"},
	{method: "POST", route: "/api/hangar/v1/repos/{type}/{ns}/{name}/discussions/{num}/comments", rule: ruleDiscuss, path: "/api/hangar/v1/repos/models/{repo}/discussions/{discussion}/comments", body: `{"content":"Hi"}`},
	{method: "POST", route: "/api/hangar/v1/repos/{type}/{ns}/{name}/discussions/{num}/merge", rule: ruleWrite, path: "/api/hangar/v1/repos/models/{repo}/discussions/{discussion}/merge"},
	{method: "GET", route: "/api/hangar/v1/webhooks", rule: ruleSignedIn, path: "/api/hangar/v1/webhooks"},
	{method: "POST", route: "/api/hangar/v1/webhooks", rule: ruleAdmin, path: "/api/hangar/v1/webhooks",
		body: `{"repo":"model/{repo}","url":"https://hooks.example.com/{n}","events":["repo.created"],"secret":"a webhook secret"}`},
	{method: "GET", route: "/api/hangar/v1/webhooks/{id}", rule: ruleAdmin, path: "/api/hangar/v1/webhooks/{webhook}"},
	{method: "PATCH", route: "/api/hangar/v1/webhooks/{id}", rule: ruleAdmin, path: "/api/hangar/v1/webhooks/{webhook}", body: `{"enabled":false}`},
	{method: "DELETE", route: "/api/hangar/v1/webhooks/{id}", rule: ruleAdmin, path: "/api/hangar/v1/webhooks/{webhook}"},
	{method: "GET", route: "/api/hangar/v1/webhooks/{id}/deliveries", rule: ruleAdmin, path: "/api/hangar/v1/webhooks/{webhook}/deliveries"},
	{method: "POST", route: "/api/hangar/v1/webhooks/{id}/deliveries/{delivery_id}/redeliver", rule: ruleAdmin, path: "/api/hangar/v1/webhooks/{webhook}/deliveries/{uuid}/redeliver"},
	{method: "POST", route: "/api/hangar/v1/webhooks/{id}/test", rule: ruleAdmin, path: "/api/hangar/v1/webhooks/{webhook}/test"},
	{method: "GET", route: "/api/hangar/v1/admin/orgs", rule: ruleInstanceAdmin, path: "/api/hangar/v1/admin/orgs"},
	{method: "GET", route: "/api/hangar/v1/admin/users", rule: ruleInstanceAdmin, path: "/api/hangar/v1/admin/users"},
	{method: "POST", route: "/api/hangar/v1/admin/users", rule: ruleInstanceAdmin, path: "/api/hangar/v1/admin/users", body: `{"username":"made-{n}","email":"made{n}@example.com"}`},
	{method: "PATCH", route: "/api/hangar/v1/admin/users/{id}", rule: ruleInstanceAdmin, path: "/api/hangar/v1/admin/users/{uuid}", body: `{"disabled":false}`},
	{method: "POST", route: "/api/hangar/v1/admin/users/{id}/reset-link", rule: ruleInstanceAdmin, path: "/api/hangar/v1/admin/users/{uuid}/reset-link"},
	{method: "POST", route: "/api/hangar/v1/admin/users/{id}/email-link", rule: ruleInstanceAdmin, path: "/api/hangar/v1/admin/users/{uuid}/email-link", body: `{"email":"probe@example.com"}`},
	{method: "POST", route: "/api/hangar/v1/admin/invites", rule: ruleInstanceAdmin, path: "/api/hangar/v1/admin/invites", body: `{}`},
	{method: "GET", route: "/api/hangar/v1/admin/upstreams", rule: ruleInstanceAdmin, path: "/api/hangar/v1/admin/upstreams"},
	{method: "POST", route: "/api/hangar/v1/admin/upstreams", rule: ruleInstanceAdmin, path: "/api/hangar/v1/admin/upstreams", body: `{"name":"u{n}","url":"https://upstream{n}.example.com"}`},
	{method: "GET", route: "/api/hangar/v1/admin/upstreams/{id}", rule: ruleInstanceAdmin, path: "/api/hangar/v1/admin/upstreams/{uuid}"},
	{method: "PATCH", route: "/api/hangar/v1/admin/upstreams/{id}", rule: ruleInstanceAdmin, path: "/api/hangar/v1/admin/upstreams/{uuid}", body: `{"enabled":false}`},
	{method: "DELETE", route: "/api/hangar/v1/admin/upstreams/{id}", rule: ruleInstanceAdmin, path: "/api/hangar/v1/admin/upstreams/{uuid}"},
	{method: "POST", route: "/api/hangar/v1/admin/upstreams/{id}/test", rule: ruleInstanceAdmin, path: "/api/hangar/v1/admin/upstreams/{uuid}/test"},
	{method: "GET", route: "/api/hangar/v1/admin/settings", rule: ruleInstanceAdmin, path: "/api/hangar/v1/admin/settings"},
	{method: "PATCH", route: "/api/hangar/v1/admin/settings", rule: ruleInstanceAdmin, path: "/api/hangar/v1/admin/settings", body: `{}`},
	{method: "GET", route: "/api/hangar/v1/admin/storage", rule: ruleInstanceAdmin, path: "/api/hangar/v1/admin/storage"},
	{method: "PUT", route: "/api/hangar/v1/admin/quotas/{ns}", rule: ruleInstanceAdmin, path: "/api/hangar/v1/admin/quotas/alice", body: `{"bytes":null}`},
	{method: "POST", route: "/api/hangar/v1/admin/gc", rule: ruleInstanceAdmin, path: "/api/hangar/v1/admin/gc"},
	{method: "GET", route: "/api/hangar/v1/admin/audit", rule: ruleInstanceAdmin, path: "/api/hangar/v1/admin/audit"},
	{method: "GET", route: "/api/hangar/v1/admin/system", rule: ruleInstanceAdmin, path: "/api/hangar/v1/admin/system"},
	{method: "GET", route: "/api/hangar/v1/admin/tokens", rule: ruleInstanceAdmin, path: "/api/hangar/v1/admin/tokens"},
	{method: "DELETE", route: "/api/hangar/v1/admin/tokens/{id}", rule: ruleInstanceAdmin, path: "/api/hangar/v1/admin/tokens/{uuid}"},
}

// routeSiteRoutes acknowledges every route declaration that
// scanRouteSites finds outside the native API (whose routes chi.Walk
// lists), naming the routeMatrix entries it produces. A declaration that is
// not listed fails the test.
var routeSiteRoutes = map[string][]string{
	`cmd/hangar/router.go: httpx.SetRoute(r.Context(), rc.RoutePattern())`:        {"native"},
	`cmd/hangar/router.go: httpx.SetRoute(r.Context(), d.Route)`:                  {"GET /api/hangar/v1/pull-through/{repo}/{oid}"},
	`internal/api/repos.go: httpx.SetRoute(r.Context(), ExportPath+"{file}")`:     {"GET /api/hangar/v1/exports/{file}"},
	`internal/auth/handler.go: httpx.SetRoute(r.Context(), tmpl)`:                 {"helper"},
	`internal/auth/handler.go: s.route("/auth/invite/{code}", s.acceptInvite)`:    {"POST /auth/invite/{code}"},
	`internal/auth/handler.go: s.route("/auth/oidc/{provider}", s.oidcStart)`:     {"GET /auth/oidc/{provider}"},
	`internal/auth/handler.go: s.route("/auth/oidc/{provider}/link", s.oidcLink)`: {"POST /auth/oidc/{provider}/link"},
	`internal/auth/handler.go: s.route("/auth/oidc/{provider}/link", s.oidcUnlink)`: {
		"DELETE /auth/oidc/{provider}/link"},
	`internal/auth/handler.go: s.route("/auth/oidc/{provider}/callback", s.oidcCallback)`: {"GET /auth/oidc/{provider}/callback"},
	`internal/auth/handler.go: s.route("/auth/register", s.register)`:                     {"POST /auth/register"},
	`internal/auth/handler.go: s.route("/auth/reset/{code}", s.resetPassword)`:            {"POST /auth/reset/{code}"},
	`internal/auth/handler.go: s.route("/auth/email/{code}", s.confirmEmail)`:             {"POST /auth/email/{code}"},
	`internal/auth/handler.go: mux.HandleFunc("DELETE /auth/oidc/{provider}/link", s.route("/auth/oidc/{provider}/link", s.oidcUnlink))`: {
		"DELETE /auth/oidc/{provider}/link"},
	`internal/auth/handler.go: mux.HandleFunc("GET /auth/oidc/{provider}", s.route("/auth/oidc/{provider}", s.oidcStart))`: {"GET /auth/oidc/{provider}"},
	`internal/auth/handler.go: mux.HandleFunc("GET /auth/oidc/{provider}/callback", s.route("/auth/oidc/{provider}/callback", s.oidcCallback))`: {
		"GET /auth/oidc/{provider}/callback"},
	`internal/auth/handler.go: mux.HandleFunc("POST /auth/invite/{code}", s.route("/auth/invite/{code}", s.acceptInvite))`: {"POST /auth/invite/{code}"},
	`internal/auth/handler.go: mux.HandleFunc("POST /auth/oidc/{provider}/link", s.route("/auth/oidc/{provider}/link", s.oidcLink))`: {
		"POST /auth/oidc/{provider}/link"},
	`internal/auth/handler.go: mux.HandleFunc("POST /auth/register", s.route("/auth/register", s.register))`:              {"POST /auth/register"},
	`internal/auth/handler.go: mux.HandleFunc("POST /auth/reset/{code}", s.route("/auth/reset/{code}", s.resetPassword))`: {"POST /auth/reset/{code}"},
	`internal/auth/handler.go: mux.HandleFunc("POST /auth/email/{code}", s.route("/auth/email/{code}", s.confirmEmail))`:  {"POST /auth/email/{code}"},
	`internal/datasets/viewer/http.go: httpx.SetRoute(r.Context(), "/datasets-server"+endpoint)`:                          {"viewer"},
	`internal/datasets/viewer/http.go: httpx.SetRoute(r.Context(), "/datasets-server/{endpoint}")`:                        {"GET /datasets-server/{endpoint}"},
	`internal/datasets/viewer/http.go: httpx.SetRoute(r.Context(), "/datasets-server/assets/...")`:                        {"GET /datasets-server/assets/..."},
	`internal/gitserver/lfs.go: httpx.SetRoute(r.Context(), "/{lfs}")`:                                                    {"GET /{lfs}"},
	`internal/gitserver/lfs.go: httpx.SetRoute(r.Context(), base+"/locks")`:                                               {"GET /{ns}/{name}.git/info/lfs/locks"},
	`internal/gitserver/lfs.go: httpx.SetRoute(r.Context(), base+"/objects/batch")`:                                       {"POST /{ns}/{name}.git/info/lfs/objects/batch"},
	`internal/gitserver/lfs.go: httpx.SetRoute(r.Context(), base+"/objects/{oid}")`: {
		"GET /{ns}/{name}.git/info/lfs/objects/{oid}", "PUT /{ns}/{name}.git/info/lfs/objects/{oid}"},
	`internal/gitserver/lfs.go: httpx.SetRoute(r.Context(), base+"/uploads/{id}")`:        {"POST /{ns}/{name}.git/info/lfs/uploads/{id}"},
	`internal/gitserver/lfs.go: httpx.SetRoute(r.Context(), base+"/uploads/{id}/{part}")`: {"PUT /{ns}/{name}.git/info/lfs/uploads/{id}/{part}"},
	`internal/gitserver/lfs.go: httpx.SetRoute(r.Context(), base+"/verify")`:              {"POST /{ns}/{name}.git/info/lfs/verify"},
	`internal/gitserver/lfs.go: httpx.SetRoute(r.Context(), base+"/{path}")`:              {"GET /{ns}/{name}.git/info/lfs/{path}"},
	`internal/gitserver/smart.go: httpx.SetRoute(r.Context(), "/{git}")`:                  {"GET /{git}"},
	`internal/gitserver/smart.go: httpx.SetRoute(r.Context(), prefix+".git/{path}")`:      {"GET /{ns}/{name}.git/{path}"},
	`internal/gitserver/smart.go: httpx.SetRoute(r.Context(), prefix+"/"+rp.Rest[0])`: {
		"POST /{ns}/{name}/git-upload-pack", "POST /{ns}/{name}/git-receive-pack"},
	`internal/gitserver/smart.go: httpx.SetRoute(r.Context(), prefix+"/info/refs")`: {"GET /{ns}/{name}/info/refs", "GET /datasets/{ns}/{name}/info/refs"},
	`internal/hfapi/collections.go: h.route(w, r, "/api/collections", true, methods{http.MethodGet: h.listCollections, http.MethodPost: h.createCollection})`: {
		"GET /api/collections", "POST /api/collections"},
	`internal/hfapi/collections.go: h.route(w, r, "/api/collections/{ns}/{slug}", true, methods{ http.MethodGet: fn, http.MethodPatch: fn, http.MethodDelete: fn, })`: {
		"GET /api/collections/{ns}/{slug}", "PATCH /api/collections/{ns}/{slug}", "DELETE /api/collections/{ns}/{slug}"},
	`internal/hfapi/collections.go: h.route(w, r, "/api/collections/{ns}/{slug}/items", true, methods{http.MethodPost: fn})`: {
		"POST /api/collections/{ns}/{slug}/items"},
	`internal/hfapi/collections.go: h.route(w, r, "/api/collections/{ns}/{slug}/items/{id}", true, methods{ http.MethodPatch: fn, http.MethodDelete: fn, })`: {
		"PATCH /api/collections/{ns}/{slug}/items/{id}", "DELETE /api/collections/{ns}/{slug}/items/{id}"},
	`internal/hfapi/discussions.go: h.route(w, r, prefix+"/{num}", true, methods{http.MethodGet: fn})`: {
		"GET /api/models/{ns}/{name}/discussions/{num}"},
	`internal/hfapi/discussions.go: h.route(w, r, prefix+"/{num}/comment", true, methods{http.MethodPost: fn})`: {
		"POST /api/models/{ns}/{name}/discussions/{num}/comment"},
	`internal/hfapi/discussions.go: h.route(w, r, prefix+"/{num}/comment/{id}/"+change, true, methods{http.MethodPost: fn, })`: {
		"POST /api/models/{ns}/{name}/discussions/{num}/comment/{id}/edit", "POST /api/models/{ns}/{name}/discussions/{num}/comment/{id}/hide"},
	`internal/hfapi/discussions.go: h.route(w, r, prefix+"/{num}/merge", true, methods{http.MethodPost: fn})`: {
		"POST /api/models/{ns}/{name}/discussions/{num}/merge"},
	`internal/hfapi/discussions.go: h.route(w, r, prefix+"/{num}/status", true, methods{http.MethodPost: fn})`: {
		"POST /api/models/{ns}/{name}/discussions/{num}/status"},
	`internal/hfapi/discussions.go: h.route(w, r, prefix+"/{num}/title", true, methods{http.MethodPost: fn})`: {
		"POST /api/models/{ns}/{name}/discussions/{num}/title"},
	`internal/hfapi/discussions.go: h.route(w, r, prefix, true, methods{ http.MethodGet: fn, http.MethodPost: fn, })`: {
		"GET /api/models/{ns}/{name}/discussions", "POST /api/models/{ns}/{name}/discussions"},
	`internal/hfapi/hfapi.go: h.route(w, r, "/api/"+t.Plural(), true, map[string]http.HandlerFunc{http.MethodGet: fn})`: {
		"GET /api/models", "GET /api/datasets"},
	`internal/hfapi/hfapi.go: h.route(w, r, "/api/users/{user}/likes", p.len() == 3 && p.at(2) == "likes", map[string]http.HandlerFunc{ http.MethodGet: fn})`: {
		"GET /api/users/{user}/likes"},
	`internal/hfapi/hfapi.go: h.route(w, r, "/api/validate-yaml", p.len() == 1, map[string]http.HandlerFunc{http.MethodPost: h.validateYAML})`: {
		"POST /api/validate-yaml"},
	`internal/hfapi/hfapi.go: h.route(w, r, "/api/whoami-v2", p.len() == 1, map[string]http.HandlerFunc{http.MethodGet: h.whoami})`: {"GET /api/whoami-v2"},
	`internal/hfapi/hfapi.go: httpx.SetRoute(r.Context(), "/{hub-unknown}")`:                                                        {"GET /{hub-unknown}"},
	`internal/hfapi/hfapi.go: httpx.SetRoute(r.Context(), template)`:                                                                {"helper"},
	`internal/hfapi/identity.go: h.route(w, r, "/oauth/device", true, methods{http.MethodPost: h.deviceStart})`:                     {"POST /oauth/device"},
	`internal/hfapi/identity.go: h.route(w, r, "/oauth/token", true, methods{http.MethodPost: h.deviceToken})`:                      {"POST /oauth/token"},
	`internal/hfapi/manage.go: h.route(w, r, "/api/repos/create", true, methods{http.MethodPost: h.createRepo})`:                    {"POST /api/repos/create"},
	`internal/hfapi/manage.go: h.route(w, r, "/api/repos/delete", true, methods{http.MethodDelete: h.deleteRepo})`:                  {"DELETE /api/repos/delete"},
	`internal/hfapi/manage.go: h.route(w, r, "/api/repos/move", true, methods{http.MethodPost: h.moveRepo})`:                        {"POST /api/repos/move"},
	`internal/hfapi/resolve.go: h.route(w, r, template, rev != "", methods{http.MethodGet: fn})`: {
		"GET /{ns}/{name}/resolve/{rev}/{path}", "GET /datasets/{ns}/{name}/resolve/{rev}/{path}"},
	`internal/hfapi/resolve.go: httpx.SetRoute(r.Context(), "/"+t.URLPrefix()+"{name}/resolve/{rev}/{path}")`: {"GET /{name}/resolve/{rev}/{path}"},
	`internal/hfapi/routes.go: h.route(w, r, prefix+"/branch/{branch}", branch != "", methods{ http.MethodPost: fn, http.MethodDelete: fn, })`: {
		"POST /api/models/{ns}/{name}/branch/{branch}", "DELETE /api/models/{ns}/{name}/branch/{branch}"},
	`internal/hfapi/routes.go: h.route(w, r, prefix+"/commit/{rev}", rev != "" && tail.len() == 0, methods{http.MethodPost: fn})`: {
		"POST /api/models/{ns}/{name}/commit/{rev}"},
	`internal/hfapi/routes.go: h.route(w, r, prefix+"/commits/{rev}", tail.len() == 0, methods{http.MethodGet: fn})`: {
		"GET /api/models/{ns}/{name}/commits/{rev}"},
	`internal/hfapi/routes.go: h.route(w, r, prefix+"/croissant", t == model.RepoTypeDataset && args.len() == 0, methods{http.MethodGet: fn})`: {
		"GET /api/datasets/{ns}/{name}/croissant"},
	`internal/hfapi/routes.go: h.route(w, r, prefix+"/like", args.len() == 0, methods{ http.MethodPost: fn, http.MethodDelete: fn, })`: {
		"POST /api/models/{ns}/{name}/like", "DELETE /api/models/{ns}/{name}/like"},
	`internal/hfapi/routes.go: h.route(w, r, prefix+"/likers", args.len() == 0, methods{http.MethodGet: fn})`: {"GET /api/models/{ns}/{name}/likers"},
	`internal/hfapi/routes.go: h.route(w, r, prefix+"/parquet/{config}/{split}/{n}", args.len() <= 3, methods{http.MethodGet: fn})`: {
		"GET /api/datasets/{ns}/{name}/parquet/{config}/{split}/{n}"},
	`internal/hfapi/routes.go: h.route(w, r, prefix+"/paths-info/{rev}", rev != "" && tail.len() == 0, methods{http.MethodPost: fn})`: {
		"POST /api/models/{ns}/{name}/paths-info/{rev}"},
	`internal/hfapi/routes.go: h.route(w, r, prefix+"/preupload/{rev}", rev != "" && tail.len() == 0, methods{http.MethodPost: fn})`: {
		"POST /api/models/{ns}/{name}/preupload/{rev}"},
	`internal/hfapi/routes.go: h.route(w, r, prefix+"/refs", args.len() == 0, methods{http.MethodGet: fn})`: {"GET /api/models/{ns}/{name}/refs"},
	`internal/hfapi/routes.go: h.route(w, r, prefix+"/revision/{rev}", rev != "" && tail.len() == 0, methods{http.MethodGet: fn})`: {
		"GET /api/models/{ns}/{name}/revision/{rev}"},
	`internal/hfapi/routes.go: h.route(w, r, prefix+"/settings", args.len() == 0, methods{http.MethodPut: fn})`: {"PUT /api/models/{ns}/{name}/settings"},
	`internal/hfapi/routes.go: h.route(w, r, prefix+"/tag/{rev}", name != "", methods{ http.MethodPost: fn, http.MethodDelete: fn, })`: {
		"POST /api/models/{ns}/{name}/tag/{rev}", "DELETE /api/models/{ns}/{name}/tag/{rev}"},
	`internal/hfapi/routes.go: h.route(w, r, prefix+"/tree/{rev}/{path}", rev != "", methods{http.MethodGet: fn})`: {
		"GET /api/models/{ns}/{name}/tree/{rev}/{path}"},
	`internal/hfapi/routes.go: h.route(w, r, prefix+"/xet-write-token/{rev}", rev != "" && tail.len() == 0, methods{http.MethodGet: fn})`: {
		"GET /api/models/{ns}/{name}/xet-write-token/{rev}"},
	`internal/hfapi/routes.go: h.route(w, r, prefix, true, methods{http.MethodGet: fn})`:               {"GET /api/models/{ns}/{name}"},
	`internal/hfapi/routes.go: httpx.SetRoute(r.Context(), "/api/"+t.Plural()+"/{name}")`:              {"GET /api/models/{name}"},
	`internal/httpx/health.go: SetRoute(r.Context(), r.URL.Path)`:                                      {"GET /healthz", "GET /readyz"},
	`internal/ollama/registry.go: httpx.SetRoute(r.Context(), "/v2/")`:                                 {"GET /v2/"},
	`internal/ollama/registry.go: httpx.SetRoute(r.Context(), "/v2/token")`:                            {"GET /v2/token"},
	`internal/ollama/registry.go: httpx.SetRoute(r.Context(), "/v2/{ns}/{name}/"+segs[2])`:             {"PUT /v2/{ns}/{name}/manifests", "POST /v2/{ns}/{name}/blobs"},
	`internal/ollama/registry.go: httpx.SetRoute(r.Context(), "/v2/{ns}/{name}/blobs/{digest}")`:       {"GET /v2/{ns}/{name}/blobs/{digest}"},
	`internal/ollama/registry.go: httpx.SetRoute(r.Context(), "/v2/{ns}/{name}/manifests/{tag}")`:      {"GET /v2/{ns}/{name}/manifests/{tag}"},
	`internal/web/web.go: httpx.SetRoute(r.Context(), "/assets/{file}")`:                               {"GET /assets/{file}"},
	`internal/web/web.go: httpx.SetRoute(r.Context(), "/{ui}")`:                                        {"GET /{ui}"},
	`internal/xet/cas.go: h.route(w, r, "/v1/chunks/{prefix}/{hash}", http.MethodGet, false, h.chunk)`: {"GET /xet-cas/v1/chunks/{prefix}/{hash}"},
	`internal/xet/cas.go: h.route(w, r, "/v1/shards", http.MethodPost, true, h.shardV1)`:               {"POST /xet-cas/v1/shards"},
	`internal/xet/cas.go: h.route(w, r, "/v1/xorbs/{prefix}/{hash}", http.MethodPost, true, fn, )`:     {"POST /xet-cas/v1/xorbs/{prefix}/{hash}"},
	`internal/xet/cas.go: h.route(w, r, "/v2/shards", http.MethodPost, true, h.shardV2)`:               {"POST /xet-cas/v2/shards"},
	`internal/xet/cas.go: httpx.SetRoute(r.Context(), "/v1/telemetry")`:                                {"POST /v1/telemetry"},
	`internal/xet/cas.go: httpx.SetRoute(r.Context(), h.prefix+route)`:                                 {"helper"},
}

// probeServer is a full Instance behind the production handler, with a
// hook that records the route template each request matched.
type probeServer struct {
	t      *testing.T
	in     *instance
	url    string
	tokens map[string]string
	routes *routeLog
	mu     sync.Mutex // guards n
	n      int
}

// routeWait is how long a probe waits for the access log line of its
// request once the response has arrived. The line is written after the
// handler returns, which can be after the client has read the response.
const routeWait = 30 * time.Second

// routeLog keeps the route template the access log recorded for each
// request ID, and lets a probe wait for it.
//
// httpx.AccessLog logs after the handler returns, but a response larger
// than the server's write buffer, or one the handler flushes (git
// info/refs, file downloads), reaches the client before that. Reading
// the route as soon as the response arrives then races the log line.
type routeLog struct {
	mu     sync.Mutex
	routes map[string]string
	ready  map[string]chan struct{}
}

func newRouteLog() *routeLog {
	return &routeLog{routes: map[string]string{}, ready: map[string]chan struct{}{}}
}

// signal returns the channel closed when the route for id is recorded.
// The caller holds l.mu.
func (l *routeLog) signal(id string) chan struct{} {
	ch, ok := l.ready[id]
	if !ok {
		ch = make(chan struct{})
		l.ready[id] = ch
	}
	return ch
}

func (l *routeLog) record(id, route string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, done := l.routes[id]; done {
		return
	}
	l.routes[id] = route
	close(l.signal(id))
}

// wait returns the route recorded for id, waiting up to timeout for it.
// It returns "" and false if no route was recorded in time.
func (l *routeLog) wait(id string, timeout time.Duration) (string, bool) {
	l.mu.Lock()
	ch := l.signal(id)
	l.mu.Unlock()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ch:
	case <-timer.C:
		return "", false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.routes[id], true
}

func newProbeServer(t *testing.T) *probeServer {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	pub := "http://" + srv.Listener.Addr().String()
	in := newTestInstance(t, func(c *config.Config) {
		c.Server.PublicURL = pub
		c.HFAPI.DefaultNamespace = "alice"
	})
	ctx := context.Background()
	routes := newRouteLog()
	// The access log records each request's route template; keep it by
	// request ID.
	in.log = slog.New(routeRecorder{record: routes.record})
	h, err := newHandler(ctx, in, newHealth(in.db, in.blobs), false)
	require.NoError(t, err)
	ps := &probeServer{t: t, in: in, url: pub, tokens: map[string]string{}, routes: routes}
	srv.Config.Handler = h
	srv.Start()
	t.Cleanup(srv.Close)

	alice, outsider := testUser(t, in, "alice"), testUser(t, in, "outsider")
	root, err := in.auth.Users.Create(ctx, auth.NewUser{Username: "root", IsAdmin: true, Password: "a long enough password 1"}, model.System())
	require.NoError(t, err)
	mint := func(p model.Principal, scope model.Scope) string {
		_, secret, err := in.auth.TokenStore.Create(ctx, auth.NewToken{UserID: p.UserID, Name: string(scope), Scope: scope})
		require.NoError(t, err)
		return secret
	}
	ps.tokens["outsider"] = mint(outsider, model.ScopeWrite)
	ps.tokens["alice-read"] = mint(alice, model.ScopeRead)
	ps.tokens["alice-write"] = mint(alice, model.ScopeWrite)
	ps.tokens["alice-admin"] = mint(alice, model.ScopeAdmin)
	ps.tokens["root"] = mint(root.Principal(model.AuthSession), model.ScopeAdmin)
	return ps
}

type probeResp struct {
	status int
	route  string
	body   string
}

func (ps *probeServer) do(persona, method, path, body string, header map[string]string) probeResp {
	ps.t.Helper()
	req, err := http.NewRequest(method, ps.url+path, strings.NewReader(body))
	require.NoError(ps.t, err)
	ps.mu.Lock()
	ps.n++
	id := "probe-" + strconv.Itoa(ps.n)
	ps.mu.Unlock()
	req.Header.Set(httpx.HeaderRequestID, id)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	if tok := ps.tokens[persona]; tok != "" {
		if strings.Contains(path, ".git/") || strings.HasSuffix(strings.SplitN(path, "?", 2)[0], ".git") {
			req.SetBasicAuth(persona, tok)
		} else {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	start := time.Now()
	resp, err := client.Do(req)
	require.NoError(ps.t, err)
	if d := time.Since(start); d > 2*time.Second {
		ps.t.Logf("slow probe: %s %s as %s took %s", method, path, persona, d)
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	// Close before waiting: a handler still writing a large body only
	// returns, and logs its route, once the client stops reading.
	require.NoError(ps.t, resp.Body.Close())
	route, ok := ps.routes.wait(id, routeWait)
	if !ok {
		ps.t.Logf("no access log line for %s %s as %s within %s", method, path, persona, routeWait)
	}
	return probeResp{status: resp.StatusCode, route: route, body: string(raw)}
}

// fixtures creates what a case's placeholders name and returns the
// replacements.
func (ps *probeServer) fixtures(i int, c routeCase) map[string]string {
	t := ps.t
	t.Helper()
	ctx := context.Background()
	alice, err := ps.in.auth.Users.GetByUsername(ctx, "alice")
	require.NoError(t, err)
	ap := alice.Principal(model.AuthSession)
	n := strconv.Itoa(i)
	rep := map[string]string{"{n}": n, "{uuid}": "0b6e6400-0000-4000-8000-0000000000" + fmt.Sprintf("%02d", i%100)}
	all := c.path + c.body
	need := func(p string) bool { return strings.Contains(all, p) }
	if need("{repo}") || need("{name}") || need("{sha}") || need("{webhook}") || need("{job}") || need("{discussion}") || need("{comment}") || need("{collection}") {
		name := "r" + n
		id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: name}
		_, err := ps.in.repos.Create(ctx, repo.CreateParams{ID: id, Actor: ap, Visibility: model.VisibilityPrivate, InitialReadme: true})
		require.NoError(t, err)
		sha, err := ps.in.repos.Resolve(ctx, id, "main")
		require.NoError(t, err)
		rep["{repo}"], rep["{name}"], rep["{sha}"] = "alice/"+name, name, string(sha)
	}
	if need("{dataset}") {
		name := "d" + n
		id := model.RepoID{Type: model.RepoTypeDataset, Namespace: "alice", Name: name}
		_, err := ps.in.repos.Create(ctx, repo.CreateParams{ID: id, Actor: ap, Visibility: model.VisibilityPrivate, InitialReadme: true})
		require.NoError(t, err)
		rep["{dataset}"] = "alice/" + name
	}
	if need("{org}") {
		name := "lab" + n
		_, err := ps.in.orgs.CreateOrg(ctx, name, "Lab", ap)
		require.NoError(t, err)
		rep["{org}"] = name
	}
	create := func(method, path, body string) map[string]any {
		t.Helper()
		r := ps.do("alice-admin", method, path, body, nil)
		require.Less(t, r.status, 300, "fixture %s %s: %s", method, path, r.body)
		var out map[string]any
		require.NoError(t, json.Unmarshal([]byte(r.body), &out), r.body)
		return out
	}
	if need("{webhook}") {
		out := create(http.MethodPost, "/api/hangar/v1/webhooks",
			`{"repo":"model/`+rep["{repo}"]+`","url":"https://hooks.example.com/f`+n+`","events":["repo.created"],"secret":"a webhook secret"}`)
		rep["{webhook}"] = fmt.Sprint(out["id"])
	}
	if need("{job}") {
		out := create(http.MethodPost, "/api/hangar/v1/repos/models/"+rep["{repo}"]+"/zip", "")
		rep["{job}"] = fmt.Sprint(out["id"])
	}
	if need("{discussion}") || need("{comment}") {
		out := create(http.MethodPost, "/api/models/"+rep["{repo}"]+"/discussions", `{"title":"Fixture","description":"First"}`)
		rep["{discussion}"] = fmt.Sprint(out["num"])
		d := create(http.MethodGet, "/api/models/"+rep["{repo}"]+"/discussions/"+rep["{discussion}"], "")
		events, _ := d["events"].([]any)
		require.NotEmpty(t, events, "fixture discussion has no events")
		first, _ := events[0].(map[string]any)
		rep["{comment}"] = fmt.Sprint(first["id"])
	}
	if need("{collection}") {
		out := create(http.MethodPost, "/api/hangar/v1/collections", `{"namespace":"alice","title":"Fixture `+n+`","visibility":"private"}`)
		rep["{collection}"] = "alice/" + fmt.Sprint(out["slug"])
	}
	return rep
}

func expand(s string, rep map[string]string) string {
	keys := make([]string, 0, len(rep))
	for k := range rep {
		keys = append(keys, k)
	}
	// Longer placeholders first, so {repo} does not eat {repo}-moved's prefix wrongly.
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, k := range keys {
		s = strings.ReplaceAll(s, k, rep[k])
	}
	return s
}

// routeRecorder is a slog handler that passes the route of every access
// log record to record, with the request ID from the record's context.
type routeRecorder struct{ record func(id, route string) }

func (routeRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (h routeRecorder) Handle(ctx context.Context, r slog.Record) error {
	if r.Message != "request" {
		return nil
	}
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "route" {
			h.record(httpx.RequestID(ctx), a.Value.String())
			return false
		}
		return true
	})
	return nil
}
func (h routeRecorder) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h routeRecorder) WithGroup(string) slog.Handler      { return h }

// nativeRoutes walks the generated native API router, registered as
// protocolHandler registers it.
func nativeRoutes(t *testing.T, in *instance) []string {
	t.Helper()
	srv, err := newNativeServer(in)
	require.NoError(t, err)
	h := gen.HandlerWithOptions(srv, gen.ChiServerOptions{BaseURL: "/api/hangar/v1"})
	routes, ok := h.(chi.Routes)
	require.True(t, ok, "the native API router is a chi router")
	var out []string
	require.NoError(t, chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		out = append(out, method+" "+route)
		return nil
	}))
	return out
}

// TestRouteAuthorisationMatrix enumerates every route Hangar serves (the
// native API router, and every route declaration in the other routers)
// and checks each against routeMatrix, which records its authorisation
// rule from docs/spec/05-auth.md. A route with no entry fails, so every
// new route needs an authorisation decision. Each entry is then probed on
// the full server as six callers: the ones its rule allows must not be
// refused, and the others must get 401, 403 or 404.
func TestRouteAuthorisationMatrix(t *testing.T) {
	ps := newProbeServer(t)
	cases := map[string]routeCase{}
	for _, c := range routeMatrix {
		_, dup := cases[c.key()]
		require.False(t, dup, "duplicate entry %s", c.key())
		cases[c.key()] = c
	}

	// Completeness: every registered route has a decision. Report every
	// gap at once.
	var gaps []string
	gap := func(format string, args ...any) { gaps = append(gaps, fmt.Sprintf(format, args...)) }
	covered := map[string]bool{}
	for _, r := range nativeRoutes(t, ps.in) {
		if _, ok := cases[r]; !ok {
			gap("native API route %s has no entry in routeMatrix: decide its authorisation rule", r)
		}
		covered[r] = true
	}
	seen := map[string]bool{}
	for _, s := range scanRouteSites(t) {
		seen[s] = true
		keys, ok := routeSiteRoutes[s]
		if !ok {
			gap("route declaration %q is not in routeSiteRoutes: add it, and give each route it serves an entry in routeMatrix", s)
			continue
		}
		for _, k := range keys {
			switch k {
			case "native", "helper":
				continue
			case "viewer":
				for _, e := range viewerEndpoints(t) {
					k := "GET /datasets-server" + e
					if _, ok := cases[k]; !ok {
						gap("Viewer endpoint %s has no entry in routeMatrix", k)
					}
					covered[k] = true
				}
				continue
			}
			if _, ok := cases[k]; !ok {
				gap("%s lists %s, which has no entry in routeMatrix", s, k)
			}
			covered[k] = true
		}
	}
	for s := range routeSiteRoutes {
		if !seen[s] {
			gap("routeSiteRoutes lists %q, which the code no longer declares", s)
		}
	}
	for _, d := range directRoutes(ps.in) {
		if !covered["GET "+d.Route] {
			gap("direct route %s has no entry in routeMatrix", d.Route)
		}
	}
	for k := range cases {
		if !covered[k] {
			gap("routeMatrix entry %s matches no registered route", k)
		}
	}
	sort.Strings(gaps)
	require.Empty(t, gaps, "routes without an authorisation decision:\n%s", strings.Join(gaps, "\n"))

	// Behaviour: probe each route as every persona.
	for i, c := range routeMatrix {
		rep := ps.fixtures(i+1, c)
		path, body := expand(c.path, rep), expand(c.body, rep)
		allowed := map[string]bool{}
		for _, p := range allowedBy[c.rule] {
			allowed[p] = true
		}
		// Refused callers first, so an allowed change cannot hide them.
		order := append([]string{}, personas...)
		sort.SliceStable(order, func(a, b int) bool { return !allowed[order[a]] && allowed[order[b]] })
		for _, persona := range order {
			r := ps.do(persona, c.method, path, body, c.header)
			if r.route != c.route {
				t.Errorf("%s as %s: %s %s reached route %q (%d: %s)", c.key(), persona, c.method, path, r.route, r.status, r.body)
				continue
			}
			in := func(codes ...int) bool {
				for _, code := range codes {
					if r.status == code {
						return true
					}
				}
				return false
			}
			switch {
			case c.rule == ruleNotFound:
				if !in(http.StatusNotFound, http.StatusMethodNotAllowed) {
					t.Errorf("%s as %s: want 404 or 405, got %d: %s", c.key(), persona, r.status, r.body)
				}
			case c.rule == ruleCredentialExchange:
			case c.rule == ruleOwnCredential:
				if !in(http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound) {
					t.Errorf("%s as %s must need its own credential, got %d: %s", c.key(), persona, r.status, r.body)
				}
			case allowed[persona]:
				if in(http.StatusUnauthorized, http.StatusForbidden) {
					t.Errorf("%s (%s) refused %s with %d: %s", c.key(), c.rule, persona, r.status, r.body)
				}
			default:
				if !in(http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound) {
					t.Errorf("%s (%s) allowed %s with %d: %s", c.key(), c.rule, persona, r.status, r.body)
				}
			}
		}
	}
	t.Logf("%d routes probed as %d callers", len(routeMatrix), len(personas))

	// ruleProfile routes open to anonymous callers once an admin turns
	// security.anonymous_read on, with no restart.
	on := ps.do("root", http.MethodPatch, "/api/hangar/v1/admin/settings", `{"anonymous_read":true}`, nil)
	require.Equal(t, http.StatusOK, on.status, on.body)
	profiles := 0
	for i, c := range routeMatrix {
		if c.rule != ruleProfile {
			continue
		}
		profiles++
		rep := ps.fixtures(len(routeMatrix)+i+1, c)
		r := ps.do("anonymous", c.method, expand(c.path, rep), expand(c.body, rep), c.header)
		if r.route != c.route || r.status == http.StatusUnauthorized || r.status == http.StatusForbidden {
			t.Errorf("%s with anonymous_read on: anonymous got %d at route %q: %s", c.key(), r.status, r.route, r.body)
		}
	}
	require.NotZero(t, profiles, "the matrix has profile routes")
}

// viewerEndpoints returns the Dataset Viewer endpoints the Viewer serves,
// from the `known` table in internal/datasets/viewer/http.go, plus its
// health check.
func viewerEndpoints(t *testing.T) []string {
	t.Helper()
	return append([]string{"/healthcheck"}, scanMapKeys(t, "internal/datasets/viewer/http.go", "known")...)
}

// TestProbeWaitsForLateAccessLog covers a flake in
// TestRouteAuthorisationMatrix: httpx.AccessLog writes its line after the
// handler returns, so for a response flushed before then (git info/refs,
// file downloads, anything over the write buffer) the probe used to read
// the route before it was recorded and report `reached route ""`. The
// handler here flushes more than the probe reads and, like a handler
// streaming a body, returns and logs only once the client hangs up.
func TestProbeWaitsForLateAccessLog(t *testing.T) {
	const route = "/{ns}/{name}.git/info/refs"
	routes := newRouteLog()
	log := slog.New(routeRecorder{record: routes.record})
	srv := httptest.NewServer(httpx.RequestIDs(httpx.AccessLog(log)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.SetRoute(r.Context(), route)
		_, _ = w.Write([]byte(strings.Repeat("x", 8192)))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))))
	t.Cleanup(srv.Close)

	ps := &probeServer{t: t, url: srv.URL, tokens: map[string]string{}, routes: routes}
	r := ps.do("anonymous", http.MethodGet, "/alice/r1.git/info/refs", "", nil)
	require.Equal(t, http.StatusOK, r.status)
	require.Equal(t, route, r.route)
}

func TestRouteLogWait(t *testing.T) {
	l := newRouteLog()
	_, ok := l.wait("missing", 10*time.Millisecond)
	require.False(t, ok, "wait must time out when no route is recorded")

	l.record("early", "/a")
	route, ok := l.wait("early", time.Second)
	require.True(t, ok)
	require.Equal(t, "/a", route)

	done := make(chan string, 1)
	go func() {
		r, _ := l.wait("late", 10*time.Second)
		done <- r
	}()
	l.record("late", "/b")
	l.record("late", "/c") // A second line for the same ID is ignored.
	require.Equal(t, "/b", <-done)
}
