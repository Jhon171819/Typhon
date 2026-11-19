CREATE POLICY tenant_select ON tenant_records FOR SELECT
USING (tenant_id = NULLIF(pg_catalog.current_setting('typhon.tenant_id', true), ''));
