-- Which credentials a session was started with, so that changing the password
-- or the admin user ends the sessions from before. Existing sessions have no
-- value and end with the upgrade.
ALTER TABLE sessions ADD COLUMN credentials_hash TEXT NOT NULL DEFAULT '';
