-- Each long-running service writes one row here every minute. The status
-- page reads it to show which parts of the system are alive.
CREATE TABLE IF NOT EXISTS heartbeats (
    service    text        PRIMARY KEY,
    host       text        NOT NULL,
    detail     jsonb       NOT NULL DEFAULT '{}',
    last_seen  timestamptz NOT NULL DEFAULT now()
);
