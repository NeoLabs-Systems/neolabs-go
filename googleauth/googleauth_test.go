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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServiceAccountTokenExchange(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
			t.Errorf("grant %q", r.Form.Get("grant_type"))
		}
		parts := strings.Split(r.Form.Get("assertion"), ".")
		sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
		sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
			t.Errorf("signature: %v", err)
		}
		claims, _ := base64.RawURLEncoding.DecodeString(parts[1])
		var c map[string]any
		_ = json.Unmarshal(claims, &c)
		if c["iss"] != "sa@p.iam" || c["scope"] != CloudPlatformScope {
			t.Errorf("claims %v", c)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "tok"})
	}))
	defer srv.Close()
	c := &Credentials{Type: "service_account", ClientEmail: "sa@p.iam", PrivateKey: pemKey, TokenURI: srv.URL}
	got, err := c.AccessToken(context.Background(), srv.Client(), CloudPlatformScope)
	if err != nil || got != "tok" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestAuthorizedUserRefreshAndErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("refresh_token") == "bad" {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"Token has been expired"}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"user-tok"}`))
	}))
	defer srv.Close()
	ok := &Credentials{Type: "authorized_user", ClientID: "c", ClientSecret: "s", RefreshToken: "r", TokenURI: srv.URL}
	if got, err := ok.AccessToken(context.Background(), nil, CloudPlatformScope); err != nil || got != "user-tok" {
		t.Fatalf("%q %v", got, err)
	}
	bad := &Credentials{Type: "authorized_user", RefreshToken: "bad", TokenURI: srv.URL}
	if _, err := bad.AccessToken(context.Background(), nil, CloudPlatformScope); err == nil || !strings.Contains(err.Error(), "Token has been expired") {
		t.Fatalf("got %v", err)
	}
	if _, err := (&Credentials{Type: "external_account"}).AccessToken(context.Background(), nil, CloudPlatformScope); err == nil {
		t.Fatal("unsupported type accepted")
	}
}
