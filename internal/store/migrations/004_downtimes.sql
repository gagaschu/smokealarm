-- Downtimes silence the mails of one problem (one key) until expires_at. Object
-- and reason are copied, because the row in findings can be gone already: the
-- problem may be resolved and past its retention while the downtime still runs.
-- AUTOINCREMENT never reuses an id, so an "End" button on a page that is out of
-- date cannot hit a later downtime.
CREATE TABLE downtimes (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    key        TEXT    NOT NULL UNIQUE,
    check_name TEXT    NOT NULL,
    kind       TEXT    NOT NULL,
    namespace  TEXT    NOT NULL DEFAULT '',
    name       TEXT    NOT NULL,
    reason     TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);

CREATE INDEX downtimes_expires ON downtimes (expires_at);
