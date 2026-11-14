package users

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tsirysndr/tinysonic/internal/passwords"
)

// InviteTTL is how long an invitation code stays valid unless the admin
// chooses otherwise.
const InviteTTL = 7 * 24 * time.Hour

// ErrInviteInvalid covers unknown, used, revoked and expired codes alike, so
// a caller cannot tell which codes exist.
var ErrInviteInvalid = errors.New("invalid or expired invitation code")

// Invite is a stored invitation. The code itself is only returned once, at
// creation; the table keeps its hash.
type Invite struct {
	ID        int64
	Role      string
	Note      string
	CreatedAt string
	ExpiresAt string
	UsedAt    sql.NullString
	UsedBy    sql.NullString // username of the account it created
	RevokedAt sql.NullString
	Username  sql.NullString // when set, the only username the code can create
}

// State describes the invite for listings: open, used, revoked or expired.
func (i Invite) State() string {
	switch {
	case i.UsedAt.Valid:
		return "used"
	case i.RevokedAt.Valid:
		return "revoked"
	case i.ExpiresAt <= Now():
		return "expired"
	}
	return "open"
}

// inviteBodyLen is the number of base32 characters in a code: 10 random bytes,
// 80 bits.
const inviteBodyLen = 16

// newInviteCode returns a code formatted as inv-XXXX-XXXX-XXXX-XXXX.
func newInviteCode() (string, error) {
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	body := base32.StdEncoding.EncodeToString(b[:])
	var sb strings.Builder
	sb.WriteString("inv")
	for i := 0; i < len(body); i += 4 {
		sb.WriteByte('-')
		sb.WriteString(body[i : i+4])
	}
	return sb.String(), nil
}

// normaliseInvite reduces a code as typed to its 16-character body. Case,
// dashes, spaces and the inv prefix are optional.
func normaliseInvite(code string) (string, bool) {
	s := strings.ToUpper(code)
	s = strings.NewReplacer("-", "", " ", "").Replace(s)
	if len(s) == inviteBodyLen+3 && strings.HasPrefix(s, "INV") {
		s = s[3:]
	}
	if len(s) != inviteBodyLen {
		return "", false
	}
	for _, r := range s {
		if !(r >= 'A' && r <= 'Z' || r >= '2' && r <= '7') {
			return "", false
		}
	}
	return s, true
}

// CreateInvite issues a single-use invitation code for an account with the
// given role. A non-empty username binds the code to that name, which is how
// an admin provisions a passkey-only account. It returns the plaintext code and
// the stored invite.
func CreateInvite(ctx context.Context, db *sql.DB, role, username, note string, ttl time.Duration) (string, Invite, error) {
	if role != RoleUser && role != RoleLibrarian {
		return "", Invite{}, fmt.Errorf("%w: unknown role %q", ErrInvalid, role)
	}
	if username != "" {
		if err := ValidUsername(username); err != nil {
			return "", Invite{}, err
		}
	}
	if ttl <= 0 {
		return "", Invite{}, fmt.Errorf("%w: expiry must be positive", ErrInvalid)
	}
	code, err := newInviteCode()
	if err != nil {
		return "", Invite{}, err
	}
	body, _ := normaliseInvite(code)
	res, err := db.ExecContext(ctx,
		"INSERT INTO invites (code_hash, role, note, created_at, expires_at, username) VALUES (?, ?, ?, ?, ?, ?)",
		hashToken(body), role, note, Now(), ts(time.Now().Add(ttl)), sql.NullString{String: username, Valid: username != ""})
	if err != nil {
		return "", Invite{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return "", Invite{}, err
	}
	inv, err := inviteByID(ctx, db, id)
	return code, inv, err
}

const inviteCols = `i.id, i.role, i.note, i.created_at, i.expires_at, i.used_at, u.username, i.revoked_at, i.username
	FROM invites i LEFT JOIN users u ON u.id = i.used_by`

func scanInvite(s interface{ Scan(...any) error }) (Invite, error) {
	var i Invite
	err := s.Scan(&i.ID, &i.Role, &i.Note, &i.CreatedAt, &i.ExpiresAt, &i.UsedAt, &i.UsedBy, &i.RevokedAt, &i.Username)
	return i, err
}

func inviteByID(ctx context.Context, db *sql.DB, id int64) (Invite, error) {
	i, err := scanInvite(db.QueryRowContext(ctx, "SELECT "+inviteCols+" WHERE i.id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Invite{}, ErrNotFound
	}
	return i, err
}

// ListInvites returns every invite, newest first.
func ListInvites(ctx context.Context, db *sql.DB) ([]Invite, error) {
	rows, err := db.QueryContext(ctx, "SELECT "+inviteCols+" ORDER BY i.id DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Invite
	for rows.Next() {
		i, err := scanInvite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// RevokeInvite withdraws an unused invite. Used or already revoked invites
// report ErrNotFound.
func RevokeInvite(ctx context.Context, db *sql.DB, id int64) error {
	res, err := db.ExecContext(ctx,
		"UPDATE invites SET revoked_at = ? WHERE id = ? AND used_at IS NULL AND revoked_at IS NULL",
		Now(), id)
	if err != nil {
		return err
	}
	return requireOne(res)
}

// RegisterWithInvite creates an active account with the invite's role and
// consumes the invite in the same transaction. Claiming the invite is the
// transaction's first statement, so a concurrent second registration with
// the same code waits for the write lock and then finds it used.
func RegisterWithInvite(ctx context.Context, db *sql.DB, code, username, password string) (User, error) {
	if err := ValidUsername(username); err != nil {
		return User{}, err
	}
	if err := CheckPassword(password); err != nil {
		return User{}, err
	}
	hash, err := passwords.Hash(password)
	if err != nil {
		return User{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	body, ok := normaliseInvite(code)
	if !ok {
		return User{}, ErrInviteInvalid
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()

	now := Now()
	var inviteID int64
	var role string
	err = tx.QueryRowContext(ctx,
		`UPDATE invites SET used_at = ?
		 WHERE code_hash = ? AND used_at IS NULL AND revoked_at IS NULL AND expires_at > ?
		   AND (username IS NULL OR username = ? COLLATE NOCASE)
		 RETURNING id, role`,
		now, hashToken(body), now, username).Scan(&inviteID, &role)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, inviteRefusal(ctx, db, body, now)
	}
	if err != nil {
		return User{}, err
	}

	// Checked after the invite, so only invitees learn which names are taken.
	var taken int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM users WHERE username = ?", username).Scan(&taken); err != nil {
		return User{}, err
	}
	if taken > 0 {
		return User{}, ErrExists
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO users (username, password_hash, role, status, created_at) VALUES (?, ?, ?, ?, ?)`,
		username, hash, role, StatusActive, now)
	if err != nil {
		return User{}, err
	}
	userID, err := res.LastInsertId()
	if err != nil {
		return User{}, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE invites SET used_by = ? WHERE id = ?", userID, inviteID); err != nil {
		return User{}, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	return ByID(ctx, db, userID)
}

// ErrInviteExpired is returned for a code past its time limit and never used.
// It matches ErrInviteInvalid.
var ErrInviteExpired = fmt.Errorf("%w: expired", ErrInviteInvalid)

// inviteRefusal says why a code was not redeemed: expired, or invalid for any
// other reason (unknown, used, revoked or for another name).
func inviteRefusal(ctx context.Context, db *sql.DB, body string, now string) error {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM invites
		 WHERE code_hash = ? AND used_at IS NULL AND revoked_at IS NULL AND expires_at <= ?`,
		hashToken(body), now).Scan(&n)
	if err == nil && n > 0 {
		return ErrInviteExpired
	}
	return ErrInviteInvalid
}
