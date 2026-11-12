package tlscert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func parseLeaf(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("no PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func TestGenerateVerifiesForIPAndName(t *testing.T) {
	now := time.Now()
	certPEM, _, err := Generate([]string{"192.168.1.10", "folksonic.local"}, now)
	if err != nil {
		t.Fatal(err)
	}
	cert := parseLeaf(t, certPEM)
	if err := cert.VerifyHostname("192.168.1.10"); err != nil {
		t.Errorf("IP not in certificate: %v", err)
	}
	if err := cert.VerifyHostname("folksonic.local"); err != nil {
		t.Errorf("name not in certificate: %v", err)
	}
	if err := cert.VerifyHostname("other.example"); err == nil {
		t.Error("certificate verifies for a host it does not name")
	}
	if len(cert.IPAddresses) != 1 || !cert.IPAddresses[0].Equal(net.ParseIP("192.168.1.10")) {
		t.Errorf("IP SANs %v", cert.IPAddresses)
	}
}

func TestGenerateIsP256AndServerOnly(t *testing.T) {
	certPEM, _, err := Generate([]string{"192.168.1.10"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cert := parseLeaf(t, certPEM)
	key, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || key.Curve != elliptic.P256() {
		t.Fatalf("key is not ECDSA P-256: %T", cert.PublicKey)
	}
	if cert.IsCA {
		t.Error("certificate is a CA")
	}
	if len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Errorf("extended key usage %v, want server auth only", cert.ExtKeyUsage)
	}
	if got := cert.NotAfter.Sub(cert.NotBefore); got < Validity-2*time.Hour {
		t.Errorf("validity %v, want about %v", got, Validity)
	}
}

func TestGenerateNeedsAHost(t *testing.T) {
	if _, _, err := Generate(nil, time.Now()); err == nil {
		t.Fatal("no host accepted")
	}
}

func TestEachInstallGetsItsOwnKey(t *testing.T) {
	a, _, _ := Generate([]string{"192.168.1.10"}, time.Now())
	b, _, _ := Generate([]string{"192.168.1.10"}, time.Now())
	pa, _ := Pin(writeTemp(t, a))
	pb, _ := Pin(writeTemp(t, b))
	if pa == pb {
		t.Fatal("two installs share a key")
	}
}

func TestWriteKeyIsPrivateAndRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, err := Write(dir, []string{"192.168.1.10"}, false)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("key mode %v, want 0600", info.Mode().Perm())
	}
	before, _ := Pin(certPath)

	if _, _, err := Write(dir, []string{"192.168.1.10"}, false); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("overwrite without force: err = %v", err)
	}
	if after, _ := Pin(certPath); after != before {
		t.Fatal("refused write still changed the certificate")
	}
	if _, _, err := Write(dir, []string{"192.168.1.10"}, true); err != nil {
		t.Fatalf("force: %v", err)
	}
	if after, _ := Pin(certPath); after == before {
		t.Fatal("force did not replace the certificate")
	}
}

func TestPinIsSPKISHA256(t *testing.T) {
	certPEM, _, err := Generate([]string{"192.168.1.10"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	path := writeTemp(t, certPEM)
	got, err := Pin(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(parseLeaf(t, certPEM).RawSubjectPublicKeyInfo)
	want := "sha256/" + base64.StdEncoding.EncodeToString(sum[:])
	if got != want {
		t.Fatalf("pin %q, want %q", got, want)
	}
}

func TestPinEmptyPathHasNoPin(t *testing.T) {
	if got, err := Pin(""); got != "" || err != nil {
		t.Fatalf("empty path: %q, %v", got, err)
	}
}

func writeTemp(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cert.pem")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
