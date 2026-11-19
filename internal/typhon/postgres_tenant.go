package typhon

import (
	"log"
	"time"
)

const postgresPrivilegedRole = `SELECT EXISTS (
	SELECT 1 FROM pg_catalog.pg_roles AS role
	WHERE (role.rolsuper OR role.rolbypassrls)
	AND (
		role.rolname = current_user OR role.rolname = session_user
		OR pg_catalog.pg_has_role(current_user, role.oid, 'MEMBER')
		OR pg_catalog.pg_has_role(session_user, role.oid, 'MEMBER')
	)
)`

func (db *postgresConnection) tenantTransaction(vm *VM, args []Value) (Value, error) {
	request := args[0]
	state, ok := request.NativeObject.data.(*webRequest)
	if !ok || vm.request == nil || vm.request.web != state || state.authorizationAction == "" ||
		!validAuthText(state.principal.subject, 256) || !validAuthText(state.principal.tenant, 256) {
		return NoneValue(), (&webRejection{status: 403}).exception()
	}
	principal, action := state.principal, state.authorizationAction
	value, err := db.scopedTransaction(vm, args[1], args[2:], func(tx *postgresTransaction) error {
		if !principal.expires.IsZero() && !time.Now().Before(principal.expires) {
			return (&webRejection{status: 403}).exception()
		}
		ctx, cancel := tx.operation(vm)
		defer cancel()
		var privileged bool
		if err := tx.tx.QueryRow(ctx, postgresPrivilegedRole).Scan(&privileged); err != nil {
			return driverFailure("PostgreSQL", databaseKind, err)
		}
		if privileged {
			log.Printf("Typhon tenant transaction id=%s action=%q reason=privileged_database_role", state.id, action)
			return (&webRejection{status: 403}).exception()
		}
		_, err := tx.tx.Exec(ctx,
			"SELECT pg_catalog.set_config('typhon.tenant_id',$1,true), pg_catalog.set_config('row_security','on',true)", principal.tenant)
		return driverFailure("PostgreSQL", databaseKind, err)
	}, request)
	outcome := "completed"
	if err != nil {
		outcome = "failed"
	}
	log.Printf("Typhon tenant transaction id=%s action=%q outcome=%s", state.id, action, outcome)
	return value, err
}
