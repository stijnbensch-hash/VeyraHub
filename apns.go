package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// APNsConfig holds the credentials needed to sign and send Apple Push
// Notification Service requests using token-based (JWT) provider
// authentication. A zero-value APNsConfig is "unconfigured": push stays an
// optional feature and the hub runs fine without it — see loadAPNsConfig.
type APNsConfig struct {
	PrivateKey *ecdsa.PrivateKey
	KeyID      string
	TeamID     string
	BundleID   string
	Production bool
}

func (c APNsConfig) configured() bool {
	return c.PrivateKey != nil && c.KeyID != "" && c.TeamID != "" && c.BundleID != ""
}

// loadAPNsConfig reads an APNs authentication key (a .p8 file, a PKCS#8 EC
// private key in PEM form) from keyPath and builds an APNsConfig. An empty
// keyPath means "push not configured": it returns a zero-value config with
// no error, so deployments without push credentials aren't forced to set
// anything.
func loadAPNsConfig(keyPath, keyID, teamID, bundleID string, production bool) (APNsConfig, error) {
	if strings.TrimSpace(keyPath) == "" {
		return APNsConfig{}, nil
	}
	if keyID == "" || teamID == "" || bundleID == "" {
		return APNsConfig{}, errors.New("apns-sleutel-id, team-id en bundle-id zijn allemaal vereist als een apns-sleutel is opgegeven")
	}
	data, err := os.ReadFile(keyPath)
	if err != nil {
		return APNsConfig{}, fmt.Errorf("kan apns-sleutel niet lezen: %w", err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return APNsConfig{}, errors.New("apns-sleutel is geen geldig PEM-bestand")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return APNsConfig{}, fmt.Errorf("kan apns-sleutel niet parsen: %w", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return APNsConfig{}, errors.New("apns-sleutel is geen EC-sleutel")
	}
	return APNsConfig{
		PrivateKey: key, KeyID: keyID, TeamID: teamID, BundleID: bundleID, Production: production,
	}, nil
}

// apnsTokenTTL controls how long a signed provider JWT is reused before
// being re-signed. Apple allows reuse for up to an hour; this stays well
// under that.
const apnsTokenTTL = 45 * time.Minute

// apnsClient sends push notifications to APNs using a cached, periodically
// re-signed provider JWT (token-based authentication). It's implemented
// against the standard library only: Go's net/http negotiates HTTP/2 over
// TLS automatically, which is all APNs requires beyond the JWT itself, so
// no new module dependency is needed.
type apnsClient struct {
	config APNsConfig
	client *http.Client

	mu       sync.Mutex
	token    string
	tokenIat time.Time
}

func newAPNsClient(config APNsConfig) *apnsClient {
	return &apnsClient{config: config, client: &http.Client{Timeout: 10 * time.Second}}
}

// signedToken returns a cached ES256-signed provider JWT, re-signing it once
// apnsTokenTTL has elapsed.
func (c *apnsClient) signedToken() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.token != "" && time.Since(c.tokenIat) < apnsTokenTTL {
		return c.token, nil
	}

	now := time.Now().UTC()
	headerJSON, err := json.Marshal(map[string]any{"alg": "ES256", "kid": c.config.KeyID})
	if err != nil {
		return "", err
	}
	claimsJSON, err := json.Marshal(map[string]any{"iss": c.config.TeamID, "iat": now.Unix()})
	if err != nil {
		return "", err
	}
	signingInput := base64URLEncode(headerJSON) + "." + base64URLEncode(claimsJSON)

	hashed := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, c.config.PrivateKey, hashed[:])
	if err != nil {
		return "", err
	}

	// APNs (like JWS in general) wants the raw, fixed-width r||s signature,
	// not the ASN.1 DER encoding ecdsa.Sign's inputs might suggest — each
	// half zero-padded to the curve's byte length (32 for P-256).
	byteLen := (c.config.PrivateKey.Curve.Params().BitSize + 7) / 8
	signature := make([]byte, 2*byteLen)
	r.FillBytes(signature[:byteLen])
	s.FillBytes(signature[byteLen:])

	token := signingInput + "." + base64URLEncode(signature)
	c.token = token
	c.tokenIat = now
	return token, nil
}

// send delivers one push notification to a device token. It's used from a
// best-effort background goroutine per device (see Hub.sendPush), so a
// failure here is only ever logged, never surfaced to the request that
// triggered the notification.
func (c *apnsClient) send(ctx context.Context, deviceToken, title, body string) error {
	token, err := c.signedToken()
	if err != nil {
		return err
	}

	host := "https://api.sandbox.push.apple.com"
	if c.config.Production {
		host = "https://api.push.apple.com"
	}

	payload := map[string]any{
		"aps": map[string]any{
			"alert": map[string]any{"title": title, "body": body},
			"sound": "default",
		},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, host+"/3/device/"+deviceToken, bytes.NewReader(data))
	if err != nil {
		return err
	}
	request.Header.Set("authorization", "bearer "+token)
	request.Header.Set("apns-topic", c.config.BundleID)
	request.Header.Set("apns-push-type", "alert")
	request.Header.Set("apns-priority", "10")
	request.Header.Set("content-type", "application/json")

	response, err := c.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("apns returned HTTP %d", response.StatusCode)
	}
	return nil
}

func base64URLEncode(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}
