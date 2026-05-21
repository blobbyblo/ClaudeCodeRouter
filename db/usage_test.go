package db_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/bobbyo/ccr/db"
)

// openMemDB opens a fresh in-memory SQLite database and fails the test if
// anything goes wrong.
func openMemDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("Open(:memory:): %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// ---------------------------------------------------------------------------
// Client-key tests
// ---------------------------------------------------------------------------

func TestCreateAndGetKey(t *testing.T) {
	d := openMemDB(t)

	if err := db.CreateKey(d, "sk-ccr-aabbccddee112233445566", "alice"); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	k, err := db.GetKey(d, "sk-ccr-aabbccddee112233445566")
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}
	if k == nil {
		t.Fatal("GetKey returned nil for existing key")
	}
	if k.Name != "alice" {
		t.Errorf("Name: got %q, want %q", k.Name, "alice")
	}
	if k.Revoked {
		t.Error("expected Revoked=false for a freshly created key")
	}
	if !k.LastUsed.Valid {
		// last_used is NULL until first use — that is fine.
	}
}

func TestGetKey_NotFound(t *testing.T) {
	d := openMemDB(t)

	k, err := db.GetKey(d, "sk-ccr-doesnotexist000000000000")
	if err != nil {
		t.Fatalf("GetKey on missing key should not error: %v", err)
	}
	if k != nil {
		t.Errorf("expected nil for missing key, got %+v", k)
	}
}

func TestListKeys(t *testing.T) {
	d := openMemDB(t)

	keys := []struct{ id, name string }{
		{"sk-ccr-aaaaaaaaaaaaaaaaaaaaaaaaa1", "key-one"},
		{"sk-ccr-aaaaaaaaaaaaaaaaaaaaaaaaa2", "key-two"},
		{"sk-ccr-aaaaaaaaaaaaaaaaaaaaaaaaa3", "key-three"},
	}
	for _, kk := range keys {
		if err := db.CreateKey(d, kk.id, kk.name); err != nil {
			t.Fatalf("CreateKey(%q): %v", kk.id, err)
		}
	}

	list, err := db.ListKeys(d)
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	if len(list) != len(keys) {
		t.Fatalf("ListKeys: got %d rows, want %d", len(list), len(keys))
	}
}

func TestRevokeKey(t *testing.T) {
	d := openMemDB(t)
	id := "sk-ccr-revoke0000000000000000000"

	if err := db.CreateKey(d, id, "bob"); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if err := db.RevokeKey(d, id); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}

	k, err := db.GetKey(d, id)
	if err != nil {
		t.Fatalf("GetKey after revoke: %v", err)
	}
	if k == nil {
		t.Fatal("GetKey returned nil after revoke")
	}
	if !k.Revoked {
		t.Error("expected Revoked=true after RevokeKey")
	}
}

func TestRevokeKey_NoOp(t *testing.T) {
	d := openMemDB(t)
	// Revoking a non-existent key must not return an error.
	if err := db.RevokeKey(d, "sk-ccr-ghost000000000000000000"); err != nil {
		t.Errorf("RevokeKey on non-existent key: %v", err)
	}
}

func TestUpdateLastUsed(t *testing.T) {
	d := openMemDB(t)
	id := "sk-ccr-lastused00000000000000000"

	if err := db.CreateKey(d, id, "charlie"); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	before := time.Now().Unix()
	if err := db.UpdateLastUsed(d, id); err != nil {
		t.Fatalf("UpdateLastUsed: %v", err)
	}
	after := time.Now().Unix()

	k, err := db.GetKey(d, id)
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}
	if !k.LastUsed.Valid {
		t.Fatal("expected last_used to be non-NULL after UpdateLastUsed")
	}
	if k.LastUsed.Int64 < before || k.LastUsed.Int64 > after {
		t.Errorf("last_used %d out of expected range [%d, %d]", k.LastUsed.Int64, before, after)
	}
}

// ---------------------------------------------------------------------------
// Usage tests
// ---------------------------------------------------------------------------

func sampleRecord(clientKey, reqModel, routedModel, provider string) db.UsageRecord {
	return db.UsageRecord{
		Ts:             time.Now().Unix(),
		ClientKey:      clientKey,
		RequestedModel: reqModel,
		RoutedModel:    routedModel,
		Provider:       provider,
		InputTokens:    100,
		OutputTokens:   200,
		LatencyMs:      42,
		Status:         200,
		FallbackCount:  0,
	}
}

func TestWriteAndQueryUsage(t *testing.T) {
	d := openMemDB(t)

	r := sampleRecord("sk-ccr-testkey0000000000000000", "claude-3-opus", "claude-3-haiku", "anthropic")
	if err := db.WriteUsage(d, r); err != nil {
		t.Fatalf("WriteUsage: %v", err)
	}

	rows, err := db.QueryUsage(d, "", "", 0, 0, 0)
	if err != nil {
		t.Fatalf("QueryUsage: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("QueryUsage: got %d rows, want 1", len(rows))
	}

	got := rows[0]
	if got.ClientKey != r.ClientKey {
		t.Errorf("ClientKey: got %q, want %q", got.ClientKey, r.ClientKey)
	}
	if got.InputTokens != r.InputTokens {
		t.Errorf("InputTokens: got %d, want %d", got.InputTokens, r.InputTokens)
	}
	if got.OutputTokens != r.OutputTokens {
		t.Errorf("OutputTokens: got %d, want %d", got.OutputTokens, r.OutputTokens)
	}
	if got.Status != r.Status {
		t.Errorf("Status: got %d, want %d", got.Status, r.Status)
	}
}

func TestQueryUsage_KeyFilter(t *testing.T) {
	d := openMemDB(t)

	db.WriteUsage(d, sampleRecord("sk-ccr-key1000000000000000000", "model-a", "model-a", "prov"))
	db.WriteUsage(d, sampleRecord("sk-ccr-key2000000000000000000", "model-b", "model-b", "prov"))
	db.WriteUsage(d, sampleRecord("sk-ccr-key1000000000000000000", "model-c", "model-c", "prov"))

	rows, err := db.QueryUsage(d, "sk-ccr-key1000000000000000000", "", 0, 0, 0)
	if err != nil {
		t.Fatalf("QueryUsage with key filter: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("got %d rows, want 2", len(rows))
	}
	for _, r := range rows {
		if r.ClientKey != "sk-ccr-key1000000000000000000" {
			t.Errorf("unexpected client key %q in filtered results", r.ClientKey)
		}
	}
}

func TestQueryUsage_ModelFilter(t *testing.T) {
	d := openMemDB(t)

	db.WriteUsage(d, sampleRecord("k1", "claude-3-opus", "claude-3-opus", "anthropic"))
	db.WriteUsage(d, sampleRecord("k1", "gpt-4o", "gpt-4o", "openai"))
	db.WriteUsage(d, sampleRecord("k1", "claude-3-haiku", "claude-3-haiku", "anthropic"))

	rows, err := db.QueryUsage(d, "", "claude", 0, 0, 0)
	if err != nil {
		t.Fatalf("QueryUsage with model filter: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("got %d rows, want 2", len(rows))
	}
}

func TestQueryUsage_TimeRange(t *testing.T) {
	d := openMemDB(t)

	now := time.Now().Unix()
	old := db.UsageRecord{
		Ts: now - 1000, ClientKey: "k", RequestedModel: "m", RoutedModel: "m",
		Provider: "p", Status: 200,
	}
	recent := db.UsageRecord{
		Ts: now, ClientKey: "k", RequestedModel: "m", RoutedModel: "m",
		Provider: "p", Status: 200,
	}
	db.WriteUsage(d, old)
	db.WriteUsage(d, recent)

	rows, err := db.QueryUsage(d, "", "", now-500, now+1, 0)
	if err != nil {
		t.Fatalf("QueryUsage with time range: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("got %d rows, want 1", len(rows))
	}
}

func TestQueryUsage_Limit(t *testing.T) {
	d := openMemDB(t)

	for i := 0; i < 10; i++ {
		db.WriteUsage(d, sampleRecord("k", "m", "m", "p"))
	}

	rows, err := db.QueryUsage(d, "", "", 0, 0, 3)
	if err != nil {
		t.Fatalf("QueryUsage with limit: %v", err)
	}
	if len(rows) != 3 {
		t.Errorf("got %d rows, want 3", len(rows))
	}
}

func TestUsageSummary(t *testing.T) {
	d := openMemDB(t)

	now := time.Now().Unix()
	// 3 requests for model-a, 5 for model-b — all in the same hour bucket.
	for i := 0; i < 3; i++ {
		db.WriteUsage(d, db.UsageRecord{
			Ts: now, ClientKey: "keyA",
			RequestedModel: "model-a", RoutedModel: "model-a", Provider: "p",
			InputTokens: 10, OutputTokens: 20, Status: 200,
		})
	}
	for i := 0; i < 5; i++ {
		db.WriteUsage(d, db.UsageRecord{
			Ts: now, ClientKey: "keyB",
			RequestedModel: "model-b", RoutedModel: "model-b", Provider: "p",
			InputTokens: 5, OutputTokens: 15, Status: 200,
		})
	}

	summary, err := db.UsageSummary(d)
	if err != nil {
		t.Fatalf("UsageSummary: %v", err)
	}
	// Two distinct models → two rows in the same hour bucket.
	if len(summary) != 2 {
		t.Fatalf("got %d summary rows, want 2", len(summary))
	}

	// Both rows should be in the current hour bucket.
	wantHourTs := (now / 3600) * 3600
	for _, row := range summary {
		if row.HourTs != wantHourTs {
			t.Errorf("HourTs: got %d, want %d", row.HourTs, wantHourTs)
		}
	}

	// Build a map for easier assertions.
	byModel := map[string]db.UsageSummaryRow{}
	for _, row := range summary {
		byModel[row.Model] = row
	}

	a := byModel["model-a"]
	if a.RequestCount != 3 {
		t.Errorf("model-a RequestCount: got %d, want 3", a.RequestCount)
	}
	if a.InputTokens != 30 {
		t.Errorf("model-a InputTokens: got %d, want 30", a.InputTokens)
	}
	if a.OutputTokens != 60 {
		t.Errorf("model-a OutputTokens: got %d, want 60", a.OutputTokens)
	}

	b := byModel["model-b"]
	if b.RequestCount != 5 {
		t.Errorf("model-b RequestCount: got %d, want 5", b.RequestCount)
	}
	if b.InputTokens != 25 {
		t.Errorf("model-b InputTokens: got %d, want 25", b.InputTokens)
	}
	if b.OutputTokens != 75 {
		t.Errorf("model-b OutputTokens: got %d, want 75", b.OutputTokens)
	}
}

func TestUsageSummary_Empty(t *testing.T) {
	d := openMemDB(t)

	summary, err := db.UsageSummary(d)
	if err != nil {
		t.Fatalf("UsageSummary on empty DB: %v", err)
	}
	if len(summary) != 0 {
		t.Errorf("expected empty slice, got %d rows", len(summary))
	}
}

func TestWriteUsage_Autoincrement(t *testing.T) {
	d := openMemDB(t)

	for i := 0; i < 5; i++ {
		if err := db.WriteUsage(d, sampleRecord("k", "m", "m", "p")); err != nil {
			t.Fatalf("WriteUsage[%d]: %v", i, err)
		}
	}

	rows, err := db.QueryUsage(d, "", "", 0, 0, 0)
	if err != nil {
		t.Fatalf("QueryUsage: %v", err)
	}
	ids := make(map[int64]bool)
	for _, r := range rows {
		if ids[r.ID] {
			t.Errorf("duplicate ID %d in usage rows", r.ID)
		}
		ids[r.ID] = true
	}
}
