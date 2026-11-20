CREATE TABLE idempotent_orders (
  id bigint GENERATED ALWAYS AS IDENTITY,
  tenant_id text NOT NULL,
  actor text NOT NULL,
  request_key text NOT NULL,
  fingerprint text NOT NULL CHECK (length(fingerprint) = 64),
  item text NOT NULL CHECK (length(item) BETWEEN 1 AND 80),
  PRIMARY KEY (tenant_id, actor, request_key)
);
