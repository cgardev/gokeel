CREATE TABLE IF NOT EXISTS event_message
(
    id               TEXT NOT NULL PRIMARY KEY,
    event_type       TEXT NOT NULL,
    serialized_event TEXT NOT NULL,
    publisher_node   TEXT NOT NULL,
    publication_date TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS event_message_by_type_and_date_idx
    ON event_message (event_type, publication_date);

CREATE INDEX IF NOT EXISTS event_message_by_publication_date_idx
    ON event_message (publication_date);

CREATE TABLE IF NOT EXISTS event_message_listener
(
    listener_id   TEXT NOT NULL PRIMARY KEY,
    delivery_mode TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS event_message_consumer
(
    listener_id       TEXT NOT NULL,
    instance          TEXT NOT NULL,
    event_type        TEXT NOT NULL,
    delivery_mode     TEXT NOT NULL,
    start_boundary    TEXT NOT NULL,
    frontier          TEXT NOT NULL,
    registration_date TEXT NOT NULL,
    heartbeat_date    TEXT NOT NULL,
    PRIMARY KEY (listener_id, instance, event_type)
);

CREATE TABLE IF NOT EXISTS event_message_delivery
(
    message_id        TEXT    NOT NULL,
    listener_id       TEXT    NOT NULL,
    instance          TEXT    NOT NULL,
    status            TEXT    NOT NULL,
    attempts          INTEGER NOT NULL DEFAULT 0,
    claim_token       TEXT    NOT NULL DEFAULT '',
    claim_date        TEXT,
    next_attempt_date TEXT,
    completion_date   TEXT,
    last_error        TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (message_id, listener_id, instance)
);

CREATE INDEX IF NOT EXISTS event_message_delivery_by_consumer_and_status_idx
    ON event_message_delivery (listener_id, instance, status);
