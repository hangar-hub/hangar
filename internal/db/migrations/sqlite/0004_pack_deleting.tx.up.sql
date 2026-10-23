-- A sweep claims a pack by setting its state to deleting before it deletes
-- the pack's files (04-git.md "Compaction and garbage collection"), so a
-- push that stores the same pack again waits for the sweep to finish
-- instead of uploading files the sweep then deletes. SQLite cannot change
-- a CHECK constraint, so the table is rebuilt; no other table refers to it.
CREATE TABLE packs_new (
    repo_id TEXT NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    pack_sha1 text NOT NULL,
    size bigint NOT NULL,
    object_count bigint NOT NULL,
    state text NOT NULL CHECK (state IN ('pending', 'live', 'retired', 'orphaned', 'deleting')),
    retired_at TIMESTAMP,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (repo_id, pack_sha1)
);
--bun:split
INSERT INTO packs_new (repo_id, pack_sha1, size, object_count, state, retired_at, created_at, updated_at)
SELECT repo_id, pack_sha1, size, object_count, state, retired_at, created_at, updated_at FROM packs;
--bun:split
DROP TABLE packs;
--bun:split
ALTER TABLE packs_new RENAME TO packs;
--bun:split
CREATE INDEX packs_state_idx ON packs (repo_id, state);
