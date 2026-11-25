package oauth

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"
)

func token(claims string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(claims)) + ".s"
}

func TestJWTHelpers(t *testing.T) {
	if ms, ok := JWTExpiryMs(token(`{"exp":1800000000}`)); !ok || ms != 1800000000000 {
		t.Errorf("exp = %d %v", ms, ok)
	}
	for _, bad := range []string{"", "abc", token(`{"exp":0}`), token(`not json`), token(`{}`)} {
		if _, ok := JWTExpiryMs(bad); ok {
			t.Errorf("%q should have no expiry", bad)
		}
	}
	if c := JWTClaims(token(`{"chatgpt_account_id":"a"}`)); c["chatgpt_account_id"] != "a" {
		t.Errorf("claims = %v", c)
	}
	if JWTClaims("x") != nil {
		t.Error("garbage has no claims")
	}
}

func TestExpiryFromResponse(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_000)
	cases := []struct {
		data map[string]any
		want int64
	}{
		{map[string]any{"expires_at": 1_800_000_000.0}, 1_800_000_000_000},
		{map[string]any{"expiresAt": 1_800_000_000_000.0}, 1_800_000_000_000},
		{map[string]any{"expires_in": 3600.0}, 1_700_000_000_000 + 3_600_000},
		{map[string]any{"expires_in": -5.0}, 0},
		{map[string]any{}, 0},
	}
	for _, c := range cases {
		if got := ExpiryFromResponse(c.data, now); got != c.want {
			t.Errorf("%v = %d, want %d", c.data, got, c.want)
		}
	}
}

func TestRefreshKeepsOldRefreshTokenAndPersists(t *testing.T) {
	var persisted []Tokens
	c := New(Tokens{Access: "a1", Refresh: "r1"}, func(t Tokens) { persisted = append(persisted, t) })
	got, err := c.Refresh(context.Background(), func(_ context.Context, refresh string) (Tokens, error) {
		if refresh != "r1" {
			t.Errorf("refresh token = %q", refresh)
		}
		return Tokens{Access: "a2"}, nil
	})
	if err != nil || got.Access != "a2" || got.Refresh != "r1" || c.Get().Access != "a2" || len(persisted) != 1 {
		t.Errorf("got %+v err %v creds %+v persisted %v", got, err, c.Get(), persisted)
	}
	// a failed exchange changes nothing
	_, err = c.Refresh(context.Background(), func(context.Context, string) (Tokens, error) { return Tokens{}, errors.New("nope") })
	if err == nil || c.Get().Access != "a2" || len(persisted) != 1 {
		t.Errorf("failed refresh: %v %+v", err, c.Get())
	}
	// no refresh token: nothing to exchange
	static := New(Tokens{Access: "only"}, nil)
	got, err = static.Refresh(context.Background(), func(context.Context, string) (Tokens, error) { t.Error("called"); return Tokens{}, nil })
	if err != nil || got.Access != "only" || static.Refreshable() {
		t.Errorf("static: %+v %v", got, err)
	}
}

func TestExpiring(t *testing.T) {
	now := time.Now()
	c := New(Tokens{Access: "a", ExpiresAtMs: now.Add(2 * time.Minute).UnixMilli()}, nil)
	if !c.Expiring(now, 5*time.Minute) || c.Expiring(now, time.Minute) {
		t.Error("skew not applied")
	}
	if New(Tokens{Access: "a"}, nil).Expiring(now, time.Hour) {
		t.Error("unknown expiry is not expiring")
	}
}
