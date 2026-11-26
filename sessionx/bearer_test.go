package sessionx

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/dbx"
)

func TestBearerSessionsAreStoredByDigestAndRevocable(t *testing.T) {
	ctx := context.Background()
	db, err := dbx.Open(ctx, filepath.Join(t.TempDir(), "s.db"), dbx.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store, err := NewStore(ctx, db.W)
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := store.IssueBearer(ctx, "nrs_", map[string]any{"userId": "u-1"}, time.Hour)
	if err != nil || !strings.HasPrefix(token, "nrs_") {
		t.Fatal(token, err)
	}
	sid, data, err := store.Bearer(ctx, token)
	if err != nil || data["userId"] != "u-1" || strings.Contains(sid, token) {
		t.Fatal(sid, data, err)
	}
	if err := store.DestroyForUser(ctx, "u-1"); err != nil {
		t.Fatal(err)
	}
	if _, data, _ := store.Bearer(ctx, token); data != nil {
		t.Fatal("a revoked token still works")
	}
}
