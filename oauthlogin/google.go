package oauthlogin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	googleAuthURL     = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL    = "https://oauth2.googleapis.com/token"
	googleUserInfoURL = "https://www.googleapis.com/oauth2/v2/userinfo"
)

var googleScopes = []string{
	"openid",
	"https://www.googleapis.com/auth/userinfo.email",
	"https://www.googleapis.com/auth/userinfo.profile",
}

// GoogleConfig is the OAuth client for Google sign-in.
type GoogleConfig struct {
	ClientID, ClientSecret, RedirectURI string
	// Endpoints and HTTPClient exist for tests; zero values use Google.
	AuthURL, TokenURL, UserInfoURL string
	HTTPClient                     *http.Client
}

// Google is the Google identity provider.
type Google struct{ Cfg GoogleConfig }

var _ Provider = Google{}

func (g Google) Key() string   { return "google" }
func (g Google) Label() string { return "Google" }
func (g Google) Icon() string  { return "google" }

func (g Google) configured() bool { return g.Cfg.ClientID != "" && g.Cfg.ClientSecret != "" }

// Status mirrors the Node wording.
func (g Google) Status(string) (bool, string) {
	if g.configured() {
		return true, "Google sign-in is ready for account connections."
	}
	return false, "Google sign-in still needs administrator setup before accounts can connect."
}

func pick(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

func (g Google) client() *http.Client {
	if g.Cfg.HTTPClient != nil {
		return g.Cfg.HTTPClient
	}
	return &http.Client{Timeout: 20 * time.Second}
}

// Begin builds the authorization address.
func (g Google) Begin(_ context.Context, state, codeVerifier, _ string) (string, error) {
	if !g.configured() {
		return "", fmt.Errorf("Google sign-in is not configured.")
	}
	q := url.Values{}
	q.Set("access_type", "offline")
	q.Set("scope", strings.Join(googleScopes, " "))
	q.Set("state", state)
	q.Set("prompt", "select_account")
	q.Set("code_challenge_method", "S256")
	q.Set("code_challenge", CodeChallenge(codeVerifier))
	q.Set("response_type", "code")
	q.Set("client_id", g.Cfg.ClientID)
	q.Set("redirect_uri", g.Cfg.RedirectURI)
	return pick(g.Cfg.AuthURL, googleAuthURL) + "?" + q.Encode(), nil
}

type googleProfile struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	VerifiedEmail bool   `json:"verified_email"`
	Name          string `json:"name"`
	Picture       string `json:"picture"`
	GivenName     string `json:"given_name"`
	FamilyName    string `json:"family_name"`
	Locale        string `json:"locale"`
}

func (g Google) getJSON(req *http.Request, out any) error {
	resp, err := g.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("Google answered %d", resp.StatusCode)
	}
	return json.Unmarshal(body, out)
}

// Finish exchanges the code and reads the account's profile.
func (g Google) Finish(ctx context.Context, code, codeVerifier string) (Identity, error) {
	if !g.configured() {
		return Identity{}, fmt.Errorf("Google sign-in is not configured.")
	}
	form := url.Values{
		"code": {code}, "client_id": {g.Cfg.ClientID}, "client_secret": {g.Cfg.ClientSecret},
		"redirect_uri": {g.Cfg.RedirectURI}, "grant_type": {"authorization_code"}, "code_verifier": {codeVerifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, pick(g.Cfg.TokenURL, googleTokenURL), strings.NewReader(form.Encode()))
	if err != nil {
		return Identity{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := g.getJSON(req, &tok); err != nil || tok.AccessToken == "" {
		return Identity{}, fmt.Errorf("Google did not accept the sign-in code.")
	}
	preq, err := http.NewRequestWithContext(ctx, http.MethodGet, pick(g.Cfg.UserInfoURL, googleUserInfoURL), nil)
	if err != nil {
		return Identity{}, err
	}
	preq.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	var p googleProfile
	if err := g.getJSON(preq, &p); err != nil {
		return Identity{}, fmt.Errorf("Google did not return a usable account identity.")
	}
	id := strings.TrimSpace(p.ID)
	email := strings.ToLower(strings.TrimSpace(p.Email))
	if id == "" || email == "" {
		return Identity{}, fmt.Errorf("Google did not return a usable account identity.")
	}
	ident := Identity{
		ProviderUserID: id, Email: email, EmailVerified: p.VerifiedEmail,
		DisplayName: strings.TrimSpace(p.Name),
		Metadata: map[string]any{
			"givenName":  nilIfEmpty(strings.TrimSpace(p.GivenName)),
			"familyName": nilIfEmpty(strings.TrimSpace(p.FamilyName)),
			"locale":     nilIfEmpty(strings.TrimSpace(p.Locale)),
		},
	}
	if pic := strings.TrimSpace(p.Picture); pic != "" {
		ident.AvatarURL = &pic
	}
	return ident, nil
}
