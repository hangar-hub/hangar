-- A Repo's size, total and present (07-indexing-and-scanning.md "size",
-- 09-web-ui.md "size present / total"). size_bytes becomes the total: git
-- data plus every LFS object the Repo's Commits reference, held or not.
-- present_bytes is git data plus the LFS objects the Instance holds, which
-- is what size_bytes counted before. The two differ only for a Partial
-- mirror. Both sides of the UPDATE read the old size_bytes.
ALTER TABLE repos ADD COLUMN present_bytes bigint NOT NULL DEFAULT 0;
--bun:split
UPDATE repos SET present_bytes = size_bytes,
    size_bytes = size_bytes + (SELECT COALESCE(SUM(size), 0) FROM repo_lfs WHERE repo_lfs.repo_id = repos.id AND NOT repo_lfs.present);
