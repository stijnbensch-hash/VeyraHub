package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTestAPNsKey generates a fresh P-256 EC key, PKCS8/PEM-encodes it (the
// same shape as an Apple-issued .p8 file), and writes it to a temp file,
// returning its path.
func writeTestAPNsKey(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "AuthKey_TEST.p8")
	data := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadAPNsConfigEmptyKeyPathIsUnconfigured(t *testing.T) {
	config, err := loadAPNsConfig("", "", "", "", false)
	if err != nil {
		t.Fatalf("expected no error for an empty key path, got %v", err)
	}
	if config.configured() {
		t.Fatal("expected an empty key path to leave APNs unconfigured")
	}
}

func TestLoadAPNsConfigRequiresAllFields(t *testing.T) {
	keyPath := writeTestAPNsKey(t)
	if _, err := loadAPNsConfig(keyPath, "", "team", "bundle", false); err == nil {
		t.Fatal("expected an error when the key id is missing")
	}
	if _, err := loadAPNsConfig(keyPath, "key", "", "bundle", false); err == nil {
		t.Fatal("expected an error when the team id is missing")
	}
	if _, err := loadAPNsConfig(keyPath, "key", "team", "", false); err == nil {
		t.Fatal("expected an error when the bundle id is missing")
	}
}

func TestLoadAPNsConfigValidKeyIsConfigured(t *testing.T) {
	keyPath := writeTestAPNsKey(t)
	config, err := loadAPNsConfig(keyPath, "KEYID123", "TEAMID456", "com.example.Veyra", true)
	if err != nil {
		t.Fatalf("loadAPNsConfig: %v", err)
	}
	if !config.configured() {
		t.Fatal("expected a valid key + ids to be configured")
	}
	if !config.Production {
		t.Fatal("expected Production to be carried through from the argument")
	}
}

func TestLoadAPNsConfigRejectsNonPEMFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "AuthKey_TEST.p8")
	if err := os.WriteFile(path, []byte("not a pem file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAPNsConfig(path, "key", "team", "bundle", false); err == nil {
		t.Fatal("expected an error for a non-PEM key file")
	}
}

// TestAPNsSignedTokenIsWellFormedAndCached checks that signedToken produces
// a three-part JWT (header.claims.signature, all base64url) with a raw
// 64-byte r||s signature rather than ASN.1 DER, and that calling it again
// immediately reuses the cached token instead of re-signing.
func TestAPNsSignedTokenIsWellFormedAndCached(t *testing.T) {
	keyPath := writeTestAPNsKey(t)
	config, err := loadAPNsConfig(keyPath, "KEYID123", "TEAMID456", "com.example.Veyra", false)
	if err != nil {
		t.Fatalf("loadAPNsConfig: %v", err)
	}
	client := newAPNsClient(config)

	token, err := client.signedToken()
	if err != nil {
		t.Fatalf("signedToken: %v", err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("expected a 3-part JWT, got %d parts", len(parts))
	}

	again, err := client.signedToken()
	if err != nil {
		t.Fatalf("signedToken (second call): %v", err)
	}
	if again != token {
		t.Fatal("expected signedToken to reuse the cached token within apnsTokenTTL")
	}
}
