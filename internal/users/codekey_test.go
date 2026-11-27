package users

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The key is created once with mode 0600, and the same key is read back.
func TestLoadOrCreateCodeKeyPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "device-code.key")
	first, err := LoadOrCreateCodeKey(path)
	if err != nil || len(first) != codeKeyLen {
		t.Fatalf("create: %v, len %d", err, len(first))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o, want 600", info.Mode().Perm())
	}
	second, err := LoadOrCreateCodeKey(path)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("reload: %v, same key %v", err, bytes.Equal(first, second))
	}
}

// A file that is not a key is an error, not a silent new key.
func TestLoadOrCreateCodeKeyRejectsBadFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "device-code.key")
	if err := os.WriteFile(path, []byte("not hex"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateCodeKey(path); err == nil {
		t.Fatal("bad key file was accepted")
	}
}

// The same code hashes differently under another key, so a copy of the
// database alone cannot check guesses.
func TestUserCodeHashIsKeyed(t *testing.T) {
	a := UserCodeHash([]byte("key-one-0123456789abcdef0123456"), "BCDF2345")
	b := UserCodeHash([]byte("key-two-0123456789abcdef0123456"), "BCDF2345")
	if a == b {
		t.Fatal("user code hash does not depend on the key")
	}
	if a != UserCodeHash([]byte("key-one-0123456789abcdef0123456"), "BCDF2345") {
		t.Fatal("user code hash is not deterministic")
	}
}

// A grant cannot be started or approved without the key.
func TestDeviceGrantNeedsCodeKey(t *testing.T) {
	ctx, pool, bob, now := grantFixture(t)
	if _, err := CreateDeviceAuth(ctx, pool, nil, "phone", now); !errors.Is(err, ErrNoCodeKey) {
		t.Fatalf("create without key: %v", err)
	}
	if err := ApproveUserCode(ctx, pool, nil, bob, "BCDF2345", now); !errors.Is(err, ErrNoCodeKey) {
		t.Fatalf("approve without key: %v", err)
	}
}
