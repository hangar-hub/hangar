-- What the approval page shows about a device login, and what it grants
-- (05-auth.md "Device login"): the IP address that started it, and the
-- scope the user chose when approving. Codes from before this migration
-- have no IP address and get a read Token; they expire within 15 minutes.
ALTER TABLE oauth_device_codes ADD COLUMN client_ip text NOT NULL DEFAULT '';
--bun:split
ALTER TABLE oauth_device_codes ADD COLUMN scope text NOT NULL DEFAULT 'read' CHECK (scope IN ('read', 'write'));
