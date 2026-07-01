// Package gowaymigrator provides a goway-backed sqlbus.Migrator. Importing
// this package is the single action that pulls github.com/cgardev/goway into a
// build; the sqlbus core itself depends only on database/sql.
package gowaymigrator

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"

	"github.com/cgardev/gokeel/sqlbus"
	"github.com/cgardev/goway"
)

type migrator struct{}

var _ sqlbus.Migrator = migrator{}

// New returns a sqlbus.Migrator backed by goway. It applies the sqlbus-owned
// embedded schema as a Flyway-style versioned migration, recording history in
// the sqlbus.SchemaHistoryTable table. Pass the result to sqlbus.WithMigrator:
//
//	store := sqlbus.NewPostgresStore(db, sqlbus.WithMigrator(gowaymigrator.New()))
func New() sqlbus.Migrator { return migrator{} }

// Migrate maps the core dialect onto goway's sealed Dialect and runs the
// embedded schema through goway. The schema fs.FS is supplied by the store
// (sqlbus.Schema()), so the SQL is never duplicated here.
func (migrator) Migrate(
	ctx context.Context, db *sql.DB, dialect sqlbus.Dialect, schema fs.FS,
) error {
	gowayDialect, err := toGowayDialect(dialect)
	if err != nil {
		return err
	}
	migrations, err := goway.Configure().
		DataSource(db).
		Dialect(gowayDialect).
		Table(sqlbus.SchemaHistoryTable).
		FS(schema). // schema is already rooted at the migration directory; paths default to "."
		Load()
	if err != nil {
		return fmt.Errorf("configure migrations: %w", err)
	}
	if _, err := migrations.Migrate(ctx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

// toGowayDialect maps the core string enum to goway's sealed Dialect. No goway
// type ever crosses into the sqlbus core, and no sqlbus type ever crosses into
// goway.
func toGowayDialect(d sqlbus.Dialect) (goway.Dialect, error) {
	switch d {
	case sqlbus.DialectSQLite:
		return goway.SQLite(), nil
	case sqlbus.DialectPostgres:
		return goway.Postgres(), nil
	default:
		return nil, fmt.Errorf("gowaymigrator: unsupported dialect %q", d)
	}
}
