-- Scope 3 counts attempts per account and source together (see
-- auth.LoginThrottleKey), beside scope 1 (account) and scope 2 (source).
-- A counter row is now created empty (attempt_count 0) before it is locked
-- and incremented, so concurrent first attempts serialize on the row.
ALTER TABLE forge_login_throttle
    DROP CONSTRAINT forge_login_throttle_scope_check,
    DROP CONSTRAINT forge_login_throttle_attempt_count_check;

ALTER TABLE forge_login_throttle
    ADD CONSTRAINT forge_login_throttle_scope_check CHECK (scope IN (1, 2, 3)),
    ADD CONSTRAINT forge_login_throttle_attempt_count_check CHECK (attempt_count >= 0);
