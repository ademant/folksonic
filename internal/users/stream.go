package users

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// StreamURLTTL is how long a signed stream URL works: a full track plus
// pauses, so seeking keeps working until it expires.
const StreamURLTTL = 2 * time.Hour

const streamSecretSetting = "stream_secret"

// LoadStreamSecret returns the key that signs stream URLs. It is created on
// first use and kept in the settings table.
func LoadStreamSecret(ctx context.Context, db *sql.DB) ([]byte, error) {
	var stored string
	err := db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, streamSecretSetting).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return RotateStreamSecret(ctx, db)
	}
	if err != nil {
		return nil, err
	}
	return hex.DecodeString(stored)
}

// RotateStreamSecret replaces the signing key. Every signed URL made with the
// old key stops verifying, but only once the server has restarted: it keeps
// the key it loaded.
func RotateStreamSecret(ctx context.Context, db *sql.DB) ([]byte, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	_, err := db.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		streamSecretSetting, hex.EncodeToString(secret))
	if err != nil {
		return nil, err
	}
	return secret, nil
}

// SignStream is the signature of a stream URL: HMAC-SHA256 over the song id,
// the user id and the expiry (Unix seconds), hex encoded.
func SignStream(secret []byte, songID string, userID, expires int64) string {
	mac := hmac.New(sha256.New, secret)
	fmt.Fprintf(mac, "%s|%d|%d", songID, userID, expires)
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifySignedStream reports whether sig is the signature of the parameters.
// It does not check the expiry; the caller does, after this passes.
func VerifySignedStream(secret []byte, songID string, userID, expires int64, sig string) bool {
	got, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	want, _ := hex.DecodeString(SignStream(secret, songID, userID, expires))
	return hmac.Equal(got, want)
}
