package users

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/tsirysndr/tinysonic/internal/db"
)

func TestStreamSecretIsStableUntilRotated(t *testing.T) {
	pool, err := db.Init(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ctx := context.Background()

	first, err := LoadStreamSecret(ctx, pool)
	if err != nil || len(first) != 32 {
		t.Fatalf("first load: %d bytes, %v", len(first), err)
	}
	again, err := LoadStreamSecret(ctx, pool)
	if err != nil || string(again) != string(first) {
		t.Fatal("second load gave a different key")
	}
	rotated, err := RotateStreamSecret(ctx, pool)
	if err != nil || string(rotated) == string(first) {
		t.Fatal("rotation kept the old key")
	}
	after, err := LoadStreamSecret(ctx, pool)
	if err != nil || string(after) != string(rotated) {
		t.Fatal("load after rotation did not return the new key")
	}
}

func TestSignedStreamBindsSongUserAndExpiry(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	sig := SignStream(secret, "so-1", 7, 1000)

	if !VerifySignedStream(secret, "so-1", 7, 1000, sig) {
		t.Fatal("valid signature refused")
	}
	for name, ok := range map[string]bool{
		"other song":   VerifySignedStream(secret, "so-2", 7, 1000, sig),
		"other user":   VerifySignedStream(secret, "so-1", 8, 1000, sig),
		"other expiry": VerifySignedStream(secret, "so-1", 7, 1001, sig),
		"other key":    VerifySignedStream([]byte("another-key-another-key-another!"), "so-1", 7, 1000, sig),
		"not hex":      VerifySignedStream(secret, "so-1", 7, 1000, "zz"),
		"empty":        VerifySignedStream(secret, "so-1", 7, 1000, ""),
	} {
		if ok {
			t.Errorf("%s accepted", name)
		}
	}
}
