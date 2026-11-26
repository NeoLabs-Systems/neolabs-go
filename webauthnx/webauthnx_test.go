package webauthnx

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/httpx"
	"github.com/fxamacker/cbor/v2"
)

// ---- in-memory store and accounts -----------------------------------------

type memStore struct {
	rows []Credential
	next int64
}

func (m *memStore) ListForUser(_ context.Context, uid string) ([]Credential, error) {
	var out []Credential
	for _, c := range m.rows {
		if c.UserID == uid {
			out = append(out, c)
		}
	}
	return out, nil
}
func (m *memStore) ForUserAndRP(_ context.Context, uid, rp string) ([]Credential, error) {
	var out []Credential
	for _, c := range m.rows {
		if c.UserID == uid && c.RPID == rp {
			out = append(out, c)
		}
	}
	return out, nil
}
func (m *memStore) CountForRP(_ context.Context, rp string) (int, error) {
	n := 0
	for _, c := range m.rows {
		if c.RPID == rp {
			n++
		}
	}
	return n, nil
}
func (m *memStore) FindByCredentialID(_ context.Context, id, rp string) (*Credential, error) {
	for i := range m.rows {
		if m.rows[i].CredentialID == id && m.rows[i].RPID == rp {
			c := m.rows[i]
			return &c, nil
		}
	}
	return nil, nil
}
func (m *memStore) Exists(_ context.Context, id string) (bool, error) {
	for _, c := range m.rows {
		if c.CredentialID == id {
			return true, nil
		}
	}
	return false, nil
}
func (m *memStore) Insert(_ context.Context, c Credential) error {
	m.next++
	c.RowID = m.next
	m.rows = append(m.rows, c)
	return nil
}
func (m *memStore) UpdateAfterLogin(_ context.Context, row int64, counter uint32, uv bool) error {
	for i := range m.rows {
		if m.rows[i].RowID == row {
			m.rows[i].Counter, m.rows[i].UserVerified = counter, uv
		}
	}
	return nil
}
func (m *memStore) Rename(_ context.Context, uid string, row int64, label string) (bool, error) {
	for i := range m.rows {
		if m.rows[i].RowID == row && m.rows[i].UserID == uid {
			m.rows[i].Label = label
			return true, nil
		}
	}
	return false, nil
}
func (m *memStore) Delete(_ context.Context, uid string, row int64) (bool, error) {
	for i := range m.rows {
		if m.rows[i].RowID == row && m.rows[i].UserID == uid {
			m.rows = append(m.rows[:i], m.rows[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

type accounts struct{}

func (accounts) ByID(_ context.Context, id string) (Identity, bool, error) {
	if id == "7" {
		return Identity{ID: "7", Username: "neo", Email: "neo@example.com"}, true, nil
	}
	return Identity{}, false, nil
}
func (accounts) ForSignIn(_ context.Context, ident string) (Identity, bool, error) {
	if strings.EqualFold(ident, "neo") || ident == "neo@example.com" {
		return Identity{ID: "7", Username: "neo"}, true, nil
	}
	return Identity{}, false, nil
}

// ---- a software authenticator (ES256, no attestation) ---------------------

type authenticator struct {
	key     *ecdsa.PrivateKey
	credID  []byte
	counter uint32
	uv      bool
}

func newAuthenticator(t *testing.T) *authenticator {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 32)
	rand.Read(id)
	return &authenticator{key: k, credID: id, uv: true}
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func challengeOf(t *testing.T, options any) string {
	t.Helper()
	raw, _ := json.Marshal(options)
	var o struct{ Challenge string }
	if err := json.Unmarshal(raw, &o); err != nil || o.Challenge == "" {
		t.Fatalf("options without challenge: %s", raw)
	}
	return o.Challenge
}

func (a *authenticator) flags(extra byte) byte {
	f := byte(0x01) | extra // user present
	if a.uv {
		f |= 0x04
	}
	return f
}

func (a *authenticator) register(t *testing.T, rpID, origin, challenge string) json.RawMessage {
	t.Helper()
	rpHash := sha256.Sum256([]byte(rpID))
	x, y := a.key.PublicKey.X.FillBytes(make([]byte, 32)), a.key.PublicKey.Y.FillBytes(make([]byte, 32))
	cose, err := cbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: x, -3: y})
	if err != nil {
		t.Fatal(err)
	}
	authData := append([]byte{}, rpHash[:]...)
	authData = append(authData, a.flags(0x40)) // attested credential data present
	authData = binary.BigEndian.AppendUint32(authData, 0)
	authData = append(authData, make([]byte, 16)...) // AAGUID
	authData = binary.BigEndian.AppendUint16(authData, uint16(len(a.credID)))
	authData = append(authData, a.credID...)
	authData = append(authData, cose...)
	att, err := cbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": authData})
	if err != nil {
		t.Fatal(err)
	}
	cd, _ := json.Marshal(map[string]any{"type": "webauthn.create", "challenge": challenge, "origin": origin, "crossOrigin": false})
	out, _ := json.Marshal(map[string]any{
		"id": b64(a.credID), "rawId": b64(a.credID), "type": "public-key",
		"response":               map[string]any{"clientDataJSON": b64(cd), "attestationObject": b64(att), "transports": []string{"internal"}},
		"clientExtensionResults": map[string]any{},
	})
	return out
}

func (a *authenticator) assert(t *testing.T, rpID, origin, challenge, userHandle string) json.RawMessage {
	t.Helper()
	a.counter++
	rpHash := sha256.Sum256([]byte(rpID))
	authData := append([]byte{}, rpHash[:]...)
	authData = append(authData, a.flags(0))
	authData = binary.BigEndian.AppendUint32(authData, a.counter)
	cd, _ := json.Marshal(map[string]any{"type": "webauthn.get", "challenge": challenge, "origin": origin, "crossOrigin": false})
	cdHash := sha256.Sum256(cd)
	digest := sha256.Sum256(append(append([]byte{}, authData...), cdHash[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, a.key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	resp := map[string]any{"clientDataJSON": b64(cd), "authenticatorData": b64(authData), "signature": b64(sig)}
	if userHandle != "" {
		resp["userHandle"] = b64([]byte(userHandle))
	}
	out, _ := json.Marshal(map[string]any{"id": b64(a.credID), "rawId": b64(a.credID), "type": "public-key", "response": resp, "clientExtensionResults": map[string]any{}})
	return out
}

// ---- tests -----------------------------------------------------------------

const (
	origin = "https://app.example.test"
	rpID   = "app.example.test"
)

func req() *http.Request { return httptest.NewRequest("POST", origin+"/api/x", nil) }

func newService() (*Service, *memStore) {
	st := &memStore{}
	return &Service{RPName: "NeoAgent", Origins: httpx.ParseOrigins(""), Store: st, Accounts: accounts{}}, st
}

func register(t *testing.T, s *Service, a *authenticator, label string) {
	t.Helper()
	ctx := context.Background()
	options, pending, err := s.BeginRegistration(ctx, req(), "7")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	resp := a.register(t, rpID, origin, challengeOf(t, options))
	if err := s.CompleteRegistration(ctx, req(), "7", resp, label, pending); err != nil {
		t.Fatalf("complete: %v", err)
	}
}

func TestRegistrationStoresAKeyInTheNodeLayout(t *testing.T) {
	s, st := newService()
	a := newAuthenticator(t)
	register(t, s, a, "  My   laptop ")

	if len(st.rows) != 1 {
		t.Fatalf("rows: %d", len(st.rows))
	}
	c := st.rows[0]
	if c.CredentialID != b64(a.credID) || c.UserID != "7" || c.RPID != rpID || c.Label != "My laptop" ||
		c.Counter != 0 || !c.UserVerified || c.DeviceType != "singleDevice" || c.PublicKey == "" {
		t.Fatalf("stored: %+v", c)
	}
	if _, err := base64.RawURLEncoding.DecodeString(c.PublicKey); err != nil {
		t.Fatalf("public key must be base64url: %v", err)
	}

	// The same key cannot be added twice.
	options, pending, _ := s.BeginRegistration(context.Background(), req(), "7")
	err := s.CompleteRegistration(context.Background(), req(), "7", a.register(t, rpID, origin, challengeOf(t, options)), "", pending)
	var we *Error
	if !errors.As(err, &we) || we.Status != 409 {
		t.Fatalf("duplicate: %v", err)
	}

	// Existing keys are excluded from the next registration.
	options, _, _ = s.BeginRegistration(context.Background(), req(), "7")
	raw, _ := json.Marshal(options)
	if !strings.Contains(string(raw), b64(a.credID)) {
		t.Fatalf("existing key missing from excludeCredentials: %s", raw)
	}
}

func TestRegistrationOptionsShape(t *testing.T) {
	s, _ := newService()
	options, _, err := s.BeginRegistration(context.Background(), req(), "7")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(options)
	var o map[string]any
	json.Unmarshal(raw, &o)
	// Flat options, not wrapped in "publicKey": that is what the Flutter client reads.
	for _, k := range []string{"challenge", "rp", "user", "pubKeyCredParams", "timeout", "authenticatorSelection", "attestation"} {
		if _, ok := o[k]; !ok {
			t.Errorf("missing %q in %s", k, raw)
		}
	}
	if o["timeout"].(float64) != 120000 || o["attestation"] != "none" {
		t.Errorf("timeout/attestation: %v %v", o["timeout"], o["attestation"])
	}
	if rp := o["rp"].(map[string]any); rp["id"] != rpID || rp["name"] != "NeoAgent" {
		t.Errorf("rp: %v", rp)
	}
	if u := o["user"].(map[string]any); u["id"] != b64([]byte("7")) || u["name"] != "neo@example.com" {
		t.Errorf("user: %v", u)
	}
	sel := o["authenticatorSelection"].(map[string]any)
	if sel["residentKey"] != "preferred" || sel["userVerification"] != "preferred" {
		t.Errorf("selection: %v", sel)
	}
}

func TestLoginRoundTripAndReplayIsRefused(t *testing.T) {
	s, st := newService()
	ctx := context.Background()
	a := newAuthenticator(t)
	register(t, s, a, "")

	options, pending, err := s.BeginLogin(ctx, req(), "")
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.CompleteLogin(ctx, req(), a.assert(t, rpID, origin, challengeOf(t, options), "7"), pending)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if res.UserID != "7" || !res.UserVerified {
		t.Fatalf("result: %+v", res)
	}
	if st.rows[0].Counter != 1 {
		t.Fatalf("counter not stored: %d", st.rows[0].Counter)
	}

	// A fresh challenge but a signature counter that did not advance (a cloned
	// key) must be refused.
	options, pending, _ = s.BeginLogin(ctx, req(), "neo")
	a.counter = 0
	_, err = s.CompleteLogin(ctx, req(), a.assert(t, rpID, origin, challengeOf(t, options), "7"), pending)
	var we *Error
	if !errors.As(err, &we) || we.Status != 401 {
		t.Fatalf("cloned key accepted: %v", err)
	}
}

func TestLoginWithUsernameListsOnlyThatAccountsKeys(t *testing.T) {
	s, _ := newService()
	a := newAuthenticator(t)
	register(t, s, a, "")
	options, _, err := s.BeginLogin(context.Background(), req(), "NEO")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(options)
	if !strings.Contains(string(raw), `"allowCredentials"`) || !strings.Contains(string(raw), b64(a.credID)) {
		t.Fatalf("allowCredentials missing: %s", raw)
	}
	// Unknown usernames look exactly like known ones: no allow list, no error.
	options, _, err = s.BeginLogin(context.Background(), req(), "nobody")
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := json.Marshal(options); strings.Contains(string(raw), b64(a.credID)) {
		t.Fatalf("unknown username must not leak keys: %s", raw)
	}
}

func TestLoginRefusals(t *testing.T) {
	ctx := context.Background()
	status := func(err error) int {
		var we *Error
		if errors.As(err, &we) {
			return we.Status
		}
		return -1
	}

	empty, _ := newService()
	if _, _, err := empty.BeginLogin(ctx, req(), ""); status(err) != 409 {
		t.Fatalf("no keys registered: %v", err)
	}

	s, _ := newService()
	a := newAuthenticator(t)
	register(t, s, a, "")

	options, pending, _ := s.BeginLogin(ctx, req(), "")
	good := a.assert(t, rpID, origin, challengeOf(t, options), "7")

	// Wrong user handle.
	if _, err := s.CompleteLogin(ctx, req(), a.assert(t, rpID, origin, challengeOf(t, options), "8"), pending); status(err) != 401 {
		t.Fatalf("wrong user handle: %v", err)
	}
	// Expired challenge.
	expired := *pending
	expired.ExpiresAt = time.Now().Add(-time.Second).UnixMilli()
	if _, err := s.CompleteLogin(ctx, req(), good, &expired); status(err) != 400 {
		t.Fatalf("expired: %v", err)
	}
	if _, err := s.CompleteLogin(ctx, req(), good, nil); status(err) != 400 {
		t.Fatalf("no pending: %v", err)
	}
	// Challenge started on another address.
	other := *pending
	other.RPID = "other.example.test"
	if _, err := s.CompleteLogin(ctx, req(), good, &other); status(err) != 400 {
		t.Fatalf("other rp: %v", err)
	}
	// Unknown key.
	stranger := newAuthenticator(t)
	if _, err := s.CompleteLogin(ctx, req(), stranger.assert(t, rpID, origin, challengeOf(t, options), "7"), pending); status(err) != 401 {
		t.Fatalf("unregistered key: %v", err)
	}
	// Challenge mismatch (replaying a response for another challenge).
	other2, _, _ := s.BeginLogin(ctx, req(), "")
	if _, err := s.CompleteLogin(ctx, req(), a.assert(t, rpID, origin, challengeOf(t, other2), "7"), pending); status(err) != 401 {
		t.Fatalf("wrong challenge: %v", err)
	}
	// A ceremony from a disallowed browser origin.
	r := req()
	r.Header.Set("Origin", "https://evil.example")
	if _, _, err := s.BeginLogin(ctx, r, ""); status(err) != 403 {
		t.Fatalf("evil origin: %v", err)
	}
	// The untouched good response still works with its own pending state.
	if _, err := s.CompleteLogin(ctx, req(), good, pending); err != nil {
		t.Fatalf("good response rejected after the failures: %v", err)
	}
}

func TestLabelsAndLimits(t *testing.T) {
	s, st := newService()
	ctx := context.Background()
	register(t, s, newAuthenticator(t), "")
	register(t, s, newAuthenticator(t), "Phone")
	if st.rows[0].Label != "Security key 1" || st.rows[1].Label != "Phone" {
		t.Fatalf("labels: %q %q", st.rows[0].Label, st.rows[1].Label)
	}
	if _, err := s.Rename(ctx, "7", st.rows[0].RowID, strings.Repeat("x", 49)); err == nil {
		t.Fatal("over-long label accepted")
	}
	if _, err := s.Rename(ctx, "7", st.rows[0].RowID, "   "); err == nil {
		t.Fatal("blank label accepted")
	}
	list, err := s.Rename(ctx, "7", st.rows[0].RowID, "Desk key")
	if err != nil || list[0].Label != "Desk key" {
		t.Fatalf("%v %v", list, err)
	}
	if _, err := s.Rename(ctx, "8", st.rows[0].RowID, "x"); err == nil {
		t.Fatal("renaming another user's key must fail")
	}
	if _, err := s.Delete(ctx, "8", st.rows[0].RowID); err == nil {
		t.Fatal("deleting another user's key must fail")
	}
	if list, err = s.Delete(ctx, "7", st.rows[0].RowID); err != nil || len(list) != 1 {
		t.Fatalf("%v %v", list, err)
	}

	s.MaxPerUser = 1
	if _, _, err := s.BeginRegistration(ctx, req(), "7"); err == nil || !strings.Contains(err.Error(), "at most 1") {
		t.Fatalf("limit: %v", err)
	}
	if _, _, err := s.BeginRegistration(ctx, req(), "99"); err == nil {
		t.Fatal("unknown user")
	}
}

func TestPayloadJSON(t *testing.T) {
	raw, _ := json.Marshal(Credential{RowID: 3, Label: "k", RPID: "r", BackedUp: true, CreatedAt: "2026-01-01 00:00:00"}.Payload())
	want := `{"id":3,"label":"k","createdAt":"2026-01-01 00:00:00","lastUsedAt":null,"transports":[],"backedUp":true,"rpId":"r"}`
	if string(raw) != want {
		t.Fatalf("\n got %s\nwant %s", raw, want)
	}
}

func TestLoginByUsernameWithSeveralKeysAndWithoutAUserHandle(t *testing.T) {
	ctx := context.Background()
	s, _ := newService()
	first, second := newAuthenticator(t), newAuthenticator(t)
	register(t, s, first, "laptop")
	register(t, s, second, "phone")
	for _, handle := range []string{"7", ""} {
		for _, key := range []*authenticator{first, second} {
			options, pending, err := s.BeginLogin(ctx, req(), "neo")
			if err != nil {
				t.Fatal(err)
			}
			res, err := s.CompleteLogin(ctx, req(), key.assert(t, rpID, origin, challengeOf(t, options), handle), pending)
			if err != nil || res.UserID != "7" {
				t.Fatalf("handle %q: %+v %v", handle, res, err)
			}
		}
	}
}
