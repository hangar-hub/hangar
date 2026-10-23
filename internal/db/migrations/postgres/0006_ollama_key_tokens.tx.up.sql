-- The Token that registered an Ollama key, if a Token did (05-auth.md
-- "Ollama keys"). Registry tokens for the key get that Token's scope,
-- restrictions and expiry, and stop when it is revoked or expires, so a
-- Token cannot hand out wider or longer access than its own through a
-- key. Keys added in a UI session, and keys from before this migration,
-- have none and keep the user's read access.
ALTER TABLE ollama_keys ADD COLUMN token_id uuid REFERENCES tokens (id) ON DELETE CASCADE;
--bun:split
CREATE INDEX ollama_keys_token_id_idx ON ollama_keys (token_id);
