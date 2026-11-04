package passwords

import (
	"strings"
	"testing"
)

func TestHashVerifyRoundTrip(t *testing.T) {
	h, err := Hash("s3cret")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("unexpected encoding %q", h)
	}
	if !Verify(h, "s3cret") {
		t.Fatal("correct password rejected")
	}
	if Verify(h, "s3cre") || Verify(h, "S3cret") {
		t.Fatal("wrong password accepted")
	}
}

func TestHashUsesFreshSalt(t *testing.T) {
	a, _ := Hash("same")
	b, _ := Hash("same")
	if a == b {
		t.Fatal("two hashes of the same password are identical; salt is not random")
	}
}

func TestEmptyValuesNeverMatch(t *testing.T) {
	if _, err := Hash(""); err == nil {
		t.Fatal("empty password was hashed")
	}
	h, _ := Hash("x")
	if Verify(h, "") {
		t.Fatal("empty password matched")
	}
	if Verify("", "anything") {
		t.Fatal("empty encoding matched (passkey-only accounts must not match)")
	}
}

func TestMalformedEncodingsRejected(t *testing.T) {
	for _, enc := range []string{
		"plaintext-password",
		"$argon2i$v=19$m=19456,t=2,p=1$c2FsdA$aGFzaA",
		"$argon2id$v=18$m=19456,t=2,p=1$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=19456,t=2,p=1$!!$aGFzaA",
		"$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$",
	} {
		if Verify(enc, "plaintext-password") {
			t.Fatalf("malformed %q verified", enc)
		}
	}
}
