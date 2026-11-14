// Package passwords stores and checks account passwords with argon2id. The
// encoded form is the PHC string, so each hash carries its own salt and cost
// parameters.
package passwords

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// The cost parameters are the OWASP minimum for argon2id, which keeps one check
// around tens of milliseconds on a small host. Hashes record their own
// parameters, so these can be raised later without breaking existing accounts.
const (
	memoryKiB = 19 * 1024
	timeCost  = 2
	threads   = 1
	keyLen    = 32
	saltLen   = 16
)

// ErrEmpty is returned for an empty password. An empty password never matches.
var ErrEmpty = errors.New("password must not be empty")

// MaxBytes is the longest password checked at all (#109). A longer one is
// refused before any hashing, so a large request body costs nothing.
const MaxBytes = 1024

// gate bounds how many argon2 computations run at once in the process. Each
// takes 19 MiB; a burst of them exhausts a small host. A full gate makes a
// caller answer "busy" at once instead of queueing (#109).
// Slots is how many password computations may run at once.
const Slots = 2

var gate = make(chan struct{}, Slots)

// TryAcquire takes a slot for one password computation. It returns false at
// once when all slots are busy; the caller then answers 429, without a hash.
func TryAcquire() bool {
	select {
	case gate <- struct{}{}:
		return true
	default:
		return false
	}
}

// Release frees a slot taken by TryAcquire.
func Release() { <-gate }

// Hash returns the argon2id encoding of password, with a fresh random salt.
func Hash(password string) (string, error) {
	if password == "" {
		return "", ErrEmpty
	}
	if len(password) > MaxBytes {
		return "", fmt.Errorf("password is longer than %d bytes", MaxBytes)
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, timeCost, memoryKiB, threads, keyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, memoryKiB, timeCost, threads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// Verify reports whether password matches the encoded hash. An empty encoding
// (a passkey-only account), an empty password and a malformed encoding all
// return false.
func Verify(encoded, password string) bool {
	if encoded == "" || password == "" || len(password) > MaxBytes {
		return false
	}
	// "", "argon2id", "v=19", "m=…,t=…,p=…", salt, key
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}
	var memory, time uint32
	var par uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &par); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, time, memory, par, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// Burn spends the same time as Verify without a stored hash. Sign-in calls it
// for unknown usernames, so response time does not reveal which accounts exist.
func Burn(password string) {
	if len(password) > MaxBytes {
		return
	}
	salt := make([]byte, saltLen)
	_ = argon2.IDKey([]byte(password), salt, timeCost, memoryKiB, threads, keyLen)
}
