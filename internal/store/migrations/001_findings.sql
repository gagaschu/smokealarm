-- One row per problem. A problem is "open" while it is pending or firing;
-- resolved rows stay as history until retention removes them.
-- All timestamps are Unix seconds (UTC).
CREATE TABLE findings (
    id                   INTEGER PRIMARY KEY,
    key                  TEXT    NOT NULL,
    check_name           TEXT    NOT NULL,
    severity             TEXT    NOT NULL CHECK (severity IN ('critical', 'warning')),
    kind                 TEXT    NOT NULL,
    namespace            TEXT    NOT NULL DEFAULT '',
    name                 TEXT    NOT NULL,
    reason               TEXT    NOT NULL,
    message              TEXT    NOT NULL,
    state                TEXT    NOT NULL CHECK (state IN ('pending', 'firing', 'resolved')),
    first_seen           INTEGER NOT NULL,
    last_seen            INTEGER NOT NULL,
    firing_at            INTEGER,
    resolved_at          INTEGER,
    firing_notified_at   INTEGER,
    resolved_notified_at INTEGER
);

-- At most one open row per problem.
CREATE UNIQUE INDEX findings_open_key ON findings (key) WHERE state IN ('pending', 'firing');
CREATE INDEX findings_state ON findings (state);
CREATE INDEX findings_resolved ON findings (resolved_at) WHERE state = 'resolved';
