CREATE POLICY tenant_insert ON tenant_records FOR INSERT
WITH CHECK (tenant_id = NULLIF(pg_catalog.current_setting('typhon.tenant_id', true), ''));
