CREATE POLICY tenant_update ON tenant_records FOR UPDATE
USING (tenant_id = NULLIF(pg_catalog.current_setting('typhon.tenant_id', true), ''))
WITH CHECK (tenant_id = NULLIF(pg_catalog.current_setting('typhon.tenant_id', true), ''));
