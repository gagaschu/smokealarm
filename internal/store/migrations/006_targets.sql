-- The notification targets a problem goes to, from the targets annotation of
-- its object or namespace: a comma-separated list of target kinds such as
-- "mail,slack". Empty means every target. It is kept with the problem so that
-- reminders and the history know it, also when the object is gone.
ALTER TABLE findings ADD COLUMN targets TEXT NOT NULL DEFAULT '';
