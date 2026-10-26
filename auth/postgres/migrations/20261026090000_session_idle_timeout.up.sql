-- When each session was last used, for the idle timeout
-- (auth/postgres.WithSessionIdleTimeout). Sessions that exist when this
-- migration runs count as used now, so upgrading signs nobody out; from then
-- on a session unused for the idle timeout no longer resolves. now() is
-- stable, so PostgreSQL stores the default once instead of rewriting the
-- table.
ALTER TABLE forge_sessions
    ADD COLUMN last_seen_at timestamptz NOT NULL DEFAULT now();
