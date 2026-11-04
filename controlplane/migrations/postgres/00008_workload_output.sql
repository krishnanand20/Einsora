-- Workload output (ADR-0024): stdout and stderr that agents ship, with credentials removed, kept for
-- the retention period and within a size limit per workload. Readers keep their place by seq, which
-- counts each workload's output on its own (workload_output_seq), so it never shows how much output
-- anyone else's workloads produce, and is never used twice. workload_output_state records how far
-- each stream has been received, and how much was lost, trimmed, or expired, so readers are told.

-- +goose Up
CREATE TABLE workload_output (
    id          bigserial   PRIMARY KEY,
    workload_id uuid        NOT NULL REFERENCES workloads (id),
    seq         bigint      NOT NULL CHECK (seq > 0),
    stream      text        NOT NULL CHECK (stream IN ('stdout', 'stderr', 'einsora')),
    at          timestamptz NOT NULL,
    content     text        NOT NULL,
    redactions  integer     NOT NULL CHECK (redactions >= 0)
);
CREATE UNIQUE INDEX workload_output_workload_idx ON workload_output (workload_id, seq);
CREATE INDEX workload_output_at_idx ON workload_output (at);

CREATE TABLE workload_output_seq (
    workload_id uuid   PRIMARY KEY REFERENCES workloads (id),
    last_seq    bigint NOT NULL CHECK (last_seq > 0)
);

CREATE TABLE workload_output_state (
    workload_id   uuid   NOT NULL REFERENCES workloads (id),
    stream        text   NOT NULL CHECK (stream IN ('stdout', 'stderr', 'einsora')),
    source        text   NOT NULL,
    next_offset   bigint NOT NULL CHECK (next_offset >= 0),
    stored_bytes  bigint NOT NULL CHECK (stored_bytes >= 0),
    lost_bytes    bigint NOT NULL CHECK (lost_bytes >= 0),
    trimmed_bytes bigint NOT NULL CHECK (trimmed_bytes >= 0),
    expired_bytes bigint NOT NULL CHECK (expired_bytes >= 0),
    PRIMARY KEY (workload_id, stream)
);

-- +goose Down
DROP TABLE workload_output_seq;
DROP TABLE workload_output_state;
DROP TABLE workload_output;
