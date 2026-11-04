-- The SQLite version of ../postgres/00008_workload_output.sql.

-- +goose Up
CREATE TABLE workload_output (
    id          INTEGER PRIMARY KEY,
    workload_id TEXT    NOT NULL REFERENCES workloads (id),
    seq         INTEGER NOT NULL CHECK (seq > 0),
    stream      TEXT    NOT NULL CHECK (stream IN ('stdout', 'stderr', 'einsora')),
    at          TEXT    NOT NULL,
    content     TEXT    NOT NULL,
    redactions  INTEGER NOT NULL CHECK (redactions >= 0)
) STRICT;
CREATE UNIQUE INDEX workload_output_workload_idx ON workload_output (workload_id, seq);
CREATE INDEX workload_output_at_idx ON workload_output (at);

CREATE TABLE workload_output_seq (
    workload_id TEXT    PRIMARY KEY REFERENCES workloads (id),
    last_seq    INTEGER NOT NULL CHECK (last_seq > 0)
) STRICT;

CREATE TABLE workload_output_state (
    workload_id   TEXT    NOT NULL REFERENCES workloads (id),
    stream        TEXT    NOT NULL CHECK (stream IN ('stdout', 'stderr', 'einsora')),
    source        TEXT    NOT NULL,
    next_offset   INTEGER NOT NULL CHECK (next_offset >= 0),
    stored_bytes  INTEGER NOT NULL CHECK (stored_bytes >= 0),
    lost_bytes    INTEGER NOT NULL CHECK (lost_bytes >= 0),
    trimmed_bytes INTEGER NOT NULL CHECK (trimmed_bytes >= 0),
    expired_bytes INTEGER NOT NULL CHECK (expired_bytes >= 0),
    PRIMARY KEY (workload_id, stream)
) STRICT;

-- +goose Down
DROP TABLE workload_output_seq;
DROP TABLE workload_output_state;
DROP TABLE workload_output;
