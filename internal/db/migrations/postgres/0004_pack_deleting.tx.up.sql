-- A sweep claims a pack by setting its state to deleting before it deletes
-- the pack's files (04-git.md "Compaction and garbage collection"), so a
-- push that stores the same pack again waits for the sweep to finish
-- instead of uploading files the sweep then deletes.
ALTER TABLE packs DROP CONSTRAINT packs_state_check;
--bun:split
ALTER TABLE packs ADD CONSTRAINT packs_state_check CHECK (state IN ('pending', 'live', 'retired', 'orphaned', 'deleting'));
