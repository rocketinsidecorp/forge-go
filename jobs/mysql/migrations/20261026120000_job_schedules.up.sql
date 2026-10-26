-- One row per scheduled job: the latest tick a replica claimed (ADR 0047).
-- A tick runs on the replica whose INSERT ... ON DUPLICATE KEY UPDATE
-- advances last_tick_at; ticks only move forward, so a lagging replica can
-- never claim a tick again, and one row per job needs no pruning. Job names
-- compare byte for byte.
CREATE TABLE forge_job_schedules (
    name VARCHAR(255) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
    last_tick_at DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    PRIMARY KEY (name),
    CONSTRAINT forge_job_schedules_name CHECK (name <> '')
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;
