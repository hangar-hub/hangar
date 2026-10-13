-- Hangar initial schema (Postgres). Keep in step with ../sqlite/0001_initial.up.sql;
-- internal/db tests compare the two schemas column by column.

-- Namespaces, users and access ---------------------------------------------

CREATE TABLE namespaces (
    id uuid PRIMARY KEY,
    name text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('user', 'org')),
    display_name text NOT NULL DEFAULT '',
    description text NOT NULL DEFAULT '',
    avatar_key text,
    reserved_mirror boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);
--bun:split
CREATE UNIQUE INDEX namespaces_name_key ON namespaces (lower(name));
--bun:split

CREATE TABLE users (
    id uuid PRIMARY KEY,
    namespace_id uuid NOT NULL UNIQUE REFERENCES namespaces (id) ON DELETE CASCADE,
    email text,
    email_verified boolean NOT NULL DEFAULT false,
    password_hash text,
    is_admin boolean NOT NULL DEFAULT false,
    is_system boolean NOT NULL DEFAULT false,
    disabled_at timestamptz,
    last_login_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);
--bun:split
CREATE UNIQUE INDEX users_email_key ON users (lower(email)) WHERE email IS NOT NULL AND email <> '';
--bun:split

CREATE TABLE identities (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    provider text NOT NULL,
    subject text NOT NULL,
    email text,
    claims jsonb NOT NULL DEFAULT '{}',
    last_login_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (provider, subject)
);
--bun:split
CREATE INDEX identities_user_id_idx ON identities (user_id);
--bun:split

CREATE TABLE sessions (
    id uuid PRIMARY KEY,
    id_hash text NOT NULL UNIQUE,
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    csrf_secret text NOT NULL,
    expires_at timestamptz NOT NULL,
    last_seen_at timestamptz,
    user_agent text NOT NULL DEFAULT '',
    ip text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);
--bun:split
CREATE INDEX sessions_user_id_idx ON sessions (user_id);
--bun:split
CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);
--bun:split

CREATE TABLE tokens (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name text NOT NULL,
    hash text NOT NULL UNIQUE,
    prefix text NOT NULL,
    scope text NOT NULL CHECK (scope IN ('read', 'write', 'admin')),
    restrictions jsonb NOT NULL DEFAULT '{}',
    expires_at timestamptz,
    last_used_at timestamptz,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);
--bun:split
CREATE INDEX tokens_user_id_idx ON tokens (user_id);
--bun:split

CREATE TABLE ollama_keys (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name text NOT NULL DEFAULT '',
    public_key text NOT NULL,
    fingerprint text NOT NULL UNIQUE,
    last_used_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);
--bun:split
CREATE INDEX ollama_keys_user_id_idx ON ollama_keys (user_id);
--bun:split

-- Replay protection for Ollama registry token requests (08-local-tools.md).
CREATE TABLE ollama_nonces (
    nonce text PRIMARY KEY,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);
--bun:split

-- Generated Ollama config and template blobs (08-local-tools.md).
CREATE TABLE ollama_blobs (
    digest text PRIMARY KEY,
    media_type text NOT NULL,
    size bigint NOT NULL,
    data bytea NOT NULL,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);
--bun:split

CREATE TABLE memberships (
    id uuid PRIMARY KEY,
    org_namespace_id uuid NOT NULL REFERENCES namespaces (id) ON DELETE CASCADE,
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role text NOT NULL CHECK (role IN ('read', 'write', 'admin')),
    source text NOT NULL DEFAULT 'manual' CHECK (source IN ('manual', 'oidc')),
    source_provider text,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (org_namespace_id, user_id)
);
--bun:split
CREATE INDEX memberships_user_id_idx ON memberships (user_id);
--bun:split

-- Single-use invite and password reset links (05-auth.md).
CREATE TABLE invites (
    id uuid PRIMARY KEY,
    code_hash text NOT NULL UNIQUE,
    created_by uuid REFERENCES users (id) ON DELETE SET NULL,
    email text NOT NULL DEFAULT '',
    org_namespace_id uuid REFERENCES namespaces (id) ON DELETE CASCADE,
    role text,
    make_admin boolean NOT NULL DEFAULT false,
    expires_at timestamptz NOT NULL,
    used_at timestamptz,
    used_by uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);
--bun:split

CREATE TABLE password_resets (
    id uuid PRIMARY KEY,
    code_hash text NOT NULL UNIQUE,
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_by uuid REFERENCES users (id) ON DELETE SET NULL,
    expires_at timestamptz NOT NULL,
    used_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);
--bun:split

-- Login throttling per username and per IP, shared across replicas (05-auth.md).
CREATE TABLE login_attempts (
    key text PRIMARY KEY,
    failures integer NOT NULL DEFAULT 0,
    last_failure_at timestamptz,
    locked_until timestamptz,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);
--bun:split

-- OIDC authorisation requests in flight (state, nonce, PKCE), so any replica
-- can handle the callback.
CREATE TABLE oidc_states (
    state_hash text PRIMARY KEY,
    provider text NOT NULL,
    nonce text NOT NULL,
    pkce_verifier text NOT NULL,
    redirect_to text NOT NULL DEFAULT '',
    link_user_id uuid REFERENCES users (id) ON DELETE CASCADE,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);
--bun:split

-- Device-code OAuth for interactive `hf auth login` (03-hf-api.md).
CREATE TABLE oauth_device_codes (
    device_code_hash text PRIMARY KEY,
    user_code text NOT NULL UNIQUE,
    client_id text NOT NULL DEFAULT '',
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'denied', 'used')),
    user_id uuid REFERENCES users (id) ON DELETE CASCADE,
    token_id uuid REFERENCES tokens (id) ON DELETE SET NULL,
    poll_interval integer NOT NULL DEFAULT 5,
    last_polled_at timestamptz,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);
--bun:split

-- Repos and git ---------------------------------------------------------------

CREATE TABLE repos (
    id uuid PRIMARY KEY,
    namespace_id uuid NOT NULL REFERENCES namespaces (id),
    name text NOT NULL,
    type text NOT NULL CHECK (type IN ('model', 'dataset')),
    visibility text NOT NULL CHECK (visibility IN ('public', 'internal', 'private')),
    default_branch text NOT NULL DEFAULT 'main',
    head_ref text NOT NULL DEFAULT 'refs/heads/main',
    description text NOT NULL DEFAULT '',
    tags jsonb NOT NULL DEFAULT '[]',
    settings jsonb NOT NULL DEFAULT '{}',
    gating jsonb NOT NULL DEFAULT '{}',
    created_by uuid REFERENCES users (id) ON DELETE SET NULL,
    last_modified_at timestamptz,
    deleted_at timestamptz,
    size_bytes bigint NOT NULL DEFAULT 0,
    lfs_bytes bigint NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);
--bun:split
CREATE UNIQUE INDEX repos_name_key ON repos (namespace_id, type, lower(name)) WHERE deleted_at IS NULL;
--bun:split
CREATE INDEX repos_deleted_at_idx ON repos (deleted_at);
--bun:split

CREATE TABLE repo_redirects (
    id uuid PRIMARY KEY,
    old_namespace text NOT NULL,
    old_name text NOT NULL,
    type text NOT NULL,
    repo_id uuid NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);
--bun:split
CREATE UNIQUE INDEX repo_redirects_key ON repo_redirects (type, lower(old_namespace), lower(old_name));
--bun:split

CREATE TABLE collaborators (
    id uuid PRIMARY KEY,
    repo_id uuid NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role text NOT NULL CHECK (role IN ('read', 'write')),
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (repo_id, user_id)
);
--bun:split

-- Compare-and-swap on target (04-git.md "Ref updates").
CREATE TABLE refs (
    repo_id uuid NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    name text NOT NULL,
    target text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (repo_id, name)
);
--bun:split

CREATE TABLE packs (
    repo_id uuid NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    pack_sha1 text NOT NULL,
    size bigint NOT NULL,
    object_count bigint NOT NULL,
    state text NOT NULL CHECK (state IN ('pending', 'live', 'retired', 'orphaned')),
    retired_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (repo_id, pack_sha1)
);
--bun:split
CREATE INDEX packs_state_idx ON packs (repo_id, state);
--bun:split

CREATE TABLE commits (
    repo_id uuid NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    sha text NOT NULL,
    tree text NOT NULL,
    parents jsonb NOT NULL DEFAULT '[]',
    author jsonb NOT NULL DEFAULT '{}',
    committer jsonb NOT NULL DEFAULT '{}',
    title text NOT NULL DEFAULT '',
    message text NOT NULL DEFAULT '',
    authored_at timestamptz NOT NULL,
    committed_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (repo_id, sha)
);
--bun:split

CREATE TABLE tree_entries (
    repo_id uuid NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    commit_sha text NOT NULL,
    path text NOT NULL,
    parent text NOT NULL,
    type text NOT NULL CHECK (type IN ('file', 'directory')),
    size bigint NOT NULL DEFAULT 0,
    git_oid text NOT NULL,
    lfs_oid text,
    lfs_size bigint,
    pointer_size integer,
    last_commit text,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (repo_id, commit_sha, path)
);
--bun:split
CREATE INDEX tree_entries_parent_idx ON tree_entries (repo_id, commit_sha, parent, path);
--bun:split

-- LFS objects and uploads ----------------------------------------------------

CREATE TABLE lfs_objects (
    oid text PRIMARY KEY,
    size bigint NOT NULL,
    storage_key text NOT NULL,
    verified_at timestamptz,
    last_referenced_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);
--bun:split

CREATE TABLE repo_lfs (
    repo_id uuid NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    oid text NOT NULL,
    size bigint NOT NULL DEFAULT 0,
    present boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (repo_id, oid)
);
--bun:split
CREATE INDEX repo_lfs_oid_idx ON repo_lfs (oid);
--bun:split

CREATE TABLE uploads (
    id uuid PRIMARY KEY,
    oid text NOT NULL,
    size bigint NOT NULL,
    kind text NOT NULL DEFAULT 'lfs',
    upload_id text NOT NULL DEFAULT '',
    storage_key text NOT NULL,
    part_size bigint NOT NULL DEFAULT 0,
    parts jsonb NOT NULL DEFAULT '[]',
    user_id uuid REFERENCES users (id) ON DELETE SET NULL,
    repo_id uuid REFERENCES repos (id) ON DELETE CASCADE,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);
--bun:split
CREATE INDEX uploads_oid_idx ON uploads (oid);
--bun:split
CREATE INDEX uploads_expires_at_idx ON uploads (expires_at);
--bun:split

-- Upstreams, Mirrors and Dependencies -----------------------------------------

CREATE TABLE upstreams (
    id uuid PRIMARY KEY,
    name text NOT NULL UNIQUE,
    display_name text NOT NULL DEFAULT '',
    base_url text NOT NULL,
    viewer_url text NOT NULL DEFAULT '',
    kind text NOT NULL DEFAULT 'hf' CHECK (kind IN ('hf')),
    enabled boolean NOT NULL DEFAULT true,
    namespace_prefix text NOT NULL DEFAULT '',
    source text NOT NULL DEFAULT 'admin' CHECK (source IN ('config', 'admin')),
    instance_token_ciphertext text,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);
--bun:split

CREATE TABLE upstream_tokens (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    upstream_id uuid NOT NULL REFERENCES upstreams (id) ON DELETE CASCADE,
    ciphertext text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (user_id, upstream_id)
);
--bun:split

CREATE TABLE mirrors (
    repo_id uuid PRIMARY KEY REFERENCES repos (id) ON DELETE CASCADE,
    upstream_id uuid NOT NULL REFERENCES upstreams (id),
    upstream_repo text NOT NULL,
    include jsonb NOT NULL DEFAULT '[]',
    exclude jsonb NOT NULL DEFAULT '[]',
    selection jsonb NOT NULL DEFAULT '{}',
    track boolean NOT NULL DEFAULT false,
    track_ref text NOT NULL DEFAULT '',
    schedule text NOT NULL DEFAULT '',
    last_synced_commit text,
    last_synced_at timestamptz,
    status text NOT NULL DEFAULT 'pending',
    last_error text NOT NULL DEFAULT '',
    detached_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);
--bun:split
CREATE INDEX mirrors_upstream_idx ON mirrors (upstream_id, upstream_repo);
--bun:split

CREATE TABLE dependencies (
    id uuid PRIMARY KEY,
    repo_id uuid NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    commit_sha text NOT NULL,
    target_type text NOT NULL DEFAULT 'model',
    target_repo text NOT NULL,
    target_revision text NOT NULL DEFAULT '',
    kind text NOT NULL CHECK (kind IN ('code', 'base_model', 'adapter_base', 'card_base_model', 'dataset')),
    required boolean NOT NULL DEFAULT false,
    source_path text NOT NULL DEFAULT '',
    source_key text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);
--bun:split
CREATE INDEX dependencies_repo_idx ON dependencies (repo_id, commit_sha);
--bun:split
CREATE INDEX dependencies_target_idx ON dependencies (target_type, lower(target_repo));
--bun:split

-- Index and scan results -------------------------------------------------------

CREATE TABLE metadata (
    repo_id uuid NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    commit_sha text NOT NULL,
    indexer_version integer NOT NULL,
    summary jsonb NOT NULL DEFAULT '{}',
    indexed_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (repo_id, commit_sha, indexer_version)
);
--bun:split

CREATE TABLE scan_results (
    repo_id uuid NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    commit_sha text NOT NULL,
    path text NOT NULL,
    scanner_version integer NOT NULL,
    status text NOT NULL CHECK (status IN ('safe', 'suspicious', 'unsafe', 'error')),
    findings jsonb NOT NULL DEFAULT '[]',
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (repo_id, commit_sha, path, scanner_version)
);
--bun:split

-- Jobs ------------------------------------------------------------------------

CREATE TABLE jobs (
    id uuid PRIMARY KEY,
    kind text NOT NULL,
    dedupe_key text,
    payload jsonb NOT NULL DEFAULT '{}',
    status text NOT NULL CHECK (status IN ('pending', 'running', 'succeeded', 'failed', 'cancelled')),
    priority integer NOT NULL DEFAULT 0,
    attempts integer NOT NULL DEFAULT 0,
    max_attempts integer NOT NULL DEFAULT 5,
    run_after timestamptz NOT NULL,
    lease_owner text,
    lease_expires_at timestamptz,
    progress jsonb NOT NULL DEFAULT '{}',
    result jsonb NOT NULL DEFAULT '{}',
    error text NOT NULL DEFAULT '',
    created_by uuid REFERENCES users (id) ON DELETE SET NULL,
    parent_id uuid REFERENCES jobs (id) ON DELETE CASCADE,
    trace_context jsonb NOT NULL DEFAULT '{}',
    started_at timestamptz,
    finished_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);
--bun:split
CREATE UNIQUE INDEX jobs_dedupe_key ON jobs (kind, dedupe_key) WHERE status IN ('pending', 'running') AND dedupe_key IS NOT NULL;
--bun:split
CREATE INDEX jobs_claim_idx ON jobs (status, priority DESC, run_after);
--bun:split
CREATE INDEX jobs_parent_idx ON jobs (parent_id);
--bun:split
CREATE INDEX jobs_created_by_idx ON jobs (created_by, created_at);
--bun:split

CREATE TABLE schedules (
    id uuid PRIMARY KEY,
    kind text NOT NULL,
    key text NOT NULL,
    cron text NOT NULL,
    payload jsonb NOT NULL DEFAULT '{}',
    enabled boolean NOT NULL DEFAULT true,
    next_run_at timestamptz NOT NULL,
    last_run_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (kind, key)
);
--bun:split

-- Shares the Pull bandwidth budget across workers (02-architecture.md "Jobs").
CREATE TABLE rate_limit_leases (
    name text NOT NULL,
    owner text NOT NULL,
    bytes_per_second bigint NOT NULL,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (name, owner)
);
--bun:split

-- Collections and community -----------------------------------------------------

CREATE TABLE collections (
    id uuid PRIMARY KEY,
    namespace_id uuid NOT NULL REFERENCES namespaces (id) ON DELETE CASCADE,
    slug text NOT NULL,
    title text NOT NULL,
    description text NOT NULL DEFAULT '',
    visibility text NOT NULL CHECK (visibility IN ('public', 'internal', 'private')),
    theme text NOT NULL DEFAULT '',
    position integer NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);
--bun:split
CREATE UNIQUE INDEX collections_slug_key ON collections (namespace_id, lower(slug));
--bun:split

CREATE TABLE collection_items (
    id uuid PRIMARY KEY,
    collection_id uuid NOT NULL REFERENCES collections (id) ON DELETE CASCADE,
    item_type text NOT NULL,
    item_id text NOT NULL,
    position integer NOT NULL,
    note text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);
--bun:split
CREATE INDEX collection_items_collection_idx ON collection_items (collection_id, position);
--bun:split

CREATE TABLE discussions (
    id uuid PRIMARY KEY,
    repo_id uuid NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    num integer NOT NULL,
    kind text NOT NULL CHECK (kind IN ('discussion', 'pull_request')),
    title text NOT NULL,
    status text NOT NULL CHECK (status IN ('draft', 'open', 'closed', 'merged')),
    author_id uuid REFERENCES users (id) ON DELETE SET NULL,
    pr_ref text,
    pr_base text,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (repo_id, num)
);
--bun:split

CREATE TABLE discussion_events (
    id uuid PRIMARY KEY,
    discussion_id uuid NOT NULL REFERENCES discussions (id) ON DELETE CASCADE,
    type text NOT NULL,
    author_id uuid REFERENCES users (id) ON DELETE SET NULL,
    body text NOT NULL DEFAULT '',
    data jsonb NOT NULL DEFAULT '{}',
    hidden boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);
--bun:split
CREATE INDEX discussion_events_discussion_idx ON discussion_events (discussion_id, created_at);
--bun:split

CREATE TABLE likes (
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    repo_id uuid NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (user_id, repo_id)
);
--bun:split
CREATE INDEX likes_repo_idx ON likes (repo_id);
--bun:split

CREATE TABLE download_counts (
    repo_id uuid NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    day date NOT NULL,
    count bigint NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (repo_id, day)
);
--bun:split

CREATE TABLE access_requests (
    id uuid PRIMARY KEY,
    repo_id uuid NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    status text NOT NULL CHECK (status IN ('pending', 'accepted', 'rejected')),
    fields jsonb NOT NULL DEFAULT '{}',
    decided_by uuid REFERENCES users (id) ON DELETE SET NULL,
    decided_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (repo_id, user_id)
);
--bun:split

-- Webhooks, audit, quotas and settings -----------------------------------------

CREATE TABLE webhooks (
    id uuid PRIMARY KEY,
    scope_namespace_id uuid REFERENCES namespaces (id) ON DELETE CASCADE,
    scope_repo_id uuid REFERENCES repos (id) ON DELETE CASCADE,
    url text NOT NULL,
    secret_ciphertext text NOT NULL DEFAULT '',
    events jsonb NOT NULL DEFAULT '[]',
    enabled boolean NOT NULL DEFAULT true,
    created_by uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK (scope_namespace_id IS NOT NULL OR scope_repo_id IS NOT NULL)
);
--bun:split

CREATE TABLE webhook_deliveries (
    id uuid PRIMARY KEY,
    webhook_id uuid NOT NULL REFERENCES webhooks (id) ON DELETE CASCADE,
    event text NOT NULL,
    event_id uuid NOT NULL,
    payload text NOT NULL,
    status_code integer,
    attempt integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz,
    duration_ms integer,
    error text NOT NULL DEFAULT '',
    delivered_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);
--bun:split
CREATE INDEX webhook_deliveries_webhook_idx ON webhook_deliveries (webhook_id, created_at);
--bun:split

CREATE TABLE audit_events (
    id uuid PRIMARY KEY,
    actor_user_id uuid,
    actor_token_id uuid,
    action text NOT NULL,
    target_type text NOT NULL DEFAULT '',
    target_id text NOT NULL DEFAULT '',
    ip text NOT NULL DEFAULT '',
    user_agent text NOT NULL DEFAULT '',
    data jsonb NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);
--bun:split
CREATE INDEX audit_events_created_idx ON audit_events (created_at);
--bun:split
CREATE INDEX audit_events_actor_idx ON audit_events (actor_user_id, created_at);
--bun:split
CREATE INDEX audit_events_action_idx ON audit_events (action, created_at);
--bun:split

CREATE TABLE quotas (
    namespace_id uuid PRIMARY KEY REFERENCES namespaces (id) ON DELETE CASCADE,
    bytes_limit bigint NOT NULL,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);
--bun:split

CREATE TABLE settings (
    key text PRIMARY KEY,
    value jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);
--bun:split

-- Xet ---------------------------------------------------------------------------

CREATE TABLE xet_xorbs (
    hash text PRIMARY KEY,
    size bigint NOT NULL,
    storage_key text NOT NULL,
    last_referenced_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);
--bun:split
CREATE INDEX xet_xorbs_last_referenced_idx ON xet_xorbs (last_referenced_at);
--bun:split

CREATE TABLE xet_files (
    xet_hash text PRIMARY KEY,
    oid text NOT NULL,
    size bigint NOT NULL,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);
--bun:split

-- Datasets (14-datasets.md) ------------------------------------------------------

CREATE TABLE dataset_splits (
    repo_id uuid NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    commit_sha text NOT NULL,
    config text NOT NULL,
    split text NOT NULL,
    num_rows bigint,
    num_bytes_source bigint,
    num_bytes_parquet bigint,
    features jsonb NOT NULL DEFAULT '{}',
    status text NOT NULL CHECK (status IN ('pending', 'ready', 'partial', 'failed')),
    error text NOT NULL DEFAULT '',
    converted_commit text,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (repo_id, commit_sha, config, split)
);
--bun:split

CREATE TABLE dataset_stats (
    repo_id uuid NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    commit_sha text NOT NULL,
    config text NOT NULL,
    split text NOT NULL,
    column_name text NOT NULL,
    stats jsonb NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (repo_id, commit_sha, config, split, column_name)
);
