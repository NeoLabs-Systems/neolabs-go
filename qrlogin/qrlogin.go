// Package qrlogin signs a screen in with a signed-in phone: the screen shows a
// short-lived code, the phone scans and approves it, the screen claims a
// session. It owns the challenge state machine
// (pending → scanned → approved → claimed, or declined/expired); minting the
// session itself is the app's job.
//
// Challenges live in user_qr_login_challenges, the table the Node servers use.
package qrlogin

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/apperr"
	"github.com/NeoLabs-Systems/neolabs-go/dbx"
	"github.com/NeoLabs-Systems/neolabs-go/passhash"
)

const (
	ttl               = 2 * time.Minute
	terminalRetention = time.Hour
	// LongPollMax is how long a status request is held open waiting for a change.
	LongPollMax = 25 * time.Second
)

func notFound() error {
	return apperr.New(404, "QR login request was not found or has expired.", "")
}
func conflict(msg string) error { return apperr.New(409, msg, "") }

// Metadata is what a device says about itself; unknown fields are dropped and
// every value is length-limited.
type Metadata struct {
	DeviceLabel   string `json:"deviceLabel"`
	PlatformLabel string `json:"platformLabel"`
	BrowserLabel  string `json:"browserLabel"`
	DeviceClass   string `json:"deviceClass"`
	AppMode       string `json:"appMode"`
	Platform      string `json:"platform"`
}

func trim(s string, max int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > max {
		return string(r[:max])
	}
	return s
}

// jsString is String(value || ”): what JavaScript makes of a JSON value.
func jsString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return ""
	case float64:
		if t == 0 {
			return ""
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		if f, err := t.Float64(); err == nil && f == 0 {
			return ""
		}
		return t.String()
	}
	return "[object Object]"
}

// NormalizeMetadata cleans client-supplied metadata (any JSON value).
func NormalizeMetadata(raw any) Metadata {
	m, _ := raw.(map[string]any)
	str := func(k string, max int) string { return trim(jsString(m[k]), max) }
	md := Metadata{
		DeviceLabel:   str("deviceLabel", 120),
		PlatformLabel: str("platformLabel", 60),
		BrowserLabel:  str("browserLabel", 60),
		DeviceClass:   strings.ToLower(str("deviceClass", 24)),
		AppMode:       strings.ToLower(str("appMode", 24)),
		Platform:      strings.ToLower(str("platform", 32)),
	}
	switch md.DeviceClass {
	case "mobile", "tablet", "desktop", "server", "unknown":
	default:
		md.DeviceClass = ""
	}
	return md
}

// Descriptor is how the signing-in device is shown on the phone.
type Descriptor struct {
	Label         string
	PlatformLabel string
	BrowserLabel  string
	DeviceClass   string
}

// Describe derives a readable device description from the User-Agent, preferring
// what the device reported about itself.
func Describe(userAgent string, md Metadata) Descriptor {
	lower := strings.ToLower(userAgent)
	has := func(s ...string) bool {
		for _, x := range s {
			if strings.Contains(lower, x) {
				return true
			}
		}
		return false
	}
	isTablet := has("ipad", "tablet")
	isMobile := !isTablet && (has("iphone") || (has("android") && has("mobile")))

	platform := md.PlatformLabel
	if platform == "" {
		switch {
		case has("iphone"):
			platform = "iPhone"
		case has("ipad"):
			platform = "iPad"
		case has("android"):
			platform = "Android"
		case has("mac os x", "macintosh"):
			platform = "macOS"
		case has("windows nt"):
			platform = "Windows"
		case has("linux", "x11"):
			platform = "Linux"
		case has("curl/", "wget/", "httpie/"):
			platform = "CLI session"
		default:
			platform = "Unknown device"
		}
	}
	browser := md.BrowserLabel
	if browser == "" {
		switch {
		case has("edg/"):
			browser = "Edge"
		case has("opr/", "opera/"):
			browser = "Opera"
		case has("brave/"):
			browser = "Brave"
		case has("firefox/"):
			browser = "Firefox"
		case has("chrome/", "crios/", "chromium/"):
			browser = "Chrome"
		case has("safari/") && has("version/"):
			browser = "Safari"
		case has("dart/"):
			browser = "Flutter app"
		case has("curl/"):
			browser = "curl"
		case has("wget/"):
			browser = "wget"
		case has("httpie/"):
			browser = "HTTPie"
		default:
			browser = "Unknown browser"
		}
	}
	class := md.DeviceClass
	if class == "" {
		switch {
		case platform == "CLI session":
			class = "server"
		case isTablet:
			class = "tablet"
		case isMobile:
			class = "mobile"
		case platform == "macOS" || platform == "Windows" || platform == "Linux":
			class = "desktop"
		default:
			class = "unknown"
		}
	}
	label := md.DeviceLabel
	if label == "" {
		parts := []string{platform}
		if browser != "" && browser != "Unknown browser" && browser != "Flutter app" {
			parts = append(parts, browser)
		} else if browser == "Flutter app" {
			parts = append(parts, "App")
		}
		label = strings.Join(parts, " · ")
	}
	return Descriptor{Label: label, PlatformLabel: platform, BrowserLabel: browser, DeviceClass: class}
}

// Request describes the screen that asks to sign in.
type Request struct {
	UserAgent     string
	IPAddress     string // "" when unknown
	LocationLabel string
	LocationJSON  string // JSON object text
	Metadata      any    // client-supplied, normalized on the way in
}

// Created is what the signing-in screen receives.
type Created struct {
	ChallengeID   string
	PollToken     string
	ApproveSecret string
	ExpiresAt     string
	Status        string
}

// Store runs the state machine.
type Store struct {
	DB  *dbx.DB
	Now func() time.Time

	mu      sync.Mutex
	waiters map[string][]chan struct{}
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func sqliteTime(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05") }

func parseTime(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, false
	}
	v = strings.Replace(v, " ", "T", 1)
	if !strings.HasSuffix(v, "Z") && !strings.Contains(v[10:], "+") && !strings.Contains(v[10:], "-") {
		v += "Z"
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	return t, err == nil
}

func randomToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic("qrlogin: crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("qrlogin: crypto/rand failed: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	const hex = "0123456789abcdef"
	out := make([]byte, 0, 36)
	for i, c := range b {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			out = append(out, '-')
		}
		out = append(out, hex[c>>4], hex[c&0x0f])
	}
	return string(out)
}

type row struct {
	id, status, expiresAt, createdAt          string
	userAgent, ip, locLabel, locJSON, reqMeta sql.NullString
	approvedBy                                sql.NullInt64
	approvedMeta, scannedMeta                 sql.NullString
	scannedAt, approvedAt, claimedAt          sql.NullString
}

const rowColumns = `id, COALESCE(status,'pending'), expires_at, COALESCE(created_at,''),
	request_user_agent, request_ip_address, request_location_label, request_location_json, request_metadata_json,
	approved_by_user_id, approved_metadata_json, scanned_metadata_json, scanned_at, approved_at, claimed_at`

func scanRow(r interface{ Scan(...any) error }) (*row, error) {
	var x row
	err := r.Scan(&x.id, &x.status, &x.expiresAt, &x.createdAt, &x.userAgent, &x.ip, &x.locLabel, &x.locJSON, &x.reqMeta,
		&x.approvedBy, &x.approvedMeta, &x.scannedMeta, &x.scannedAt, &x.approvedAt, &x.claimedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &x, err
}

func (s *Store) prune(ctx context.Context) {
	// Housekeeping must never fail a request.
	_, _ = s.DB.W.ExecContext(ctx, `UPDATE user_qr_login_challenges SET status = 'expired'
		WHERE status IN ('pending','scanned','approved') AND datetime(expires_at) <= datetime('now')`)
	_, _ = s.DB.W.ExecContext(ctx, `DELETE FROM user_qr_login_challenges
		WHERE status IN ('expired','claimed','declined')
		  AND datetime(COALESCE(claimed_at, expires_at, created_at)) <= datetime(?)`, sqliteTime(s.now().Add(-terminalRetention)))
}

func (s *Store) bySecret(ctx context.Context, id, secret string) (*row, error) {
	s.prune(ctx)
	return scanRow(s.DB.W.QueryRowContext(ctx, `SELECT `+rowColumns+` FROM user_qr_login_challenges
		WHERE id = ? AND approve_secret_hash = ? LIMIT 1`, strings.TrimSpace(id), passhash.DigestToken(secret)))
}

func (s *Store) byPollToken(ctx context.Context, id, token string) (*row, error) {
	s.prune(ctx)
	return scanRow(s.DB.W.QueryRowContext(ctx, `SELECT `+rowColumns+` FROM user_qr_login_challenges
		WHERE id = ? AND poll_token_hash = ? LIMIT 1`, strings.TrimSpace(id), passhash.DigestToken(token)))
}

func (s *Store) expired(r *row) bool {
	exp, ok := parseTime(r.expiresAt)
	return !ok || !exp.After(s.now())
}

func (s *Store) notify(id string) {
	s.mu.Lock()
	chans := s.waiters[id]
	delete(s.waiters, id)
	s.mu.Unlock()
	for _, c := range chans {
		close(c)
	}
}

func (s *Store) subscribe(id string) (ch chan struct{}, cancel func()) {
	ch = make(chan struct{})
	s.mu.Lock()
	if s.waiters == nil {
		s.waiters = map[string][]chan struct{}{}
	}
	s.waiters[id] = append(s.waiters[id], ch)
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		list := s.waiters[id]
		for i, c := range list {
			if c == ch {
				s.waiters[id] = append(list[:i], list[i+1:]...)
				break
			}
		}
	}
}

// Create starts a challenge.
func (s *Store) Create(ctx context.Context, req Request) (*Created, error) {
	s.prune(ctx)
	id, poll, secret := newUUID(), randomToken(), randomToken()
	expires := sqliteTime(s.now().Add(ttl))
	meta, _ := json.Marshal(NormalizeMetadata(req.Metadata))
	var ip any
	if req.IPAddress != "" {
		ip = req.IPAddress
	}
	loc := req.LocationJSON
	if loc == "" {
		loc = "{}"
	}
	_, err := s.DB.W.ExecContext(ctx, `INSERT INTO user_qr_login_challenges
		(id, poll_token_hash, approve_secret_hash, status, request_user_agent, request_ip_address,
		 request_location_label, request_location_json, request_metadata_json, expires_at)
		VALUES (?, ?, ?, 'pending', ?, ?, ?, ?, ?, ?)`,
		id, passhash.DigestToken(poll), passhash.DigestToken(secret), req.UserAgent, ip, req.LocationLabel, loc, string(meta), expires)
	if err != nil {
		return nil, err
	}
	return &Created{ChallengeID: id, PollToken: poll, ApproveSecret: secret, ExpiresAt: expires, Status: "pending"}, nil
}

// ---- serialization --------------------------------------------------------

func orNil(n sql.NullString) *string {
	if !n.Valid || n.String == "" {
		return nil
	}
	return &n.String
}

func jsonObject(n sql.NullString) map[string]any {
	out := map[string]any{}
	if n.Valid && n.String != "" {
		_ = json.Unmarshal([]byte(n.String), &out)
		if out == nil {
			out = map[string]any{}
		}
	}
	return out
}

func nonEmpty(v any, max int) *string {
	s, _ := v.(string)
	if s = trim(s, max); s == "" {
		return nil
	}
	return &s
}

// ScannedBy is the phone that scanned, as the signing-in screen may see it.
type ScannedBy struct {
	Label         string  `json:"label"`
	PlatformLabel *string `json:"platformLabel"`
	DeviceClass   *string `json:"deviceClass"`
}

// RequestedDevice is the screen that asked to sign in.
type RequestedDevice struct {
	Label         string         `json:"label"`
	PlatformLabel string         `json:"platformLabel"`
	BrowserLabel  string         `json:"browserLabel"`
	DeviceClass   string         `json:"deviceClass"`
	UserAgent     string         `json:"userAgent"`
	Metadata      map[string]any `json:"metadata"`
}

// RequestLocation is where the request came from.
type RequestLocation struct {
	Label     string  `json:"label"`
	IPAddress *string `json:"ipAddress"`
	City      *string `json:"city"`
	Region    *string `json:"region"`
	Country   *string `json:"country"`
	Timezone  *string `json:"timezone"`
}

// Approval records who approved.
type Approval struct {
	UserID   int64          `json:"userId"`
	Metadata map[string]any `json:"metadata"`
}

// Challenge is the full view the approving phone and the claim result use.
type Challenge struct {
	ChallengeID     string          `json:"challengeId"`
	Status          string          `json:"status"`
	RequestedAt     *string         `json:"requestedAt"`
	ExpiresAt       *string         `json:"expiresAt"`
	ScannedAt       *string         `json:"scannedAt"`
	ApprovedAt      *string         `json:"approvedAt"`
	ClaimedAt       *string         `json:"claimedAt"`
	ScannedBy       *ScannedBy      `json:"scannedBy"`
	RequestedDevice RequestedDevice `json:"requestedDevice"`
	RequestLocation RequestLocation `json:"requestLocation"`
	Approval        *Approval       `json:"approval"`
}

func serialize(r *row) Challenge {
	reqMeta := jsonObject(r.reqMeta)
	var md Metadata
	if raw, err := json.Marshal(reqMeta); err == nil {
		_ = json.Unmarshal(raw, &md)
	}
	d := Describe(r.userAgent.String, md)
	loc := jsonObject(r.locJSON)
	label := r.locLabel.String
	if label == "" {
		label = "Unknown"
	}
	c := Challenge{
		ChallengeID: r.id, Status: r.status,
		RequestedAt: orNil(sql.NullString{String: r.createdAt, Valid: true}),
		ExpiresAt:   orNil(sql.NullString{String: r.expiresAt, Valid: true}),
		ScannedAt:   orNil(r.scannedAt), ApprovedAt: orNil(r.approvedAt), ClaimedAt: orNil(r.claimedAt),
		RequestedDevice: RequestedDevice{Label: d.Label, PlatformLabel: d.PlatformLabel, BrowserLabel: d.BrowserLabel,
			DeviceClass: d.DeviceClass, UserAgent: r.userAgent.String, Metadata: reqMeta},
		RequestLocation: RequestLocation{Label: label, IPAddress: orNil(r.ip),
			City: nonEmpty(loc["city"], 80), Region: nonEmpty(loc["region"], 80),
			Country: nonEmpty(loc["country"], 80), Timezone: nonEmpty(loc["timezone"], 80)},
	}
	if r.scannedAt.Valid && r.scannedAt.String != "" {
		sm := jsonObject(r.scannedMeta)
		lbl, _ := sm["deviceLabel"].(string)
		if lbl == "" {
			lbl, _ = sm["platformLabel"].(string)
		}
		lbl = trim(lbl, 120)
		if lbl == "" {
			lbl = "Phone"
		}
		c.ScannedBy = &ScannedBy{Label: lbl, PlatformLabel: nonEmpty(sm["platformLabel"], 60), DeviceClass: nonEmpty(sm["deviceClass"], 24)}
	}
	if r.approvedBy.Valid && r.approvedBy.Int64 != 0 {
		c.Approval = &Approval{UserID: r.approvedBy.Int64, Metadata: jsonObject(r.approvedMeta)}
	}
	return c
}

// ---- phone side -----------------------------------------------------------

// Resolve is the approval preview for the phone.
func (s *Store) Resolve(ctx context.Context, id, secret string) (*Challenge, error) {
	r, err := s.bySecret(ctx, id, secret)
	if err != nil {
		return nil, err
	}
	if r == nil || s.expired(r) || r.status == "expired" {
		return nil, notFound()
	}
	c := serialize(r)
	return &c, nil
}

// MarkScanned records that a signed-in phone opened the approval preview, so the
// signing-in screen waits for the decision instead of showing the code.
func (s *Store) MarkScanned(ctx context.Context, id, secret string, scanner any) (*Challenge, error) {
	r, err := s.bySecret(ctx, id, secret)
	if err != nil {
		return nil, err
	}
	if r == nil || s.expired(r) || r.status == "expired" {
		return nil, notFound()
	}
	if r.status == "declined" {
		return nil, conflict("This QR login request was cancelled.")
	}
	if r.status == "pending" {
		meta, _ := json.Marshal(NormalizeMetadata(scanner))
		if _, err := s.DB.W.ExecContext(ctx, `UPDATE user_qr_login_challenges
			SET status = 'scanned', scanned_at = ?, scanned_metadata_json = ? WHERE id = ? AND status = 'pending'`,
			sqliteTime(s.now()), string(meta), r.id); err != nil {
			return nil, err
		}
		s.notify(r.id)
	}
	c, err := s.Resolve(ctx, id, secret)
	if err != nil {
		return nil, err
	}
	// To the phone a scanned request is still undecided; older app builds only
	// offer Approve for pending ones.
	if c.Status == "scanned" {
		c.Status = "pending"
	}
	return c, nil
}

func (s *Store) decline(ctx context.Context, r *row) error {
	if r == nil || s.expired(r) || r.status == "expired" {
		return notFound()
	}
	if r.status == "declined" {
		return nil
	}
	if r.status != "pending" && r.status != "scanned" {
		return conflict("This QR login request was already approved.")
	}
	if _, err := s.DB.W.ExecContext(ctx, `UPDATE user_qr_login_challenges SET status = 'declined'
		WHERE id = ? AND status IN ('pending','scanned')`, r.id); err != nil {
		return err
	}
	s.notify(r.id)
	return nil
}

// Decline is the phone turning the request down.
func (s *Store) Decline(ctx context.Context, id, secret string) error {
	r, err := s.bySecret(ctx, id, secret)
	if err != nil {
		return err
	}
	return s.decline(ctx, r)
}

// Cancel is the signing-in screen giving up, so a late approval fails.
func (s *Store) Cancel(ctx context.Context, id, pollToken string) error {
	r, err := s.byPollToken(ctx, id, pollToken)
	if err != nil {
		return err
	}
	return s.decline(ctx, r)
}

// Approve is the phone approving as userID. approverSessionHash is the keyed
// hash of the approving session (may be empty).
func (s *Store) Approve(ctx context.Context, id, secret string, userID int64, approverSessionHash string, approval any) (*Challenge, error) {
	r, err := s.bySecret(ctx, id, secret)
	if err != nil {
		return nil, err
	}
	if r == nil || s.expired(r) || r.status == "expired" {
		return nil, notFound()
	}
	switch {
	case r.status == "claimed":
		return nil, conflict("This QR login request was already used.")
	case r.status == "declined":
		return nil, conflict("This QR login request was cancelled.")
	case r.status == "approved" && r.approvedBy.Valid && r.approvedBy.Int64 != 0 && r.approvedBy.Int64 != userID:
		return nil, conflict("This QR login request was already approved.")
	}
	meta, _ := json.Marshal(NormalizeMetadata(approval))
	var hash any
	if approverSessionHash != "" {
		hash = approverSessionHash
	}
	if _, err := s.DB.W.ExecContext(ctx, `UPDATE user_qr_login_challenges
		SET status = 'approved', approved_by_user_id = ?, approved_session_hash = ?, approved_metadata_json = ?, approved_at = ?
		WHERE id = ? AND status IN ('pending','scanned','approved')`,
		userID, hash, string(meta), sqliteTime(s.now()), r.id); err != nil {
		return nil, err
	}
	s.notify(r.id)
	return s.Resolve(ctx, id, secret)
}

// ---- signing-in screen ------------------------------------------------------

// Poll is the answer to a status request. Fields that do not apply are omitted.
type Poll struct {
	ChallengeID     string          `json:"challengeId"`
	Status          string          `json:"status"`
	ExpiresAt       *string         `json:"expiresAt"`
	ApprovedAt      *string         `json:"approvedAt,omitempty"`
	ClaimedAt       *string         `json:"claimedAt,omitempty"`
	ScannedAt       *string         `json:"scannedAt,omitempty"`
	ScannedBy       *ScannedBy      `json:"scannedBy,omitempty"`
	RequestedDevice *PollDevice     `json:"requestedDevice,omitempty"`
	RequestLocation *PollLocation   `json:"requestLocation,omitempty"`
	LongPoll        bool            `json:"longPoll,omitempty"`
	nullables       map[string]bool `json:"-"`
}

// PollDevice and PollLocation carry only the label.
type PollDevice struct {
	Label string `json:"label"`
}
type PollLocation struct {
	Label string `json:"label"`
}

// MarshalJSON keeps approvedAt and claimedAt present (null) for live challenges
// like the Node server, and omits them for the expired shape.
func (p Poll) MarshalJSON() ([]byte, error) {
	m := map[string]any{"challengeId": p.ChallengeID, "status": p.Status, "expiresAt": p.ExpiresAt}
	if p.Status != "expired" || p.ApprovedAt != nil || p.ClaimedAt != nil {
		m["approvedAt"], m["claimedAt"] = p.ApprovedAt, p.ClaimedAt
	}
	if p.ScannedAt != nil {
		m["scannedAt"], m["scannedBy"] = p.ScannedAt, p.ScannedBy
		m["requestedDevice"], m["requestLocation"] = p.RequestedDevice, p.RequestLocation
	}
	if p.LongPoll {
		m["longPoll"] = true
	}
	return json.Marshal(m)
}

// PollStatus answers the signing-in screen's status request.
func (s *Store) PollStatus(ctx context.Context, id, pollToken string) (*Poll, error) {
	r, err := s.byPollToken(ctx, id, pollToken)
	if err != nil {
		return nil, err
	}
	if r == nil || s.expired(r) || r.status == "expired" {
		p := &Poll{ChallengeID: strings.TrimSpace(id), Status: "expired"}
		if r != nil {
			p.ExpiresAt = orNil(sql.NullString{String: r.expiresAt, Valid: true})
		}
		return p, nil
	}
	p := &Poll{ChallengeID: r.id, Status: r.status,
		ExpiresAt:  orNil(sql.NullString{String: r.expiresAt, Valid: true}),
		ApprovedAt: orNil(r.approvedAt), ClaimedAt: orNil(r.claimedAt)}
	if r.status == "scanned" {
		// Only what this screen already knows about itself, plus which device of
		// the account holder is deciding; the account stays unnamed until approval.
		c := serialize(r)
		p.ScannedAt, p.ScannedBy = c.ScannedAt, c.ScannedBy
		p.RequestedDevice = &PollDevice{Label: c.RequestedDevice.Label}
		p.RequestLocation = &PollLocation{Label: c.RequestLocation.Label}
	}
	return p, nil
}

// WaitForChange answers once the challenge leaves knownStatus: at once if it
// already has, else when the phone scans, approves or declines, the code
// expires, or after maxWait. ctx ends the wait when the client goes away.
func (s *Store) WaitForChange(ctx context.Context, id, pollToken, knownStatus string, maxWait time.Duration) (*Poll, error) {
	read := func() (*Poll, error) {
		p, err := s.PollStatus(ctx, id, pollToken)
		if p != nil {
			p.LongPoll = true
		}
		return p, err
	}
	cur, err := read()
	if err != nil || cur.Status != knownStatus || ctx.Err() != nil {
		return cur, err
	}
	wait := maxWait
	if exp, ok := parseTime(derefStr(cur.ExpiresAt)); ok {
		if until := exp.Sub(s.now()) + 250*time.Millisecond; until < wait {
			wait = until
		}
	}
	if wait < 0 {
		wait = 0
	}
	ch, cancel := s.subscribe(cur.ChallengeID)
	defer cancel()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ch:
	case <-timer.C:
	case <-ctx.Done():
	}
	return read()
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// Claimed is the result of a successful claim: whose session to mint.
type Claimed struct {
	UserID    int64
	Challenge Challenge
}

// Claim spends an approved challenge exactly once.
func (s *Store) Claim(ctx context.Context, id, pollToken string) (*Claimed, error) {
	r, err := s.byPollToken(ctx, id, pollToken)
	if err != nil {
		return nil, err
	}
	if r == nil || s.expired(r) || r.status == "expired" {
		return nil, notFound()
	}
	if r.status == "claimed" {
		return nil, conflict("This QR login request was already used.")
	}
	if r.status != "approved" || !r.approvedBy.Valid || r.approvedBy.Int64 == 0 {
		return nil, conflict("This QR login request is not approved yet.")
	}
	now := sqliteTime(s.now())
	res, err := s.DB.W.ExecContext(ctx, `UPDATE user_qr_login_challenges SET status = 'claimed', claimed_at = ?
		WHERE id = ? AND poll_token_hash = ? AND status = 'approved'`, now, r.id, passhash.DigestToken(pollToken))
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, conflict("This QR login request is no longer available.")
	}
	s.notify(r.id)
	r.status, r.claimedAt = "claimed", sql.NullString{String: now, Valid: true}
	return &Claimed{UserID: r.approvedBy.Int64, Challenge: serialize(r)}, nil
}
