// Package users manages accounts, API keys, device pairing tokens and login
// sessions. Everything is stored in the library database.
package users

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

const (
	RoleAdmin = "admin"
	RoleUser  = "user"

	StatusPending  = "pending"
	StatusActive   = "active"
	StatusDisabled = "disabled"
)

var (
	ErrNotFound = errors.New("not found")
	ErrExists   = errors.New("username already exists")
	ErrInvalid  = errors.New("invalid input")
)

// User is one account. Password is stored as given because Subsonic token
// auth (md5(password+salt)) requires the server to know it.
type User struct {
	ID        int64
	Username  string
	Password  string
	Role      string
	Status    string
	CreatedAt string
}

// querier is satisfied by *sql.DB and *sql.Tx, so token redemption can run
// inside one transaction.
type querier interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
}

// Now returns the timestamp format used throughout the users tables. The
// fixed-width UTC layout makes string comparison match time order.
func Now() string { return time.Now().UTC().Format("2006-01-02T15:04:05Z") }

func ts(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

// ValidUsername accepts 1–64 characters without whitespace or control chars.
func ValidUsername(name string) error {
	if name == "" || len(name) > 64 {
		return fmt.Errorf("%w: username must be 1-64 characters", ErrInvalid)
	}
	for _, r := range name {
		if r <= ' ' || r == 0x7f {
			return fmt.Errorf("%w: username may not contain spaces or control characters", ErrInvalid)
		}
	}
	return nil
}

// Create inserts a user. Status and role are set by the caller: admin
// creation uses active/admin, self-registration uses pending/user.
func Create(ctx context.Context, db *sql.DB, username, password, role, status string) (User, error) {
	if err := ValidUsername(username); err != nil {
		return User{}, err
	}
	if password == "" {
		return User{}, fmt.Errorf("%w: password must not be empty", ErrInvalid)
	}
	if _, err := ByName(ctx, db, username); err == nil {
		return User{}, ErrExists
	}
	res, err := db.ExecContext(ctx,
		`INSERT INTO users (username, password, role, status, created_at) VALUES (?, ?, ?, ?, ?)`,
		username, password, role, status, Now())
	if err != nil {
		return User{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return User{}, err
	}
	return ByID(ctx, db, id)
}

const userCols = "id, username, password, role, status, created_at"

func scanUser(s interface{ Scan(...any) error }) (User, error) {
	var u User
	err := s.Scan(&u.ID, &u.Username, &u.Password, &u.Role, &u.Status, &u.CreatedAt)
	return u, err
}

func ByName(ctx context.Context, db *sql.DB, name string) (User, error) {
	u, err := scanUser(db.QueryRowContext(ctx, "SELECT "+userCols+" FROM users WHERE username = ?", name))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

func ByID(ctx context.Context, db *sql.DB, id int64) (User, error) {
	u, err := scanUser(db.QueryRowContext(ctx, "SELECT "+userCols+" FROM users WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

func List(ctx context.Context, db *sql.DB) ([]User, error) {
	rows, err := db.QueryContext(ctx, "SELECT "+userCols+" FROM users ORDER BY username COLLATE NOCASE")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// Count returns the number of accounts in any status.
func Count(ctx context.Context, db *sql.DB) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM users").Scan(&n)
	return n, err
}

// SetStatus changes an account's status. approvedBy is recorded when moving
// an account to active; pass 0 for the bootstrap or CLI path.
func SetStatus(ctx context.Context, db *sql.DB, id int64, status string, approvedBy int64) error {
	var by any
	if approvedBy > 0 {
		by = approvedBy
	}
	res, err := db.ExecContext(ctx,
		"UPDATE users SET status = ?, approved_by = COALESCE(?, approved_by) WHERE id = ?",
		status, by, id)
	if err != nil {
		return err
	}
	return requireOne(res)
}

func SetPassword(ctx context.Context, db *sql.DB, id int64, password string) error {
	if password == "" {
		return fmt.Errorf("%w: password must not be empty", ErrInvalid)
	}
	res, err := db.ExecContext(ctx, "UPDATE users SET password = ? WHERE id = ?", password, id)
	if err != nil {
		return err
	}
	return requireOne(res)
}

// Delete removes an account. Its API keys, sessions and pairing tokens go
// with it through ON DELETE CASCADE.
func Delete(ctx context.Context, db *sql.DB, id int64) error {
	res, err := db.ExecContext(ctx, "DELETE FROM users WHERE id = ?", id)
	if err != nil {
		return err
	}
	return requireOne(res)
}

func requireOne(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Bootstrap creates the first admin from the config file when the database
// has no users yet. It returns true when an account was created.
func Bootstrap(ctx context.Context, db *sql.DB, username, password string) (bool, error) {
	if username == "" {
		return false, nil
	}
	n, err := Count(ctx, db)
	if err != nil || n > 0 {
		return false, err
	}
	if _, err := Create(ctx, db, username, password, RoleAdmin, StatusActive); err != nil {
		return false, err
	}
	return true, nil
}

// randomToken returns a URL-safe random string with the given prefix.
func randomToken(prefix string) (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func hashToken(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
