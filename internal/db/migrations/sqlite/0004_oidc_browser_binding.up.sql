-- Each OIDC sign-in is bound to the browser that started it (05-auth.md
-- "OIDC"): the browser holds a random value in a short-lived cookie, and
-- the state row holds its SHA-256. The callback refuses a state whose
-- browser hash does not match, so a sign-in started in one browser cannot
-- be finished in another. States from before this migration have no hash
-- and are refused; they expire within ten minutes anyway.
ALTER TABLE oidc_states ADD COLUMN browser_hash text NOT NULL DEFAULT '';
