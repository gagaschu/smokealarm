-- Login sessions. Only the SHA-256 hash of the session id is stored, so a
-- leaked database file cannot be used to take over a session.
CREATE TABLE sessions (
    id_hash    TEXT    PRIMARY KEY,
    username   TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);

CREATE INDEX sessions_expires ON sessions (expires_at);
