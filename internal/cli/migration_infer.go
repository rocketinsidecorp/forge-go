package cli

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// `forge generate migration NAME [field:type...]` infers the statements from
// the name, as Rails does: add_COLUMNS_to_TABLE with fields adds them,
// remove_COLUMNS_from_TABLE with fields drops them (and its down adds them
// back). Any other name, or no fields, gets the blank pair to fill in, and a
// warning, since the migrator refuses a migration with no statements.

var (
	addColumnsName    = regexp.MustCompile(`^add_[a-z0-9_]+?_to_([a-z][a-z0-9_]*)$`)
	removeColumnsName = regexp.MustCompile(`^remove_[a-z0-9_]+?_from_([a-z][a-z0-9_]*)$`)
)

// columnChange is what a migration name and its fields say to do.
type columnChange struct {
	table  string
	remove bool
	fields []resourceField
}

// inferColumnChange reads the name and fields. ok is false when the name
// infers nothing; fields with such a name are an error.
func inferColumnChange(name string, fields []resourceField) (columnChange, bool, error) {
	change := columnChange{fields: fields}
	if match := addColumnsName.FindStringSubmatch(name); match != nil {
		change.table = match[1]
	} else if match := removeColumnsName.FindStringSubmatch(name); match != nil {
		change.table, change.remove = match[1], true
	}
	switch {
	case change.table == "" && len(fields) > 0:
		return columnChange{}, false, fmt.Errorf("fields need a migration named add_COLUMNS_to_TABLE or remove_COLUMNS_from_TABLE (for example add_sku_to_products sku:string); for a new table use `forge generate model`")
	case change.table == "" || len(fields) == 0:
		return columnChange{}, false, nil
	case strings.HasPrefix(change.table, "forge_") || strings.HasPrefix(change.table, "sqlite_"):
		return columnChange{}, false, fmt.Errorf("table %q uses a prefix reserved for Forge and SQLite internals", change.table)
	}
	if err := validateIdentifierLengths(resourceRequest{Plural: change.table, Fields: fields}); err != nil {
		return columnChange{}, false, err
	}
	return change, true, nil
}

// addColumnDefinition is columnDefinition for ALTER TABLE ADD COLUMN: rows
// that already exist need a value, so a required column gets the type's
// zero value as its default, and a required time or uuid column, which has
// none, is refused.
func addColumnDefinition(database, table string, field resourceField) (string, error) {
	definition := columnDefinition(database, table, field)
	if field.Optional {
		return definition, nil
	}
	var value string
	switch field.Type {
	case "string", "text":
		value = "''"
		if database == "mysql" && field.Type == "text" {
			value = "('')" // MySQL takes a TEXT default only as an expression
		}
	case "int":
		value = "0"
	case "bool":
		value = "FALSE"
		if database == "sqlite" {
			value = "0"
		}
	default:
		return "", fmt.Errorf("field %q: existing rows need a value, and a required %s column has no default; declare it optional (%s:%s?) and fill it in a later migration", field.Name, field.Type, field.Name, field.Type)
	}
	// The default goes right after NOT NULL, before any CHECK constraint.
	return strings.Replace(definition, " NOT NULL", " NOT NULL DEFAULT "+value, 1), nil
}

// renderColumnMigration writes the up and down statements of change for
// database (postgres, mysql or sqlite).
func renderColumnMigration(database, name string, change columnChange) (up, down string, err error) {
	q := func(identifier string) string { return quoteIdentifier(database, identifier) }
	var adds, drops strings.Builder
	for _, field := range change.fields {
		definition, err := addColumnDefinition(database, change.table, field)
		if err != nil {
			return "", "", err
		}
		fmt.Fprintf(&adds, "ALTER TABLE %s ADD COLUMN %s %s;\n", q(change.table), q(field.Name), definition)
		fmt.Fprintf(&drops, "ALTER TABLE %s DROP COLUMN %s;\n", q(change.table), q(field.Name))
	}
	if change.remove {
		return fmt.Sprintf("-- %s\n%s", name, drops.String()), fmt.Sprintf("-- Reverts %s: the columns come back, their values do not.\n%s", name, adds.String()), nil
	}
	return fmt.Sprintf("-- %s\n%s", name, adds.String()), fmt.Sprintf("-- Reverts %s.\n%s", name, drops.String()), nil
}

// removeColumnsWarning is printed after a remove_COLUMNS_from_TABLE
// migration: running it destroys data that its down migration cannot bring
// back.
func removeColumnsWarning(change columnChange) string {
	names := make([]string, 0, len(change.fields))
	for _, field := range change.fields {
		names = append(names, field.Name)
	}
	return fmt.Sprintf("\n  WARNING: this migration drops %s from %s: their data is lost when it runs, and rolling it back restores the columns (empty, or with their type's default), not the values. Back up the table first.\n",
		strings.Join(names, ", "), change.table)
}

// blankMigrationWarning is printed after a migration with no statements.
func blankMigrationWarning(up string) string {
	return fmt.Sprintf("\n  WARNING: fill in %s and %s before forge migrate or forge test; an empty migration is refused.\n  (add_COLUMNS_to_TABLE and remove_COLUMNS_from_TABLE with field:type arguments write the statements.)\n",
		up, strings.TrimSuffix(up, ".up.sql")+".down.sql")
}

var errMigrationDatabase = errors.New("the migration's statements depend on the database, and FORGE_DATABASE_URL does not select one (postgres, mysql or sqlite)")
