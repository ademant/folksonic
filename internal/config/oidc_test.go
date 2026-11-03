package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadWith writes a config file with the given oidc block and returns Load's result.
func loadWith(t *testing.T, extra string) (*Config, error) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "smolsonic.toml")
	body := "music_dirs = [\"music\"]\n" + extra
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

func TestOIDCDisabledByDefault(t *testing.T) {
	c, err := loadWith(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.OIDC.Enabled() {
		t.Fatal("oidc enabled without an oidc block")
	}
}

func TestOIDCValidation(t *testing.T) {
	const base = "base_url = \"https://music.example.org\"\n"
	cases := []struct {
		name, extra, wantErr string
	}{
		{"valid", base + "[oidc]\nissuer = \"https://auth.example.org/realms/main\"\nclient_id = \"tinysonic\"\n", ""},
		{"needs base_url", "[oidc]\nissuer = \"https://auth.example.org\"\nclient_id = \"tinysonic\"\n", "needs base_url"},
		{"needs client_id", base + "[oidc]\nissuer = \"https://auth.example.org\"\n", "needs client_id"},
		{"secret without issuer", base + "[oidc]\nclient_secret = \"x\"\n", "needs issuer"},
		{"plain http issuer refused", base + "[oidc]\nissuer = \"http://auth.example.org\"\nclient_id = \"t\"\n", "must use https"},
		{"localhost http allowed", base + "[oidc]\nissuer = \"http://localhost:8080\"\nclient_id = \"t\"\n", ""},
		{"not a URL", base + "[oidc]\nissuer = \"auth.example.org\"\nclient_id = \"t\"\n", "absolute http or https URL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadWith(t, tc.extra)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}
