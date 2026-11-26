package github

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"
	"time"
)

func testKeyPEM(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k, string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
}

// The key is accepted as PEM, as PEM with escaped newlines (one-line env var),
// and as base64 of the PEM.
func TestNewApp_AcceptsKeyFormats(t *testing.T) {
	_, pemText := testKeyPEM(t)
	for name, raw := range map[string]string{
		"pem":     pemText,
		"escaped": strings.ReplaceAll(pemText, "\n", `\n`),
		"base64":  base64.StdEncoding.EncodeToString([]byte(pemText)),
	} {
		if _, err := NewApp(1, "mountabo", raw); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := NewApp(1, "x", "not a key"); err == nil {
		t.Error("garbage key accepted")
	}
	if _, err := NewApp(0, "x", pemText); err == nil {
		t.Error("missing app id accepted")
	}
}

// The App JWT is RS256 over header.claims, issued by the App id, valid for
// under ten minutes, with iat backdated for clock drift.
func TestApp_JWTVerifies(t *testing.T) {
	key, pemText := testKeyPEM(t)
	app, err := NewApp(12345, "mountabo", pemText)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	tok, err := app.jwt(now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt has %d parts", len(parts))
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Iat int64 `json:"iat"`
		Exp int64 `json:"exp"`
		Iss int64 `json:"iss"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Iss != 12345 || claims.Iat >= now.Unix() || claims.Exp-claims.Iat > 600 {
		t.Errorf("claims = %+v", claims)
	}
}
