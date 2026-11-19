CREATE POLICY order_tenant ON idempotent_orders
USING (tenant_id = NULLIF(pg_catalog.current_setting('typhon.tenant_id', true), ''))
WITH CHECK (tenant_id = NULLIF(pg_catalog.current_setting('typhon.tenant_id', true), ''));
