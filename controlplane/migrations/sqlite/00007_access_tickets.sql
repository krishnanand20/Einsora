-- The SQLite version of ../postgres/00007_access_tickets.sql.

-- +goose Up
CREATE TABLE access_tickets (
    ticket_hash TEXT PRIMARY KEY,
    tenant_id   TEXT NOT NULL REFERENCES tenants (id),
    workload_id TEXT NOT NULL REFERENCES workloads (id),
    subject     TEXT NOT NULL,
    expires_at  TEXT NOT NULL,
    used_at     TEXT
) STRICT;
CREATE INDEX access_tickets_expires_idx ON access_tickets (expires_at);

-- +goose Down
DROP TABLE access_tickets;
