-- Which Repo each generated Ollama layer in ollama_blobs was built from
-- (08-local-tools.md "Blobs"). The registry serves a row of ollama_blobs
-- only through a Repo it was built from, so a layer of a private Repo,
-- such as a GGUF file stored in git rather than LFS, cannot be read
-- through a public Repo by its digest. Layers stored before this migration
-- have no Repo; the next manifest request for a Repo links its layers
-- again, and Ollama always asks for the manifest before the blobs. A
-- Repo's UUID is never reused, so rows of a purged Repo are never served.
CREATE TABLE ollama_repo_blobs (
    repo_id uuid NOT NULL,
    digest text NOT NULL REFERENCES ollama_blobs (digest) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (repo_id, digest)
);
--bun:split
CREATE INDEX ollama_repo_blobs_digest_idx ON ollama_repo_blobs (digest);
