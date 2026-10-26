-- One row per scheduled job: the latest tick a replica claimed (ADR 0047).
-- A tick runs on the replica whose INSERT ... ON CONFLICT advances
-- last_tick_at; ticks only move forward, so a lagging replica can never claim
-- a tick again, and one row per job needs no pruning. Like forge_jobs it is a
-- framework table, not tenant data: no row-level security.
CREATE TABLE forge_job_schedules (
    name varchar(255) PRIMARY KEY CHECK (name <> ''),
    last_tick_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
