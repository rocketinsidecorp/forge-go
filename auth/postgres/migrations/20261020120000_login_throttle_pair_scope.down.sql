-- Throttle counters are ephemeral: dropping the pair counters only forgives
-- attempts already counted in this window.
DELETE FROM forge_login_throttle WHERE scope = 3 OR attempt_count = 0;

ALTER TABLE forge_login_throttle
    DROP CONSTRAINT forge_login_throttle_scope_check,
    DROP CONSTRAINT forge_login_throttle_attempt_count_check;

ALTER TABLE forge_login_throttle
    ADD CONSTRAINT forge_login_throttle_scope_check CHECK (scope IN (1, 2)),
    ADD CONSTRAINT forge_login_throttle_attempt_count_check CHECK (attempt_count > 0);
