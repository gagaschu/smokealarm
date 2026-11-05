-- When a firing problem became critical, while that is not reported yet. A
-- problem that flips between warning and critical must not send a message per
-- flip: the escalation is reported only once it has lasted ALERT_AFTER.
ALTER TABLE findings ADD COLUMN escalating_since INTEGER;
