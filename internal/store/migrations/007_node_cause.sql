-- caused_by is the node that is down, if the problem is a consequence of it;
-- the engine keeps such a problem pending while the node is down. Empty means
-- no such node.
ALTER TABLE findings ADD COLUMN caused_by TEXT NOT NULL DEFAULT '';
-- pending_since is when the ALERT_AFTER clock of a pending problem started.
-- It is first_seen, unless the problem was held back by a down node: then it
-- is the run in which the node came back. NULL means first_seen.
ALTER TABLE findings ADD COLUMN pending_since INTEGER;
