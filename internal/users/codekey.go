package users

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

// codeKeyLen is the length of the key that keys user codes, in bytes.
const codeKeyLen = 32

// ErrNoCodeKey is returned when a device grant is started without the key.
var ErrNoCodeKey = errors.New("device code key is not loaded")

// LoadOrCreateCodeKey returns the key that keys user codes of device grants. It
// is created on first use with mode 0600. It is kept in its own file, next to
// the database, and not in the database itself, so a copy of the database (a
// backup, for example) does not carry the key. A user code is only about 38
// bits, so an unkeyed hash of it could be checked offline. Losing the key
// only cancels the device grants that are pending at the time.
func LoadOrCreateCodeKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		key, err := hex.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(key) != codeKeyLen {
			return nil, fmt.Errorf("%s does not hold a %d-byte key; delete it to get a new one", path, codeKeyLen)
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key := make([]byte, codeKeyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := f.WriteString(hex.EncodeToString(key) + "\n"); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return key, nil
}

// UserCodeHash is the stored form of a normalised user code: HMAC-SHA256 under
// the code key. Device codes are random and use hashToken instead.
func UserCodeHash(key []byte, code string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(code))
	return hex.EncodeToString(m.Sum(nil))
}
