// Package googleauth gets OAuth access tokens from Google Application Default
// Credentials files: a service account key (signed JWT exchanged for a token)
// or the authorized-user file `gcloud auth application-default login` writes
// (refresh token). It needs no Google SDK.
package googleauth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// CloudPlatformScope is the broad scope Vertex AI calls need.
const CloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

const defaultTokenURL = "https://oauth2.googleapis.com/token"

// Credentials is a parsed credentials file.
type Credentials struct {
	Type         string `json:"type"`
	ProjectID    string `json:"project_id"`
	QuotaProject string `json:"quota_project_id"`
	// Service account.
	ClientEmail  string `json:"client_email"`
	PrivateKeyID string `json:"private_key_id"`
	PrivateKey   string `json:"private_key"`
	TokenURI     string `json:"token_uri"`
	// Authorized user.
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RefreshToken string `json:"refresh_token"`
}

// Load reads a credentials file.
func Load(path string) (*Credentials, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Credentials
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, errors.New("the Google credentials file is not valid JSON")
	}
	return &c, nil
}

// DefaultFile is the first existing ADC file: GOOGLE_APPLICATION_CREDENTIALS,
// then gcloud's well-known location ("" when there is none).
func DefaultFile(getenv func(string) string, home, goos string) string {
	var candidates []string
	if p := strings.TrimSpace(getenv("GOOGLE_APPLICATION_CREDENTIALS")); p != "" {
		candidates = append(candidates, p)
	}
	if goos == "windows" {
		if appData := strings.TrimSpace(getenv("APPDATA")); appData != "" {
			candidates = append(candidates, appData+`\gcloud\application_default_credentials.json`)
		}
	} else if home != "" {
		candidates = append(candidates, home+"/.config/gcloud/application_default_credentials.json")
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.Mode().IsRegular() {
			return c
		}
	}
	return ""
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func parseKey(pemKey string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemKey))
	if block == nil {
		return nil, errors.New("the service account private key is not PEM")
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rsaKey, ok := key.(*rsa.PrivateKey); ok {
			return rsaKey, nil
		}
		return nil, errors.New("the service account private key is not RSA")
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

// signedAssertion is the RS256 JWT a service account trades for a token.
func (c *Credentials) signedAssertion(scope, audience string, now time.Time) (string, error) {
	key, err := parseKey(c.PrivateKey)
	if err != nil {
		return "", err
	}
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	if c.PrivateKeyID != "" {
		header["kid"] = c.PrivateKeyID
	}
	claims := map[string]any{"iss": c.ClientEmail, "scope": scope, "aud": audience, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()}
	h, _ := json.Marshal(header)
	p, _ := json.Marshal(claims)
	unsigned := b64(h) + "." + b64(p)
	sum := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return unsigned + "." + b64(sig), nil
}

// AccessToken trades the credentials for an access token of scope.
func (c *Credentials) AccessToken(ctx context.Context, client *http.Client, scope string) (string, error) {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	tokenURL := c.TokenURI
	if tokenURL == "" {
		tokenURL = defaultTokenURL
	}
	form := url.Values{}
	switch c.Type {
	case "service_account":
		assertion, err := c.signedAssertion(scope, tokenURL, time.Now())
		if err != nil {
			return "", err
		}
		form.Set("grant_type", "urn:ietf:params:oauth:grant-type:jwt-bearer")
		form.Set("assertion", assertion)
	case "authorized_user":
		form.Set("grant_type", "refresh_token")
		form.Set("client_id", c.ClientID)
		form.Set("client_secret", c.ClientSecret)
		form.Set("refresh_token", c.RefreshToken)
	default:
		return "", fmt.Errorf("unsupported Google credentials type %q", c.Type)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &out)
	if resp.StatusCode != http.StatusOK || out.AccessToken == "" {
		msg := out.Description
		if msg == "" {
			msg = out.Error
		}
		if msg == "" {
			msg = resp.Status
		}
		return "", fmt.Errorf("Google token exchange failed: %s", msg)
	}
	return out.AccessToken, nil
}
