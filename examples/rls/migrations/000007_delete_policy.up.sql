CREATE POLICY tenant_delete ON tenant_records FOR DELETE
USING (tenant_id = NULLIF(pg_catalog.current_setting('typhon.tenant_id', true), ''));
