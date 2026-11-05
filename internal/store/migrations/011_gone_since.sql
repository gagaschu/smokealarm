-- Since when a firing problem is no longer found, while it waits for
-- RESOLVE_AFTER. A crash loop is out of the "Waiting" state between two
-- back-offs, and must not send "resolved" and "critical" every time.
ALTER TABLE findings ADD COLUMN gone_since INTEGER;
