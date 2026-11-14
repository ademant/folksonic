package users

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Devices are paired apps. A device holds a key in its Keystore and signs each
// request with it (DPoP, #101); the server keeps only the public key, so
// there is no bearer secret to steal.
//
// Pairing (#103) works like Bluetooth pairing. The app scans the QR code and
// sends its public key; that makes a pending pairing. Both the app and the
// account page then show the same verification code, which each side derives
// on its own from the pairing token and the key (VerificationCode). The
// device exists only once the owner (on the account page) and the device
// (with a signed request) have both confirmed that the codes match. A QR code
// can collect a few pending pairings, but the first one confirmed on both
// sides consumes it and the others are rejected.

// ErrDeviceKeyInUse is returned when a key is already paired.
var ErrDeviceKeyInUse = errors.New("users: key already paired")

// ErrProofReplayed is returned when a DPoP proof's jti was seen already.
var ErrProofReplayed = errors.New("users: proof already used")

// ErrPairingTokenUsed is returned for a pairing token that a confirmed device
// has consumed, or that the owner invalidated by rejecting a pairing.
var ErrPairingTokenUsed = errors.New("users: pairing token already used")

// ErrPairingRejected is returned for a pairing that was rejected: by the
// owner, by the device, or because another device consumed its token.
var ErrPairingRejected = errors.New("users: pairing rejected")

// ErrTooManyPending is returned when a pairing token already has
// MaxPendingPerToken pairings waiting.
var ErrTooManyPending = errors.New("users: too many pending pairings for this code")

// MaxPendingPerToken bounds how many devices may wait on one QR code.
const MaxPendingPerToken = 3

// ProofWindow is how long a proof's jti is remembered. Proofs older than the
// clock-skew window are refused anyway, so the window only has to cover that.
const ProofWindow = 5 * time.Minute

// pairingKeep is how long a rejected or expired pairing is kept, so its app
// still learns why it failed.
const pairingKeep = 24 * time.Hour

// Pairing states, and the reasons a pairing was rejected.
const (
	PairingPending  = "pending"
	PairingRejected = "rejected"
	PairingExpired  = "expired"

	RejectedByOwner   = "owner"
	RejectedByDevice  = "device"
	RejectedTokenUsed = "token_used"
)

// Device is a paired app.
type Device struct {
	ID         int64
	UserID     int64
	Name       string
	JKT        string
	CreatedAt  string
	LastUsedAt string
}

// PendingPairing is a device waiting for both confirmations.
type PendingPairing struct {
	ID              int64
	Name            string
	CreatedAt       string
	ExpiresAt       string
	Code            string // "" for a pairing from before #103
	ClientIP        string
	OwnerConfirmed  bool
	DeviceConfirmed bool
}

// VerificationCode is the six-digit code the app and the account page both
// show for a pairing: the first four bytes of
// SHA-256(tokenHash ":" jkt), big-endian, modulo 1,000,000, zero-padded.
// tokenHash is the lower-case hex SHA-256 of the pairing token from the QR
// code, and jkt the RFC 7638 thumbprint of the device key. The app computes it
// from the token it scanned and its own key; the server from the stored token
// hash and the key it received. Neither side sends it.
func VerificationCode(tokenHash, jkt string) string {
	sum := sha256.Sum256([]byte(tokenHash + ":" + jkt))
	return fmt.Sprintf("%06d", binary.BigEndian.Uint32(sum[:4])%1_000_000)
}

// CompleteDevicePairing records a device key against a pairing token: a
// pending pairing that expires with the token. Nothing is granted until both
// sides confirm (ConfirmPairingByOwner, ConfirmPairingByDevice). The token
// stays usable for other devices, up to MaxPendingPerToken, until one is
// confirmed.
func CompleteDevicePairing(ctx context.Context, db *sql.DB, plain, name, jkt, clientIP string, now time.Time) (PendingPairing, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return PendingPairing{}, err
	}
	defer tx.Rollback()

	tokenHash := hashToken(plain)
	var userID int64
	var expiresAt string
	var usedAt sql.NullString
	var scope string
	err = tx.QueryRowContext(ctx,
		"SELECT user_id, expires_at, used_at, scope FROM pairing_tokens WHERE token_hash = ?",
		tokenHash).Scan(&userID, &expiresAt, &usedAt, &scope)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && scope != "") {
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
	if usedAt.Valid {
		return PendingPairing{}, ErrPairingTokenUsed
	}
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM device_pairings WHERE expires_at < ?", ts(now.Add(-pairingKeep))); err != nil {
		return PendingPairing{}, err
	}
	var waiting int
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM device_pairings WHERE token_hash = ? AND state = ?",
		tokenHash, PairingPending).Scan(&waiting); err != nil {
		return PendingPairing{}, err
	}
	if waiting >= MaxPendingPerToken {
		return PendingPairing{}, ErrTooManyPending
	}
	if inUse, err := keyPaired(tx, jkt); err != nil {
		return PendingPairing{}, err
	} else if inUse {
		return PendingPairing{}, ErrDeviceKeyInUse
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO device_pairings (user_id, name, jkt, created_at, expires_at, token_hash, client_ip, state)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		userID, name, jkt, ts(now), expiresAt, tokenHash, clientIP, PairingPending)
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
	return PendingPairing{
		ID: id, Name: name, CreatedAt: ts(now), ExpiresAt: expiresAt,
		Code: VerificationCode(tokenHash, jkt), ClientIP: clientIP,
	}, nil
}

// keyPaired reports whether jkt belongs to a device or a pairing.
func keyPaired(q querier, jkt string) (bool, error) {
	var n int
	err := q.QueryRow(
		`SELECT (SELECT COUNT(*) FROM devices WHERE jkt = ?) + (SELECT COUNT(*) FROM device_pairings WHERE jkt = ?)`,
		jkt, jkt).Scan(&n)
	return n > 0, err
}

// PendingPairings lists the account's pairings that wait for confirmation and
// have not expired.
func PendingPairings(ctx context.Context, db *sql.DB, userID int64, now time.Time) ([]PendingPairing, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, name, jkt, created_at, expires_at, COALESCE(token_hash, ''), COALESCE(client_ip, ''),
		        owner_confirmed_at IS NOT NULL, device_confirmed_at IS NOT NULL
		   FROM device_pairings
		  WHERE user_id = ? AND state = ? AND expires_at > ? ORDER BY created_at`,
		userID, PairingPending, ts(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingPairing
	for rows.Next() {
		var p PendingPairing
		var jkt, tokenHash string
		if err := rows.Scan(&p.ID, &p.Name, &jkt, &p.CreatedAt, &p.ExpiresAt, &tokenHash, &p.ClientIP,
			&p.OwnerConfirmed, &p.DeviceConfirmed); err != nil {
			return nil, err
		}
		if tokenHash != "" {
			p.Code = VerificationCode(tokenHash, jkt)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// pairingRow is a pairing as confirmation reads it.
type pairingRow struct {
	id, userID                             int64
	name, jkt, expiresAt, tokenHash, state string
	ownerConfirmed, deviceConfirmed        bool
}

func loadPairing(ctx context.Context, tx *sql.Tx, where string, args ...any) (pairingRow, error) {
	var p pairingRow
	err := tx.QueryRowContext(ctx,
		`SELECT id, user_id, name, jkt, expires_at, COALESCE(token_hash, ''), state,
		        owner_confirmed_at IS NOT NULL, device_confirmed_at IS NOT NULL
		   FROM device_pairings WHERE `+where, args...).
		Scan(&p.id, &p.userID, &p.name, &p.jkt, &p.expiresAt, &p.tokenHash, &p.state,
			&p.ownerConfirmed, &p.deviceConfirmed)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

// checkConfirmable reports why a pairing cannot be confirmed any more, or nil.
func (p pairingRow) checkConfirmable(now time.Time) error {
	if p.state == PairingRejected {
		return ErrPairingRejected
	}
	exp, err := time.Parse(time.RFC3339, p.expiresAt)
	if err != nil {
		return err
	}
	if !now.Before(exp) {
		return ErrPairingExpired
	}
	if p.tokenHash == "" {
		// From before #103: there is no code to compare.
		return ErrPairingRejected
	}
	return nil
}

// ConfirmPairingByOwner records the account owner's answer to a pending
// pairing of their account. match=false rejects it and invalidates its QR
// code, so a rejected device cannot simply scan again; other pairings already
// waiting on that code stay open. It returns the new device when this was the
// second confirmation, and done=false when the device has not confirmed yet.
func ConfirmPairingByOwner(ctx context.Context, db *sql.DB, userID, pendingID int64, match bool, now time.Time) (Device, bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Device{}, false, err
	}
	defer tx.Rollback()
	p, err := loadPairing(ctx, tx, "id = ? AND user_id = ?", pendingID, userID)
	if err != nil {
		return Device{}, false, err
	}
	if p.state == PairingRejected {
		return Device{}, false, ErrNotFound
	}
	if err := p.checkConfirmable(now); err != nil {
		return Device{}, false, err
	}
	if !match {
		if err := rejectPairing(ctx, tx, p.id, RejectedByOwner); err != nil {
			return Device{}, false, err
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE pairing_tokens SET used_at = ? WHERE token_hash = ? AND used_at IS NULL",
			ts(now), p.tokenHash); err != nil {
			return Device{}, false, err
		}
		return Device{}, false, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE device_pairings SET owner_confirmed_at = ? WHERE id = ?", ts(now), p.id); err != nil {
		return Device{}, false, err
	}
	if !p.deviceConfirmed {
		return Device{}, false, tx.Commit()
	}
	dev, err := finishPairing(ctx, tx, p, now)
	if err != nil {
		return Device{}, false, err
	}
	return dev, true, tx.Commit()
}

// ConfirmPairingByDevice records the device's answer for the pairing of its
// key. match=false cancels the pairing; the QR code stays usable for another
// attempt. It returns the new device when this was the second confirmation,
// and done=false when the owner has not confirmed yet.
func ConfirmPairingByDevice(ctx context.Context, db *sql.DB, jkt string, match bool, now time.Time) (Device, bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Device{}, false, err
	}
	defer tx.Rollback()
	p, err := loadPairing(ctx, tx, "jkt = ?", jkt)
	if err != nil {
		return Device{}, false, err
	}
	if err := p.checkConfirmable(now); err != nil {
		return Device{}, false, err
	}
	if !match {
		if err := rejectPairing(ctx, tx, p.id, RejectedByDevice); err != nil {
			return Device{}, false, err
		}
		return Device{}, false, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE device_pairings SET device_confirmed_at = ? WHERE id = ?", ts(now), p.id); err != nil {
		return Device{}, false, err
	}
	if !p.ownerConfirmed {
		return Device{}, false, tx.Commit()
	}
	dev, err := finishPairing(ctx, tx, p, now)
	if err != nil {
		return Device{}, false, err
	}
	return dev, true, tx.Commit()
}

func rejectPairing(ctx context.Context, tx *sql.Tx, id int64, reason string) error {
	_, err := tx.ExecContext(ctx,
		"UPDATE device_pairings SET state = ?, reason = ? WHERE id = ?", PairingRejected, reason, id)
	return err
}

// finishPairing makes the device, consumes the QR code, and rejects every
// other pairing still waiting on it.
func finishPairing(ctx context.Context, tx *sql.Tx, p pairingRow, now time.Time) (Device, error) {
	res, err := tx.ExecContext(ctx,
		"INSERT INTO devices (user_id, name, jkt, created_at) VALUES (?, ?, ?, ?)",
		p.userID, p.name, p.jkt, ts(now))
	if err != nil {
		return Device{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Device{}, err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM device_pairings WHERE id = ?", p.id); err != nil {
		return Device{}, err
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE pairing_tokens SET used_at = ? WHERE token_hash = ? AND used_at IS NULL",
		ts(now), p.tokenHash); err != nil {
		return Device{}, err
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE device_pairings SET state = ?, reason = ? WHERE token_hash = ? AND state = ?",
		PairingRejected, RejectedTokenUsed, p.tokenHash, PairingPending); err != nil {
		return Device{}, err
	}
	return Device{ID: id, UserID: p.userID, Name: p.name, JKT: p.jkt, CreatedAt: ts(now)}, nil
}

// PairingStatus is where a key that is not a device stands in pairing.
type PairingStatus struct {
	State      string // PairingPending, PairingRejected, PairingExpired, or "" if the key has no pairing
	WaitingFor string // while pending: "owner", "device" or "both"
	Reason     string // when rejected: RejectedByOwner, RejectedByDevice or RejectedTokenUsed
}

// DevicePairingState reports where a key stands in pairing.
func DevicePairingState(ctx context.Context, db *sql.DB, jkt string, now time.Time) (PairingStatus, error) {
	var expiresAt, state, reason string
	var owner, device bool
	err := db.QueryRowContext(ctx,
		`SELECT expires_at, state, COALESCE(reason, ''),
		        owner_confirmed_at IS NOT NULL, device_confirmed_at IS NOT NULL
		   FROM device_pairings WHERE jkt = ?`, jkt).Scan(&expiresAt, &state, &reason, &owner, &device)
	if errors.Is(err, sql.ErrNoRows) {
		return PairingStatus{}, nil
	}
	if err != nil {
		return PairingStatus{}, err
	}
	if state == PairingRejected {
		return PairingStatus{State: PairingRejected, Reason: reason}, nil
	}
	exp, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		return PairingStatus{}, err
	}
	if !now.Before(exp) {
		return PairingStatus{State: PairingExpired}, nil
	}
	waiting := "both"
	switch {
	case owner && !device:
		waiting = "device"
	case device && !owner:
		waiting = "owner"
	}
	return PairingStatus{State: PairingPending, WaitingFor: waiting}, nil
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

// ProofReplay remembers the proofs seen from known keys within ProofWindow, so
// a captured proof cannot be sent twice (#111). It lives in memory: a restart
// clears it, which leaves only the ProofWindow after a restart open to replay.
// The zero value is ready to use. Callers check a key is a known device or a
// pending pairing first, so unknown keys cannot fill it.
type ProofReplay struct {
	mu    sync.Mutex
	seen  map[string]time.Time
	prune time.Time
}

// proofPruneEvery is how often old entries are dropped from ProofReplay.
const proofPruneEvery = time.Minute

// Len reports how many proofs are remembered.
func (p *ProofReplay) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.seen)
}

// Check records the proof jti from the key jkt and returns ErrProofReplayed
// when the same pair was recorded within ProofWindow.
func (p *ProofReplay) Check(jkt, jti string, now time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.seen == nil {
		p.seen = make(map[string]time.Time)
	}
	if now.Sub(p.prune) >= proofPruneEvery {
		for k, at := range p.seen {
			if now.Sub(at) > ProofWindow {
				delete(p.seen, k)
			}
		}
		p.prune = now
	}
	key := jkt + " " + jti
	if at, ok := p.seen[key]; ok && now.Sub(at) <= ProofWindow {
		return ErrProofReplayed
	}
	p.seen[key] = now
	return nil
}
