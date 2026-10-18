-- Who uploaded each LFS object (04-git.md "LFS objects"). A Commit may add a
-- pointer to an object only when the caller uploaded it, or can read a Repo
-- that holds it. Objects stored before this table existed have no rows:
-- they stay usable through the Repos that already hold them.
CREATE TABLE lfs_uploaders (
    oid text NOT NULL REFERENCES lfs_objects (oid) ON DELETE CASCADE,
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (oid, user_id)
);
--bun:split
CREATE INDEX lfs_uploaders_user_idx ON lfs_uploaders (user_id);
