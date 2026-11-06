-- Each long-running service writes one row here every minute. The status
-- page reads it to show which parts of the system are alive.
CREATE TABLE IF NOT EXISTS heartbeats (
    service    text        PRIMARY KEY,
    host       text        NOT NULL,
    detail     jsonb       NOT NULL DEFAULT '{}',
    last_seen  timestamptz NOT NULL DEFAULT now()
);

-- One row per SEC filing. EDGAR lists a filing once per party (a Form 4 names
-- the company and each insider), so the parties live together in one jsonb
-- array instead of one row each.
CREATE TABLE IF NOT EXISTS filings (
    accession_no  text        PRIMARY KEY,  -- EDGAR's unique ID: the idempotency key
    form_type     text        NOT NULL,
    cik           text        NOT NULL,     -- the company the filing is about
    company       text        NOT NULL,
    accepted_at   timestamptz NOT NULL,     -- when EDGAR accepted it
    url           text        NOT NULL,
    parties       jsonb       NOT NULL DEFAULT '[]',
    first_seen_at timestamptz NOT NULL DEFAULT now()  -- minus accepted_at: how fresh the feed is
);

-- The feed reads newest first, optionally for one form, and pages with a
-- (accepted_at, accession_no) cursor. Postgres scans these backwards.
CREATE INDEX IF NOT EXISTS filings_by_time ON filings (accepted_at, accession_no);
CREATE INDEX IF NOT EXISTS filings_by_form_time ON filings (form_type, accepted_at, accession_no);
