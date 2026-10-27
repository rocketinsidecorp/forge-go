package cli

import (
	"fmt"
	"strconv"
	"strings"
)

// statement is one SQL statement and the Go expressions bound to its
// placeholders, in order. Building both together keeps them aligned.
type statement struct {
	SQL  string
	Args []string
}

// ArgList renders the bound arguments for a call.
func (s statement) ArgList() string { return strings.Join(s.Args, ", ") }

// GoLiteral renders the SQL as a Go string literal: a raw literal, or quoted
// lines when the SQL itself contains backticks (MySQL identifier quotes).
func (s statement) GoLiteral() string { return goStringLiteral(s.SQL) }

func goStringLiteral(sql string) string {
	if !strings.Contains(sql, "`") {
		return "`" + sql + "`"
	}
	lines := strings.SplitAfter(sql, "\n")
	for i, line := range lines {
		lines[i] = strconv.Quote(line)
	}
	return strings.Join(lines, " +\n\t\t")
}

// quoteIdentifier quotes a validated snake_case name for the database, so a
// name that is a reserved word in any of them still works.
func quoteIdentifier(database, name string) string {
	if database == "mysql" {
		return "`" + name + "`"
	}
	return `"` + name + `"`
}

// Q quotes an identifier for the application's database.
func (v resourceView) Q(name string) string { return quoteIdentifier(v.Database, name) }

type sqlStatements struct {
	Columns   string // entity columns, in scan order
	Insert    statement
	Select    statement
	ListFirst statement
	ListAfter statement
	Update    statement
	Delete    statement
	Exists    statement
}

func buildStatements(v resourceView) sqlStatements {
	q := v.Q
	placeholder := func(n int) string {
		if v.Postgres {
			return fmt.Sprintf("$%d", n)
		}
		return "?"
	}
	// builder numbers placeholders as arguments are added.
	type builder struct {
		args []string
	}
	add := func(b *builder, arg string) string {
		b.args = append(b.args, arg)
		return placeholder(len(b.args))
	}
	tenantPredicate := func(b *builder) string {
		if !v.Tenant {
			return ""
		}
		return q("org_id") + " = " + add(b, "orgID") + " AND "
	}

	columns := []string{q("id")}
	for _, f := range v.Fields {
		columns = append(columns, q(f.Name))
	}
	columns = append(columns, q("version"), q("created_at"), q("updated_at"))
	list := strings.Join(columns, ", ")
	statements := sqlStatements{Columns: list}

	// INSERT binds org_id right after id.
	{
		var b builder
		names := []string{q("id")}
		values := []string{add(&b, "entity.ID")}
		if v.Tenant {
			names = append(names, q("org_id"))
			values = append(values, add(&b, "orgID"))
		}
		for _, f := range v.Fields {
			names = append(names, q(f.Name))
			values = append(values, add(&b, "entity."+f.Title))
		}
		for _, column := range []struct{ name, expr string }{
			{"version", "entity.Version"}, {"created_at", "entity.CreatedAt"}, {"updated_at", "entity.UpdatedAt"},
		} {
			names = append(names, q(column.name))
			values = append(values, add(&b, column.expr))
		}
		statements.Insert = statement{
			SQL:  fmt.Sprintf("INSERT INTO %s (%s)\nVALUES (%s)", q(v.Plural), strings.Join(names, ", "), strings.Join(values, ", ")),
			Args: b.args,
		}
	}
	{
		var b builder
		where := tenantPredicate(&b) + q("id") + " = " + add(&b, "id")
		statements.Select = statement{SQL: fmt.Sprintf("SELECT %s FROM %s WHERE %s", list, q(v.Plural), where), Args: b.args}
	}
	{
		var b builder
		where := ""
		if v.Tenant {
			where = " WHERE " + q("org_id") + " = " + add(&b, "orgID")
		}
		statements.ListFirst = statement{
			SQL:  fmt.Sprintf("SELECT %s FROM %s%s ORDER BY "+q("id")+" DESC LIMIT %s", list, q(v.Plural), where, add(&b, "limit")),
			Args: b.args,
		}
	}
	{
		var b builder
		where := tenantPredicate(&b) + q("id") + " < " + add(&b, "after")
		statements.ListAfter = statement{
			SQL:  fmt.Sprintf("SELECT %s FROM %s WHERE %s ORDER BY "+q("id")+" DESC LIMIT %s", list, q(v.Plural), where, add(&b, "limit")),
			Args: b.args,
		}
	}
	{
		var b builder
		assignments := make([]string, 0, len(v.Fields)+2)
		for _, f := range v.Fields {
			assignments = append(assignments, q(f.Name)+" = "+add(&b, "entity."+f.Title))
		}
		assignments = append(assignments, q("updated_at")+" = "+add(&b, "entity.UpdatedAt"), q("version")+" = "+q("version")+" + 1")
		where := tenantPredicate(&b) + q("id") + " = " + add(&b, "entity.ID") + " AND " + q("version") + " = " + add(&b, "entity.Version")
		statements.Update = statement{
			SQL:  fmt.Sprintf("UPDATE %s\nSET %s\nWHERE %s", q(v.Plural), strings.Join(assignments, ", "), where),
			Args: b.args,
		}
	}
	{
		var b builder
		where := tenantPredicate(&b) + q("id") + " = " + add(&b, "id") + " AND version = " + add(&b, "version")
		statements.Delete = statement{SQL: fmt.Sprintf("DELETE FROM %s WHERE %s", q(v.Plural), where), Args: b.args}
	}
	{
		var b builder
		where := tenantPredicate(&b) + q("id") + " = " + add(&b, "id")
		statements.Exists = statement{SQL: fmt.Sprintf("SELECT EXISTS (SELECT 1 FROM %s WHERE %s)", q(v.Plural), where), Args: b.args}
	}
	return statements
}

// listByStatements are the first and later pages of the rows of v whose
// column (a references field) names one row, newest first: the nested index
// /posts/{post_id}/comments. They bind the organization (when scoped), the
// referenced id (the Go expression id), then the cursor and the limit.
func listByStatements(v resourceView, column, id string) (first, after statement) {
	q := v.Q
	placeholder := func(n int) string {
		if v.Postgres {
			return fmt.Sprintf("$%d", n)
		}
		return "?"
	}
	build := func(paged bool) statement {
		var args, predicates []string
		add := func(expr string) string {
			args = append(args, expr)
			return placeholder(len(args))
		}
		if v.Tenant {
			predicates = append(predicates, q("org_id")+" = "+add("orgID"))
		}
		predicates = append(predicates, q(column)+" = "+add(id))
		if paged {
			predicates = append(predicates, q("id")+" < "+add("after"))
		}
		return statement{
			SQL:  fmt.Sprintf("SELECT %s FROM %s WHERE %s ORDER BY %s DESC LIMIT %s", v.SQL.Columns, q(v.Plural), strings.Join(predicates, " AND "), q("id"), add("limit")),
			Args: args,
		}
	}
	return build(false), build(true)
}

// column is one line of CREATE TABLE.
type column struct {
	name, definition string
}

// columnDefinition returns the column type and constraints for one field on
// the application's database. Strings are bounded at 255 characters; the
// domain enforces the same bound before the database sees a value.
func columnDefinition(database, table string, f resourceField) string {
	null := " NOT NULL"
	if f.Optional {
		null = ""
	}
	q := func(name string) string { return quoteIdentifier(database, name) }
	switch database {
	case "postgres":
		switch f.Type {
		case "string":
			return fmt.Sprintf("text%s CONSTRAINT %s CHECK (char_length(%s) <= 255)", null, q(table+"_"+f.Name+"_length"), q(f.Name))
		case "text":
			return "text" + null
		case "int":
			return "bigint" + null
		case "bool":
			return "boolean" + null
		case "time":
			return "timestamptz" + null
		default:
			return "uuid" + null
		}
	case "mysql":
		switch f.Type {
		case "string":
			return "VARCHAR(255)" + null
		case "text":
			return "TEXT" + null
		case "int":
			return "BIGINT" + null
		case "bool":
			return "BOOLEAN" + null
		case "time":
			return "DATETIME(6)" + null
		default:
			return "CHAR(36)" + null
		}
	default: // sqlite: STRICT tables allow only INT, INTEGER, REAL, TEXT, BLOB and ANY.
		switch f.Type {
		case "string":
			return fmt.Sprintf("TEXT%s CONSTRAINT %s CHECK (length(%s) <= 255)", null, q(table+"_"+f.Name+"_length"), q(f.Name))
		case "text", "time", "uuid":
			return "TEXT" + null
		case "int":
			return "INTEGER" + null
		default: // bool
			return fmt.Sprintf("INTEGER%s CONSTRAINT %s CHECK (%s IN (0, 1))", null, q(table+"_"+f.Name+"_boolean"), q(f.Name))
		}
	}
}

// renderMigration builds the up and down migrations. The table has the same
// shape on every database: a UUIDv7 key, the user's fields, an optimistic
// locking version and UTC timestamps. Every identifier is quoted, so a field
// or table named like a reserved word works.
func renderMigration(v resourceView) (up, down string) {
	table := v.Plural
	q := v.Q
	var columns []column
	switch {
	case v.Postgres:
		columns = append(columns, column{q("id"), "uuid PRIMARY KEY"})
	case v.MySQL:
		columns = append(columns, column{q("id"), "CHAR(36) NOT NULL PRIMARY KEY"})
	default:
		columns = append(columns, column{q("id"), "TEXT NOT NULL PRIMARY KEY"})
	}
	if v.Tenant {
		switch {
		case v.Postgres:
			columns = append(columns, column{q("org_id"), "uuid NOT NULL REFERENCES forge_tenants (id)"})
		case v.MySQL:
			// forge_tenants.id's type and collation: the foreign key below
			// needs them.
			columns = append(columns, column{q("org_id"), mysqlOrgIDType + " NOT NULL"})
		default:
			columns = append(columns, column{q("org_id"), "TEXT NOT NULL REFERENCES forge_tenants (id)"})
		}
	}
	for _, f := range v.Fields {
		columns = append(columns, column{q(f.Name), columnDefinition(v.Database, table, f.resourceField)})
	}
	versionConstraint := q(table + "_version_positive")
	switch {
	case v.Postgres:
		columns = append(columns,
			column{q("version"), fmt.Sprintf("bigint NOT NULL DEFAULT 1 CONSTRAINT %s CHECK (%s >= 1)", versionConstraint, q("version"))},
			column{q("created_at"), "timestamptz NOT NULL"},
			column{q("updated_at"), "timestamptz NOT NULL"})
	case v.MySQL:
		columns = append(columns,
			column{q("version"), "BIGINT NOT NULL DEFAULT 1"},
			column{q("created_at"), "DATETIME(6) NOT NULL"},
			column{q("updated_at"), "DATETIME(6) NOT NULL"})
	default:
		columns = append(columns,
			column{q("version"), fmt.Sprintf("INTEGER NOT NULL DEFAULT 1 CONSTRAINT %s CHECK (%s >= 1)", versionConstraint, q("version"))},
			column{q("created_at"), "TEXT NOT NULL"},
			column{q("updated_at"), "TEXT NOT NULL"})
	}

	width := 0
	for _, c := range columns {
		width = max(width, len(c.name))
	}
	lines := make([]string, 0, len(columns)+2)
	for _, c := range columns {
		lines = append(lines, fmt.Sprintf("    %-*s %s", width, c.name, c.definition))
	}
	indexName := q(table + "_org_id_idx")
	if v.MySQL {
		lines = append(lines, fmt.Sprintf("    CONSTRAINT %s CHECK (%s >= 1)", versionConstraint, q("version")))
		if v.Tenant {
			lines = append(lines, fmt.Sprintf("    INDEX %s (%s, %s)", indexName, q("org_id"), q("id")),
				fmt.Sprintf("    CONSTRAINT %s FOREIGN KEY (%s) REFERENCES forge_tenants (id)", q(table+"_org_id_fk"), q("org_id")))
		}
	}
	for _, reference := range v.References {
		if v.MySQL {
			lines = append(lines, fmt.Sprintf("    INDEX %s (%s)", q(reference.Index), referenceColumns(v, reference.Field.Name)))
		}
		lines = append(lines, fmt.Sprintf("    CONSTRAINT %s FOREIGN KEY (%s) REFERENCES %s (%s) ON DELETE %s",
			q(reference.Constraint), referenceColumns(v, reference.Field.Name), q(reference.Plural), referenceColumns(v, "id"), onDeleteAction(v.Cascade)))
	}

	var sql strings.Builder
	fmt.Fprintf(&sql, "-- forge:generate %s\n", v.Command)
	fmt.Fprintf(&sql, "-- Creates %s. Ids are UUIDv7 generated by the application", table)
	switch {
	case v.Postgres:
		sql.WriteString(", never by the\n-- database (no DEFAULT: an INSERT without an id fails instead of minting one).\n")
	case v.MySQL:
		sql.WriteString(" and stored as CHAR(36).\n")
	default:
		sql.WriteString(" and stored as TEXT in a STRICT table.\n")
	}
	for _, reference := range v.References {
		if reference.AddsKey {
			fmt.Fprintf(&sql, "-- %s reference %s by (org_id, %s), so no row can name another organization's:\n-- the foreign key needs (org_id, id) declared unique on %s (id alone already is).\n", table, reference.Plural, reference.Field.Name, reference.Plural)
			fmt.Fprintf(&sql, "CREATE UNIQUE INDEX %s ON %s (%s, %s);\n\n", q(uniqueKeyName(reference.Plural)), q(reference.Plural), q("org_id"), q("id"))
		}
	}
	fmt.Fprintf(&sql, "CREATE TABLE %s (\n%s\n)", q(table), strings.Join(lines, ",\n"))
	switch {
	case v.MySQL:
		sql.WriteString(" ENGINE=InnoDB DEFAULT CHARSET=utf8mb4")
	case v.SQLite:
		sql.WriteString(" STRICT")
	}
	sql.WriteString(";\n")
	if v.Tenant && !v.MySQL {
		fmt.Fprintf(&sql, "\nCREATE INDEX %s ON %s (%s, %s);\n", indexName, q(table), q("org_id"), q("id"))
	}
	if len(v.References) > 0 && !v.MySQL {
		sql.WriteString("\n-- The foreign keys' indexes: they serve the referenced rows' nested lists and\n-- the check deleting a referenced row makes.\n")
		for _, reference := range v.References {
			fmt.Fprintf(&sql, "CREATE INDEX %s ON %s (%s);\n", q(reference.Index), q(table), referenceColumns(v, reference.Field.Name))
		}
	}
	if v.Tenant && v.Postgres {
		sql.WriteString(rowSecurity(table, q))
	}
	if v.Identity {
		fmt.Fprintf(&sql, "\n-- Permissions that guard the HTTP routes (the protect argument of Register%s).\n", v.PluralTitle)
		sql.WriteString(grantPermissions(v.Database, table+":read", table+":write"))
	}
	up = sql.String()

	var rollback strings.Builder
	if v.Identity {
		fmt.Fprintf(&rollback, "DELETE FROM forge_role_permissions WHERE permission IN ('%s:read', '%s:write');\n", table, table)
		fmt.Fprintf(&rollback, "DELETE FROM forge_permissions WHERE name IN ('%s:read', '%s:write');\n", table, table)
	}
	fmt.Fprintf(&rollback, "DROP TABLE %s;\n", q(table))
	for _, reference := range v.References {
		if reference.AddsKey {
			if v.MySQL {
				fmt.Fprintf(&rollback, "DROP INDEX %s ON %s;\n", q(uniqueKeyName(reference.Plural)), q(reference.Plural))
			} else {
				fmt.Fprintf(&rollback, "DROP INDEX %s;\n", q(uniqueKeyName(reference.Plural)))
			}
		}
	}
	down = rollback.String()
	return up, down
}

// referenceColumns are the columns of a foreign key from v's column: the
// column alone, or (org_id, column) in an org-scoped table, so a row can only
// reference a row of its own organization.
func referenceColumns(v resourceView, column string) string {
	if v.Tenant {
		return v.Q("org_id") + ", " + v.Q(column)
	}
	return v.Q(column)
}

// onDeleteAction is what deleting a referenced row does to the rows that
// reference it: refused while they exist (RESTRICT), or they go with it.
func onDeleteAction(cascade bool) string {
	if cascade {
		return "CASCADE"
	}
	return "RESTRICT"
}

// rowSecurity is the PostgreSQL row-level security of an org-scoped table:
// the policy confines every statement to the organization InTenantTx set
// (the forge.tenant_id setting), even one without an org_id predicate. FORCE
// applies it to the table's owner too. forge generate tenancy gives a
// converted table the same policy.
func rowSecurity(table string, q func(string) string) string {
	predicate := q("org_id") + " = NULLIF(current_setting('forge.tenant_id', true), '')::uuid"
	var sql strings.Builder
	fmt.Fprintf(&sql, "\n-- Defense in depth: even a query that forgets its org_id predicate sees only\n-- the organization set by InTenantTx. FORCE applies the policy to the table owner too.\n")
	fmt.Fprintf(&sql, "-- Because migrations run as the owner, a later data migration that reads or writes this\n-- table must first set the organization (SELECT set_config('forge.tenant_id', '<org uuid>', true)\n-- inside the migration) or run with a BYPASSRLS maintenance role.\n")
	fmt.Fprintf(&sql, "ALTER TABLE %s ENABLE ROW LEVEL SECURITY;\nALTER TABLE %s FORCE ROW LEVEL SECURITY;\n\n", q(table), q(table))
	fmt.Fprintf(&sql, "CREATE POLICY %s ON %s\n    USING (%s)\n    WITH CHECK (%s);\n", q(table+"_org"), q(table), predicate, predicate)
	return sql.String()
}

// grantPermissions registers permissions (validated resource:action names)
// and grants them to every owner role, idempotently, in database's dialect.
func grantPermissions(database string, permissions ...string) string {
	quoted := make([]string, len(permissions))
	for i, permission := range permissions {
		quoted[i] = "'" + permission + "'"
	}
	// The names as a derived table: SELECT 'a' AS name UNION ALL SELECT 'b'.
	names := "SELECT " + quoted[0] + " AS name"
	for _, name := range quoted[1:] {
		names += " UNION ALL SELECT " + name
	}
	var sql strings.Builder
	switch database {
	case "postgres":
		fmt.Fprintf(&sql, "INSERT INTO forge_permissions (name) VALUES (%s)\nON CONFLICT (name) DO NOTHING;\n", strings.Join(quoted, "), ("))
		sql.WriteString("\n-- The owner role (created by `setup`) holds every permission.\n")
		fmt.Fprintf(&sql, "INSERT INTO forge_role_permissions (role_id, permission)\nSELECT r.id, p.name FROM forge_roles r CROSS JOIN (VALUES (%s)) AS p (name)\nWHERE r.name = 'owner'\nON CONFLICT (role_id, permission) DO NOTHING;\n", strings.Join(quoted, "), ("))
	case "mysql":
		fmt.Fprintf(&sql, "INSERT INTO forge_permissions (name) VALUES (%s)\nON DUPLICATE KEY UPDATE name = name;\n", strings.Join(quoted, "), ("))
		sql.WriteString("\n-- The owner role (created by `setup`) holds every permission.\n")
		fmt.Fprintf(&sql, "INSERT INTO forge_role_permissions (role_id, permission)\nSELECT r.id, p.name FROM forge_roles r CROSS JOIN (%s) AS p\nWHERE r.name = 'owner'\nON DUPLICATE KEY UPDATE permission = forge_role_permissions.permission;\n", names)
	default: // sqlite
		fmt.Fprintf(&sql, "INSERT INTO forge_permissions (name) VALUES (%s)\nON CONFLICT (name) DO NOTHING;\n", strings.Join(quoted, "), ("))
		sql.WriteString("\n-- The owner role (created by `setup`) holds every permission.\n")
		fmt.Fprintf(&sql, "INSERT INTO forge_role_permissions (role_id, permission)\nSELECT r.id, p.name FROM forge_roles r CROSS JOIN (%s) AS p\nWHERE r.name = 'owner'\nON CONFLICT (role_id, permission) DO NOTHING;\n", names)
	}
	return sql.String()
}
