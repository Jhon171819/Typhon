CREATE TABLE compose_records (
  id text PRIMARY KEY CHECK (length(id) BETWEEN 1 AND 80),
  name text NOT NULL CHECK (length(name) BETWEEN 1 AND 120)
);
