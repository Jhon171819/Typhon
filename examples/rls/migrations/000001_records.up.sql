CREATE TABLE tenant_records (
  tenant_id text NOT NULL CHECK (tenant_id <> ''),
  id text NOT NULL,
  label text NOT NULL,
  PRIMARY KEY (tenant_id, id)
);
