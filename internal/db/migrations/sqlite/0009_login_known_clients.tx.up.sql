-- IP addresses each User has signed in from with a password (05-auth.md
-- "Local accounts"). The throttle's per-username lock does not apply to
-- them, so failures sent from elsewhere cannot lock a User out where they
-- usually sign in. Rows are removed 90 days after the last success.
CREATE TABLE login_known_clients (
    user_id TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    ip text NOT NULL,
    last_success_at TIMESTAMP NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (user_id, ip)
);
--bun:split
CREATE INDEX login_known_clients_last_success_at_idx ON login_known_clients (last_success_at);
