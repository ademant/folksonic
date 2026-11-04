package db

import (
	"database/sql"
	"fmt"
	"log"

	"github.com/tsirysndr/tinysonic/internal/passwords"
)

// migratePasswordHash moves stored passwords from the plaintext column to
// users.password_hash. Each plaintext value is hashed and then cleared in the
// same statement, so a password is never left in plaintext after the step. It
// is idempotent: a row with no plaintext is left alone.
func migratePasswordHash(d *sql.DB) error {
	found, _, err := tableColumn(d, "users", "password_hash")
	if err != nil {
		return err
	}
	if !found {
		if _, err := d.Exec("ALTER TABLE users ADD COLUMN password_hash TEXT"); err != nil {
			return err
		}
	}

	type pending struct {
		id       int64
		password string
	}
	rows, err := d.Query("SELECT id, password FROM users WHERE password IS NOT NULL AND password <> ''")
	if err != nil {
		return err
	}
	var todo []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.password); err != nil {
			rows.Close()
			return err
		}
		todo = append(todo, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, p := range todo {
		hash, err := passwords.Hash(p.password)
		if err != nil {
			return fmt.Errorf("hashing password of user %d: %w", p.id, err)
		}
		// The password guard stops a concurrent change from being overwritten.
		if _, err := d.Exec(
			"UPDATE users SET password_hash = ?, password = NULL WHERE id = ? AND password = ?",
			hash, p.id, p.password); err != nil {
			return err
		}
	}
	if len(todo) > 0 {
		log.Printf("passwords: hashed %d stored password(s) and removed the plaintext", len(todo))
	}
	return nil
}
