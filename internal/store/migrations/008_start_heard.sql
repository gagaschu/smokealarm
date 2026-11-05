-- Whether a target heard the start of a problem. firing_notified_at cannot say
-- that: it is cleared to report a problem again (after an escalation or the end
-- of a downtime), and if that report fails, the target would never hear the end.
ALTER TABLE deliveries ADD COLUMN start_heard_at INTEGER;
UPDATE deliveries SET start_heard_at = firing_notified_at;
