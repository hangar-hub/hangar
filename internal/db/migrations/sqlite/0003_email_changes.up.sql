-- Email changes a User asked for and has not confirmed (05-auth.md
-- "Local accounts"). A self-set email address takes effect only when the
-- confirmation link, which an admin creates and sends to that address, is
-- used, so it never sits in users.email, takes part in its uniqueness or
-- matches an OIDC sign-in before then. One pending change per User.
CREATE TABLE email_changes (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL UNIQUE REFERENCES users (id) ON DELETE CASCADE,
    email text NOT NULL,
    code_hash text UNIQUE,
    created_by TEXT REFERENCES users (id) ON DELETE SET NULL,
    expires_at TIMESTAMP,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
--bun:split

-- Only verified emails match OIDC sign-ins from now on. Emails stored
-- before this migration were set by admins, invites, the bootstrap admin,
-- OIDC claims or open registration; they keep matching as they did.
UPDATE users SET email_verified = 1 WHERE email IS NOT NULL AND is_system = 0;
