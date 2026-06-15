CREATE TABLE IF NOT EXISTS event_publication
(
    id                     TEXT    NOT NULL PRIMARY KEY,
    listener_id            TEXT    NOT NULL,
    event_type             TEXT    NOT NULL,
    serialized_event       TEXT    NOT NULL,
    publication_date       TEXT    NOT NULL,
    completion_date        TEXT,
    status                 TEXT    NOT NULL,
    completion_attempts    INTEGER NOT NULL DEFAULT 0,
    last_resubmission_date TEXT
);

CREATE INDEX IF NOT EXISTS event_publication_by_status_idx ON event_publication (status);
CREATE INDEX IF NOT EXISTS event_publication_by_publication_date_idx ON event_publication (publication_date);

CREATE TABLE IF NOT EXISTS event_publication_archive
(
    id                     TEXT    NOT NULL PRIMARY KEY,
    listener_id            TEXT    NOT NULL,
    event_type             TEXT    NOT NULL,
    serialized_event       TEXT    NOT NULL,
    publication_date       TEXT    NOT NULL,
    completion_date        TEXT,
    status                 TEXT    NOT NULL,
    completion_attempts    INTEGER NOT NULL DEFAULT 0,
    last_resubmission_date TEXT
);
