-- Restores what create_tasks created (PostgreSQL 18 or later).
ALTER TABLE tasks ALTER COLUMN id SET DEFAULT uuidv7();
