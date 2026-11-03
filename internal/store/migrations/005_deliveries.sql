-- What each notification target has been told. Every target keeps its own
-- markers, so a target that fails is retried on its own and the others get
-- no duplicates. The columns in findings stay as the summary over all targets
-- ("last reported anywhere"); a target that is added later starts from them.
CREATE TABLE deliveries (
    finding_id           INTEGER NOT NULL,
    target               TEXT    NOT NULL,
    firing_notified_at   INTEGER,
    resolved_notified_at INTEGER,
    PRIMARY KEY (finding_id, target)
);

-- The targets that were configured at the last start. A target that is not in
-- here yet is new and starts from the current state instead of receiving the
-- whole history.
CREATE TABLE targets (
    name     TEXT    PRIMARY KEY,
    added_at INTEGER NOT NULL
);
