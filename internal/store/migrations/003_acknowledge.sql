-- When somebody acknowledged a firing critical problem in the web UI; empty if
-- nobody did. It belongs to this row and not to the key, so a problem that
-- comes back after "resolved" is a new, unacknowledged alarm.
ALTER TABLE findings ADD COLUMN acknowledged_at INTEGER;
