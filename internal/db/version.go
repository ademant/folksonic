package db

import (
	"database/sql"
	"fmt"
)

// schemaBaseline is the user_version stamped once the probe-based steps in
// migrateLegacy have run. Databases below it run those steps, databases at or
// above it skip them.
const schemaBaseline = 1

// migration is one numbered schema step. It runs in one transaction with its
// user_version bump, so a failed step leaves the previous version and no partial
// change (#115). A step that cannot run inside a transaction must be idempotent
// and say so here.
type migration struct {
	name string
	up   func(tx *sql.Tx) error
}

// migrations are the numbered steps after the baseline, applied in order. Add
// new ones at the end; never change or reorder one that has shipped.
var migrations []migration

// userVersion reads PRAGMA user_version, which is 0 on a database that has not
// been stamped.
func userVersion(d *sql.DB) (int, error) {
	var v int
	err := d.QueryRow("PRAGMA user_version").Scan(&v)
	return v, err
}

// applyMigrations stamps the baseline when it is missing, then runs each step
// whose version is above the current one.
func applyMigrations(d *sql.DB, steps []migration) error {
	cur, err := userVersion(d)
	if err != nil {
		return err
	}
	if cur < schemaBaseline {
		if _, err := d.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaBaseline)); err != nil {
			return fmt.Errorf("stamping schema baseline: %w", err)
		}
		cur = schemaBaseline
	}
	for i, m := range steps {
		want := schemaBaseline + i + 1
		if cur >= want {
			continue
		}
		if err := runMigration(d, m, want); err != nil {
			return fmt.Errorf("migration %d (%s): %w", want, m.name, err)
		}
		cur = want
	}
	return nil
}

// runMigration applies one step and its version bump in a single transaction.
func runMigration(d *sql.DB, m migration, version int) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := m.up(tx); err != nil {
		return err
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		return err
	}
	return tx.Commit()
}
