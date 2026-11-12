package main

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"path/filepath"

	"github.com/tsirysndr/tinysonic/internal/config"
	"github.com/tsirysndr/tinysonic/internal/tlscert"
)

// runCert makes the certificate for HTTPS on a LAN, or prints the pin of the
// configured one. Run it once per install; see README "Running over HTTPS".
func runCert(args []string) error {
	if len(args) > 0 && args[0] == "fingerprint" {
		return runCertFingerprint(args[1:])
	}
	fs := flag.NewFlagSet("cert", flag.ContinueOnError)
	cfgPath := fs.String("c", "smolsonic.toml", "path to the TOML config file")
	dir := fs.String("dir", "", "directory for cert.pem and key.pem (default: tls/ next to the config file)")
	force := fs.Bool("force", false, "replace an existing certificate; devices must pair again")
	if err := fs.Parse(reorderFlags(args)); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	host, err := baseHost(cfg.BaseURL)
	if err != nil {
		return err
	}
	outDir := *dir
	if outDir == "" {
		outDir = filepath.Join(filepath.Dir(*cfgPath), "tls")
	}
	certPath, keyPath, err := tlscert.Write(outDir, []string{host}, *force)
	if err != nil {
		return err
	}
	certPath, _ = filepath.Abs(certPath)
	keyPath, _ = filepath.Abs(keyPath)
	pin, err := tlscert.Pin(certPath)
	if err != nil {
		return err
	}
	fmt.Printf("wrote %s and %s (key mode 0600), valid for %d years for %s\n",
		certPath, keyPath, int(tlscert.Validity.Hours()/24/365), host)
	fmt.Printf("\nadd to %s:\n  tls_cert = %q\n  tls_key  = %q\n", *cfgPath, certPath, keyPath)
	fmt.Printf("\ncertificate pin: %s\n", pin)
	fmt.Println("pairing QR codes carry this pin; manual setup needs it confirmed on the app")
	return nil
}

// runCertFingerprint prints the pin of the configured certificate.
func runCertFingerprint(args []string) error {
	fs := flag.NewFlagSet("cert fingerprint", flag.ContinueOnError)
	cfgPath := fs.String("c", "smolsonic.toml", "path to the TOML config file")
	if err := fs.Parse(reorderFlags(args)); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if cfg.TLSCert == "" {
		return errors.New("tls_cert is not set in the config; run tinysonic cert first")
	}
	pin, err := tlscert.Pin(cfg.TLSCert)
	if err != nil {
		return err
	}
	fmt.Println(pin)
	return nil
}

// baseHost is the host name or address in base_url, which the certificate names.
func baseHost(baseURL string) (string, error) {
	if baseURL == "" {
		return "", errors.New("set base_url in the config first: the certificate names its host")
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("base_url %q has no host name", baseURL)
	}
	return u.Hostname(), nil
}
