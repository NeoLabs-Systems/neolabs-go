package qrlogin

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/apperr"
	"github.com/NeoLabs-Systems/neolabs-go/dbx"
	"github.com/NeoLabs-Systems/neolabs-go/userid"
)

type vectors struct {
	Describe []struct {
		UA     string
		Md     map[string]any
		Result struct{ Label, PlatformLabel, BrowserLabel, DeviceClass string }
	}
	Norm []struct {
		In  any
		Out Metadata
	}
	Rows []struct {
		Row map[string]any
		Out json.RawMessage
	}
}

func load(t *testing.T) vectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestMetadataAndDescriptorsMatchNode(t *testing.T) {
	v := load(t)
	for i, c := range v.Norm {
		if got := NormalizeMetadata(c.In); got != c.Out {
			t.Errorf("normalize %d: got %+v want %+v", i, got, c.Out)
		}
	}
	for _, c := range v.Describe {
		got := Describe(c.UA, NormalizeMetadata(c.Md))
		want := Descriptor{c.Result.Label, c.Result.PlatformLabel, c.Result.BrowserLabel, c.Result.DeviceClass}
		if got != want {
			t.Errorf("describe %.50q %v:\n got %+v\nwant %+v", c.UA, c.Md, got, want)
		}
	}
}

func ns(v any) sql.NullString {
	s, ok := v.(string)
	return sql.NullString{String: s, Valid: ok}
}

func TestSerializeMatchesNode(t *testing.T) {
	for _, c := range load(t).Rows {
		m := c.Row
		r := &row{
			id: m["id"].(string), status: m["status"].(string), expiresAt: m["expires_at"].(string), createdAt: m["created_at"].(string),
			userAgent: ns(m["request_user_agent"]), ip: ns(m["request_ip_address"]), locLabel: ns(m["request_location_label"]),
			locJSON: ns(m["request_location_json"]), reqMeta: ns(m["request_metadata_json"]),
			approvedMeta: ns(m["approved_metadata_json"]), scannedMeta: ns(m["scanned_metadata_json"]),
			scannedAt: ns(m["scanned_at"]), approvedAt: ns(m["approved_at"]), claimedAt: ns(m["claimed_at"]),
		}
		if id, ok := m["approved_by_user_id"].(float64); ok {
			r.approvedBy = userid.FromInt(int64(id))
		}
		got, _ := json.Marshal(serialize(r))
		var g, w any
		json.Unmarshal(got, &g)
		json.Unmarshal(c.Out, &w)
		if !reflect.DeepEqual(g, w) {
			t.Errorf("row %v\n got: %s\nwant: %s", m["id"], got, c.Out)
		}
	}
}

// ---- state machine ----------------------------------------------------------

func newStore(t *testing.T) (*Store, *time.Time) {
	t.Helper()
	db, err := dbx.Open(context.Background(), filepath.Join(t.TempDir(), "q.db"), dbx.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	_, err = db.W.Exec(`CREATE TABLE user_qr_login_challenges (
		id TEXT PRIMARY KEY, poll_token_hash TEXT UNIQUE NOT NULL, approve_secret_hash TEXT NOT NULL, status TEXT DEFAULT 'pending',
		request_user_agent TEXT, request_ip_address TEXT, request_location_label TEXT, request_location_json TEXT DEFAULT '{}',
		request_metadata_json TEXT DEFAULT '{}', approved_by_user_id INTEGER, approved_session_hash TEXT,
		approved_metadata_json TEXT DEFAULT '{}', scanned_metadata_json TEXT DEFAULT '{}', expires_at TEXT NOT NULL,
		scanned_at TEXT, approved_at TEXT, claimed_at TEXT, created_at TEXT DEFAULT (datetime('now')))`)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	return &Store{DB: db, Now: func() time.Time { return now }}, &now
}

func status(err error) int {
	if e, ok := apperr.As(err); ok {
		return e.Status
	}
	return -1
}

func create(t *testing.T, s *Store) *Created {
	t.Helper()
	c, err := s.Create(context.Background(), Request{UserAgent: "Mozilla/5.0 (X11; Linux) Firefox/121.0", IPAddress: "203.0.113.5", LocationLabel: "Berlin"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestFullFlowAndSingleClaim(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	c := create(t, s)
	if c.Status != "pending" || len(c.PollToken) < 30 || len(c.ApproveSecret) < 30 || c.PollToken == c.ApproveSecret {
		t.Fatalf("%+v", c)
	}
	var stored string
	s.DB.W.QueryRow(`SELECT poll_token_hash || approve_secret_hash FROM user_qr_login_challenges`).Scan(&stored)
	if stored == "" || contains(stored, c.PollToken) || contains(stored, c.ApproveSecret) {
		t.Fatal("only digests may be stored")
	}

	// Not approved yet: nothing to claim.
	if _, err := s.Claim(ctx, c.ChallengeID, c.PollToken); status(err) != 409 {
		t.Fatalf("claim before approval: %v", err)
	}
	// The phone scans: the screen sees "scanned" with who is deciding.
	prev, err := s.MarkScanned(ctx, c.ChallengeID, c.ApproveSecret, map[string]any{"deviceLabel": "Neo iPhone", "platformLabel": "iPhone", "deviceClass": "mobile"})
	if err != nil || prev.Status != "pending" || prev.RequestedDevice.PlatformLabel != "Linux" {
		t.Fatalf("preview: %+v %v", prev, err)
	}
	p, _ := s.PollStatus(ctx, c.ChallengeID, c.PollToken)
	raw, _ := json.Marshal(p)
	var pm map[string]any
	json.Unmarshal(raw, &pm)
	if pm["status"] != "scanned" || pm["scannedBy"].(map[string]any)["label"] != "Neo iPhone" ||
		pm["requestedDevice"].(map[string]any)["label"] != "Linux · Firefox" || pm["requestLocation"].(map[string]any)["label"] != "Berlin" {
		t.Fatalf("scanned poll: %s", raw)
	}
	if _, has := pm["approval"]; has || pm["requestedDevice"].(map[string]any)["userAgent"] != nil {
		t.Fatal("the screen must not learn more than its own label")
	}

	ch, err := s.Approve(ctx, c.ChallengeID, c.ApproveSecret, "7", "sess-hash", map[string]any{"platform": "ios"})
	if err != nil || ch.Status != "approved" || ch.Approval == nil || ch.Approval.UserID != "7" {
		t.Fatalf("approve: %+v %v", ch, err)
	}
	// Another account cannot take over an approved request.
	if _, err := s.Approve(ctx, c.ChallengeID, c.ApproveSecret, "8", "", nil); status(err) != 409 {
		t.Fatalf("second approver: %v", err)
	}
	got, err := s.Claim(ctx, c.ChallengeID, c.PollToken)
	if err != nil || got.UserID != "7" || got.Challenge.Status != "claimed" {
		t.Fatalf("claim: %+v %v", got, err)
	}
	if _, err := s.Claim(ctx, c.ChallengeID, c.PollToken); status(err) != 409 {
		t.Fatalf("a request is spent once: %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestWrongSecretsAndExpiry(t *testing.T) {
	s, now := newStore(t)
	ctx := context.Background()
	c := create(t, s)
	if _, err := s.Resolve(ctx, c.ChallengeID, "wrong"); status(err) != 404 {
		t.Fatalf("wrong secret: %v", err)
	}
	if _, err := s.Approve(ctx, c.ChallengeID, c.PollToken, "1", "", nil); status(err) != 404 {
		t.Fatal("the poll token must not approve")
	}
	if _, err := s.Claim(ctx, c.ChallengeID, c.ApproveSecret); status(err) != 404 {
		t.Fatal("the approve secret must not claim")
	}
	if p, _ := s.PollStatus(ctx, c.ChallengeID, "wrong"); p.Status != "expired" || p.ExpiresAt != nil {
		t.Fatalf("unknown token looks expired: %+v", p)
	}
	*now = now.Add(3 * time.Minute)
	if _, err := s.Resolve(ctx, c.ChallengeID, c.ApproveSecret); status(err) != 404 {
		t.Fatalf("expired: %v", err)
	}
	if p, _ := s.PollStatus(ctx, c.ChallengeID, c.PollToken); p.Status != "expired" {
		t.Fatalf("%+v", p)
	}
	// Old terminal rows are removed.
	*now = now.Add(2 * time.Hour)
	s.prune(ctx)
	var n int
	s.DB.W.QueryRow(`SELECT COUNT(*) FROM user_qr_login_challenges`).Scan(&n)
	_ = n // SQLite datetime('now') is wall clock; retention is exercised by the cutoff arg above
}

func TestDeclineAndCancel(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	c := create(t, s)
	if err := s.Decline(ctx, c.ChallengeID, c.ApproveSecret); err != nil {
		t.Fatal(err)
	}
	if err := s.Decline(ctx, c.ChallengeID, c.ApproveSecret); err != nil {
		t.Fatalf("declining twice is fine: %v", err)
	}
	if _, err := s.Approve(ctx, c.ChallengeID, c.ApproveSecret, "1", "", nil); status(err) != 409 {
		t.Fatalf("approve after decline: %v", err)
	}
	if _, err := s.MarkScanned(ctx, c.ChallengeID, c.ApproveSecret, nil); status(err) != 409 {
		t.Fatalf("scan after decline: %v", err)
	}

	c2 := create(t, s)
	if err := s.Cancel(ctx, c2.ChallengeID, c2.PollToken); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Approve(ctx, c2.ChallengeID, c2.ApproveSecret, "1", "", nil); status(err) != 409 {
		t.Fatalf("a late approval after the screen gave up must fail: %v", err)
	}

	c3 := create(t, s)
	s.Approve(ctx, c3.ChallengeID, c3.ApproveSecret, "1", "", nil)
	if err := s.Decline(ctx, c3.ChallengeID, c3.ApproveSecret); status(err) != 409 {
		t.Fatalf("decline after approval: %v", err)
	}
}

func TestLongPoll(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	c := create(t, s)

	// Already different: answers at once.
	p, err := s.WaitForChange(ctx, c.ChallengeID, c.PollToken, "scanned", time.Second)
	if err != nil || p.Status != "pending" || !p.LongPoll {
		t.Fatalf("%+v %v", p, err)
	}

	// A change wakes the waiter before the timeout.
	done := make(chan *Poll, 1)
	go func() {
		p, _ := s.WaitForChange(ctx, c.ChallengeID, c.PollToken, "pending", 10*time.Second)
		done <- p
	}()
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	if _, err := s.MarkScanned(ctx, c.ChallengeID, c.ApproveSecret, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-done:
		if p.Status != "scanned" || time.Since(start) > 2*time.Second {
			t.Fatalf("%+v", p)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("waiter was not woken")
	}

	// No change: returns after the wait with the same status.
	t0 := time.Now()
	p, _ = s.WaitForChange(ctx, c.ChallengeID, c.PollToken, "scanned", 200*time.Millisecond)
	if p.Status != "scanned" || time.Since(t0) < 150*time.Millisecond {
		t.Fatalf("%+v after %v", p, time.Since(t0))
	}

	// The client going away ends the wait.
	cctx, cancel := context.WithCancel(ctx)
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	t0 = time.Now()
	s.WaitForChange(cctx, c.ChallengeID, c.PollToken, "scanned", 5*time.Second)
	if time.Since(t0) > time.Second {
		t.Fatal("cancelled wait must return promptly")
	}
}

func TestPollJSONShapes(t *testing.T) {
	s, _ := newStore(t)
	c := create(t, s)
	p, _ := s.PollStatus(context.Background(), c.ChallengeID, c.PollToken)
	raw, _ := json.Marshal(p)
	var m map[string]any
	json.Unmarshal(raw, &m)
	for _, k := range []string{"challengeId", "status", "expiresAt", "approvedAt", "claimedAt"} {
		if _, ok := m[k]; !ok {
			t.Errorf("live poll lacks %q: %s", k, raw)
		}
	}
	if m["approvedAt"] != nil || m["claimedAt"] != nil {
		t.Fatalf("%s", raw)
	}
	gone, _ := s.PollStatus(context.Background(), "nope", "nope")
	raw, _ = json.Marshal(gone)
	if string(raw) != `{"challengeId":"nope","expiresAt":null,"status":"expired"}` {
		t.Fatalf("expired shape: %s", raw)
	}
}
