-- One-time tickets for opening a workload through the access proxy (ADR-0017). Only a ticket's
-- SHA-256 is stored; it's used once, and expires in a minute.

-- +goose Up
CREATE TABLE access_tickets (
    ticket_hash text        PRIMARY KEY,
    tenant_id   uuid        NOT NULL REFERENCES tenants (id),
    workload_id uuid        NOT NULL REFERENCES workloads (id),
    subject     text        NOT NULL,
    expires_at  timestamptz NOT NULL,
    used_at     timestamptz
);
CREATE INDEX access_tickets_expires_idx ON access_tickets (expires_at);

-- +goose Down
DROP TABLE access_tickets;
