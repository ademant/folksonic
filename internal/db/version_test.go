package db

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func openTemp(t *testing.T) (string, func()) {
	t.Helper()
	return filepath.Join(t.TempDir(), "test.db"), func() {}
}

// A fresh database and one that was stamped before (an old fixture: the
// probe steps run again on the same schema) end at the same version (#115).
func TestFreshAndOldDatabasesReachTheSameVersion(t *testing.T) {
	path, _ := openTemp(t)
	d, err := Init(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	fresh, err := userVersion(d)
	if err != nil {
		t.Fatal(err)
	}
	want := schemaBaseline + len(migrations)
	if fresh != want {
		t.Fatalf("fresh database at version %d, want %d", fresh, want)
	}
	// Simulate a database from before the version existed.
	if _, err := d.Exec("PRAGMA user_version = 0"); err != nil {
		t.Fatal(err)
	}
	if err := migrate(d); err != nil {
		t.Fatalf("migrating an unstamped database: %v", err)
	}
	old, _ := userVersion(d)
	if old != fresh {
		t.Fatalf("old database at version %d, fresh at %d", old, fresh)
	}
}

// A step that fails midway leaves the previous version and none of its
// changes (#115).
func TestFailedMigrationLeavesPreviousVersion(t *testing.T) {
	path, _ := openTemp(t)
	d, err := Init(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	before, _ := userVersion(d)
	steps := []migration{
		{name: "ok", up: func(tx *sql.Tx) error {
			_, err := tx.Exec("CREATE TABLE step_one (x INTEGER)")
			return err
		}},
		{name: "broken", up: func(tx *sql.Tx) error {
			if _, err := tx.Exec("CREATE TABLE step_two (x INTEGER)"); err != nil {
				return err
			}
			return errors.New("interrupted")
		}},
	}
	if err := applyMigrations(d, steps); err == nil {
		t.Fatal("a failing step did not return an error")
	}
	after, _ := userVersion(d)
	if after != before+1 {
		t.Fatalf("version %d after the first step, want %d", after, before+1)
	}
	var n int
	if err := d.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name = 'step_two'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("the failed step left its table behind")
	}
}

// Two transactions that each read and then write run at the same time: both
// succeed, the second waiting for the first (#115).
func TestConcurrentReadThenWriteBothSucceed(t *testing.T) {
	path, _ := openTemp(t)
	d, err := Init(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec("CREATE TABLE tx_probe (x INTEGER)"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tx, err := d.BeginTx(context.Background(), nil)
			if err != nil {
				errs[i] = err
				return
			}
			defer func() { _ = tx.Rollback() }()
			var n int
			if err := tx.QueryRow("SELECT COUNT(*) FROM tx_probe").Scan(&n); err != nil {
				errs[i] = err
				return
			}
			time.Sleep(50 * time.Millisecond)
			if _, err := tx.Exec("INSERT INTO tx_probe (x) VALUES (?)", n); err != nil {
				errs[i] = err
				return
			}
			errs[i] = tx.Commit()
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("transaction %d: %v", i, err)
		}
	}
}

// The configured page cache is applied to pooled connections (#116).
func TestCacheSizeIsApplied(t *testing.T) {
	path, _ := openTemp(t)
	d, err := InitCache(path, 6)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	conn, err := d.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var v int
	if err := conn.QueryRowContext(context.Background(), "PRAGMA cache_size").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != -6*1024 {
		t.Fatalf("cache_size %d, want %d (6 MiB in KiB)", v, -6*1024)
	}
}
