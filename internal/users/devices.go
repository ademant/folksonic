package users

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Devices are paired apps. A device holds a key in its Keystore and signs each
// request with it (DPoP, #101); the server keeps only the public key, so
// there is no bearer secret to steal. Pairing makes a pending entry that the
// account owner approves on the account page, within the pairing token's
// lifetime.

// ErrDeviceKeyInUse is returned when a key is already paired.
var ErrDeviceKeyInUse = errors.New("users: key already paired")

// ErrProofReplayed is returned when a DPoP proof's jti was seen already.
var ErrProofReplayed = errors.New("users: proof already used")

// ProofWindow is how long a proof's jti is remembered. Proofs older than the
// clock-skew window are refused anyway, so the window only has to cover that.
const ProofWindow = 5 * time.Minute

// Device is a paired app.
type Device struct {
	ID         int64
	UserID     int64
	Name       string
	JKT        string
	CreatedAt  string
	LastUsedAt string
}

// PendingPairing is a paired-but-not-approved device.
type PendingPairing struct {
	ID        int64
	Name      string
	CreatedAt string
	ExpiresAt string
}

// CompleteDevicePairing redeems a pairing token for a device key. It records a
// pending pairing that expires with the token; nothing is granted until
// ApproveDevicePairing. The token is single use.
func CompleteDevicePairing(ctx context.Context, db *sql.DB, plain, name, jkt, jwkJSON string, now time.Time) (PendingPairing, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return PendingPairing{}, err
	}
	defer tx.Rollback()

	var userID int64
	var expiresAt string
	var usedAt sql.NullString
	var scope string
	err = tx.QueryRowContext(ctx,
		"SELECT user_id, expires_at, used_at, scope FROM pairing_tokens WHERE token_hash = ?",
		hashToken(plain)).Scan(&userID, &expiresAt, &usedAt, &scope)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (usedAt.Valid || scope != "")) {
		return PendingPairing{}, ErrNotFound
	}
	if err != nil {
		return PendingPairing{}, err
	}
	exp, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		return PendingPairing{}, err
	}
	if !now.Before(exp) {
		return PendingPairing{}, ErrPairingExpired
	}
	res, err := tx.ExecContext(ctx,
		"UPDATE pairing_tokens SET used_at = ? WHERE token_hash = ? AND used_at IS NULL",
		ts(now), hashToken(plain))
	if err != nil {
		return PendingPairing{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return PendingPairing{}, ErrNotFound
	}

	if inUse, err := keyPaired(tx, jkt); err != nil {
		return PendingPairing{}, err
	} else if inUse {
		return PendingPairing{}, ErrDeviceKeyInUse
	}
	res, err = tx.ExecContext(ctx,
		`INSERT INTO device_pairings (user_id, name, jkt, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?)`,
		userID, name, jkt, ts(now), expiresAt)
	if err != nil {
		return PendingPairing{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return PendingPairing{}, err
	}
	if err := tx.Commit(); err != nil {
		return PendingPairing{}, err
	}
	return PendingPairing{ID: id, Name: name, CreatedAt: ts(now), ExpiresAt: expiresAt}, nil
}

// keyPaired reports whether jkt belongs to a device or a pending pairing.
func keyPaired(q querier, jkt string) (bool, error) {
	var n int
	err := q.QueryRow(
		`SELECT (SELECT COUNT(*) FROM devices WHERE jkt = ?) + (SELECT COUNT(*) FROM device_pairings WHERE jkt = ?)`,
		jkt, jkt).Scan(&n)
	return n > 0, err
}

// PendingPairings lists the account's pairings not yet approved and not expired.
func PendingPairings(ctx context.Context, db *sql.DB, userID int64, now time.Time) ([]PendingPairing, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, name, created_at, expires_at FROM device_pairings
		 WHERE user_id = ? AND expires_at > ? ORDER BY created_at`,
		userID, ts(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingPairing
	for rows.Next() {
		var p PendingPairing
		if err := rows.Scan(&p.ID, &p.Name, &p.CreatedAt, &p.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ApproveDevicePairing turns a pending pairing into a device. It fails if the
// pairing is gone or its time has run out.
func ApproveDevicePairing(ctx context.Context, db *sql.DB, userID, pendingID int64, now time.Time) (Device, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Device{}, err
	}
	defer tx.Rollback()

	var name, jkt, expiresAt string
	err = tx.QueryRowContext(ctx,
		"SELECT name, jkt, expires_at FROM device_pairings WHERE id = ? AND user_id = ?",
		pendingID, userID).Scan(&name, &jkt, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, ErrNotFound
	}
	if err != nil {
		return Device{}, err
	}
	exp, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		return Device{}, err
	}
	if !now.Before(exp) {
		if _, err := tx.ExecContext(ctx, "DELETE FROM device_pairings WHERE id = ?", pendingID); err != nil {
			return Device{}, err
		}
		if err := tx.Commit(); err != nil {
			return Device{}, err
		}
		return Device{}, ErrPairingExpired
	}
	res, err := tx.ExecContext(ctx,
		"INSERT INTO devices (user_id, name, jkt, created_at) VALUES (?, ?, ?, ?)",
		userID, name, jkt, ts(now))
	if err != nil {
		return Device{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Device{}, err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM device_pairings WHERE id = ?", pendingID); err != nil {
		return Device{}, err
	}
	if err := tx.Commit(); err != nil {
		return Device{}, err
	}
	return Device{ID: id, UserID: userID, Name: name, JKT: jkt, CreatedAt: ts(now)}, nil
}

// DenyDevicePairing discards a pending pairing.
func DenyDevicePairing(ctx context.Context, db *sql.DB, userID, pendingID int64) error {
	res, err := db.ExecContext(ctx,
		"DELETE FROM device_pairings WHERE id = ? AND user_id = ?", pendingID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrNotFound
	}
	return nil
}

// DevicePairingState reports what a key is doing with pairing: "pending" while
// its pairing awaits approval, "expired" once that has run out, or "" if the
// key is not pending at all.
func DevicePairingState(ctx context.Context, db *sql.DB, jkt string, now time.Time) (string, error) {
	var expiresAt string
	err := db.QueryRowContext(ctx,
		"SELECT expires_at FROM device_pairings WHERE jkt = ?", jkt).Scan(&expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	exp, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		return "", err
	}
	if now.Before(exp) {
		return "pending", nil
	}
	return "expired", nil
}

// DeviceByKey returns the device and its active owner for a key thumbprint, and
// records the use. last_used_at is written at most once a minute.
func DeviceByKey(ctx context.Context, db *sql.DB, jkt string, now time.Time) (Device, User, error) {
	var d Device
	var lastUsed sql.NullString
	err := db.QueryRowContext(ctx,
		"SELECT id, user_id, name, jkt, created_at, last_used_at FROM devices WHERE jkt = ?", jkt).
		Scan(&d.ID, &d.UserID, &d.Name, &d.JKT, &d.CreatedAt, &lastUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, User{}, ErrNotFound
	}
	if err != nil {
		return Device{}, User{}, err
	}
	u, err := ByID(ctx, db, d.UserID)
	if err != nil {
		return Device{}, User{}, err
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE devices SET last_used_at = ? WHERE id = ? AND (last_used_at IS NULL OR last_used_at < ?)`,
		ts(now), d.ID, ts(now.Add(-time.Minute))); err != nil {
		return Device{}, User{}, err
	}
	d.LastUsedAt = ts(now)
	return d, u, nil
}

// Devices lists the account's paired devices.
func Devices(ctx context.Context, db *sql.DB, userID int64) ([]Device, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, name, jkt, created_at, COALESCE(last_used_at, '') FROM devices
		 WHERE user_id = ? ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Device
	for rows.Next() {
		d := Device{UserID: userID}
		if err := rows.Scan(&d.ID, &d.Name, &d.JKT, &d.CreatedAt, &d.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// RemoveDevice deletes one of the account's devices. Its proofs fail from the
// next request on.
func RemoveDevice(ctx context.Context, db *sql.DB, userID, deviceID int64) error {
	res, err := db.ExecContext(ctx,
		"DELETE FROM devices WHERE id = ? AND user_id = ?", deviceID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrNotFound
	}
	return nil
}

// RecordProofJTI remembers a proof's jti for ProofWindow and refuses one seen
// before. Old entries are pruned on the way.
func RecordProofJTI(ctx context.Context, db *sql.DB, jti string, now time.Time) error {
	if _, err := db.ExecContext(ctx,
		"DELETE FROM dpop_proofs WHERE seen_at < ?", ts(now.Add(-ProofWindow))); err != nil {
		return err
	}
	res, err := db.ExecContext(ctx,
		"INSERT INTO dpop_proofs (jti, seen_at) VALUES (?, ?) ON CONFLICT (jti) DO NOTHING",
		jti, ts(now))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrProofReplayed
	}
	return nil
}
