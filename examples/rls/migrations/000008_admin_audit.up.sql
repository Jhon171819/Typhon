CREATE TABLE tenant_admin_audit (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  occurred_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  actor name NOT NULL DEFAULT current_user,
  change_ref text NOT NULL CHECK (length(change_ref) BETWEEN 1 AND 80),
  operation text NOT NULL CHECK (length(operation) BETWEEN 1 AND 80)
);
