package postgres

import "testing"

// TestRowSecurityAppliesOnlyToRolesItBinds checks the fail-closed rule of an
// unscoped EncryptedColumn against PostgreSQL's: superusers and BYPASSRLS
// roles are exempt, and the table owner only without FORCE.
func TestRowSecurityAppliesOnlyToRolesItBinds(t *testing.T) {
	t.Parallel()
	tenantTable := rowSecurity{enabled: true, forced: true}
	if !tenantTable.applies() {
		t.Error("the generated runtime role must be bound")
	}
	if !(rowSecurity{enabled: true, forced: true, owner: true}).applies() {
		t.Error("FORCE binds the owner")
	}
	if !(rowSecurity{enabled: true}).applies() {
		t.Error("ENABLE binds a role that does not own the table")
	}
	for _, exempt := range []rowSecurity{
		{},
		{enabled: true, forced: true, superuser: true},
		{enabled: true, forced: true, bypass: true},
		{enabled: true, owner: true},
	} {
		if exempt.applies() {
			t.Errorf("%+v must be exempt", exempt)
		}
	}
}
