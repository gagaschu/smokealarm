-- Notification targets that are switched off in the web UI. A muted target
-- hears nothing; the row survives restarts, so the switch does not fall back
-- to "on" when the pod is rescheduled. unmute_requested_at is set when the
-- web UI asked to switch the target on again: the engine finishes that
-- (see FinishUnmute), because only it knows what the target missed.
CREATE TABLE target_mutes (
    target              TEXT PRIMARY KEY,
    muted_at            INTEGER NOT NULL,
    unmute_requested_at INTEGER
);
