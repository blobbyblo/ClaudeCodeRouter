package db

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// UsageRecord mirrors one row in the usage table.
type UsageRecord struct {
	ID             int64  `json:"id"`
	Ts             int64  `json:"ts"`
	ClientKey      string `json:"key_id"`
	RequestedModel string `json:"model"`
	RoutedModel    string `json:"routed_model"`
	Provider       string `json:"provider"`
	InputTokens    int64  `json:"input_tokens"`
	OutputTokens   int64  `json:"output_tokens"`
	LatencyMs      int64  `json:"latency_ms"`
	Status         int    `json:"status"`
	FallbackCount  int    `json:"fallback_count"`
}

// ClientKey mirrors one row in the client_keys table.
type ClientKey struct {
	ID        string
	Name      string
	CreatedAt int64
	LastUsed  sql.NullInt64
	Revoked   bool
}

// ---------------------------------------------------------------------------
// Usage writes
// ---------------------------------------------------------------------------

// WriteUsage inserts a completed request record.  The ID field is ignored on
// insert; the database assigns it via AUTOINCREMENT.
func WriteUsage(db *sql.DB, r UsageRecord) error {
	const q = `
INSERT INTO usage
    (ts, client_key, requested_model, routed_model, provider,
     input_tokens, output_tokens, latency_ms, status, fallback_count)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	_, err := db.Exec(q,
		r.Ts, r.ClientKey, r.RequestedModel, r.RoutedModel, r.Provider,
		r.InputTokens, r.OutputTokens, r.LatencyMs, r.Status, r.FallbackCount,
	)
	if err != nil {
		return fmt.Errorf("WriteUsage: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Usage reads
// ---------------------------------------------------------------------------

// QueryUsage returns usage rows filtered by optional client key and/or model
// substrings plus an optional [from, to] unix-second range.  limit <= 0 means
// no limit.  Results are ordered by ts DESC.
func QueryUsage(db *sql.DB, keyFilter, modelFilter string, from, to int64, limit int) ([]UsageRecord, error) {
	var (
		clauses []string
		args    []any
	)

	if keyFilter != "" {
		clauses = append(clauses, "client_key = ?")
		args = append(args, keyFilter)
	}
	if modelFilter != "" {
		clauses = append(clauses, "(requested_model LIKE ? OR routed_model LIKE ?)")
		like := "%" + modelFilter + "%"
		args = append(args, like, like)
	}
	if from > 0 {
		clauses = append(clauses, "ts >= ?")
		args = append(args, from)
	}
	if to > 0 {
		clauses = append(clauses, "ts <= ?")
		args = append(args, to)
	}

	q := `SELECT id, ts, client_key, requested_model, routed_model, provider,
                 input_tokens, output_tokens, latency_ms, status, fallback_count
          FROM usage`
	if len(clauses) > 0 {
		q += " WHERE " + strings.Join(clauses, " AND ")
	}
	q += " ORDER BY ts DESC"
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("QueryUsage: %w", err)
	}
	defer rows.Close()

	var out []UsageRecord
	for rows.Next() {
		var r UsageRecord
		if err := rows.Scan(
			&r.ID, &r.Ts, &r.ClientKey, &r.RequestedModel, &r.RoutedModel,
			&r.Provider, &r.InputTokens, &r.OutputTokens, &r.LatencyMs,
			&r.Status, &r.FallbackCount,
		); err != nil {
			return nil, fmt.Errorf("QueryUsage scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("QueryUsage rows: %w", err)
	}
	return out, nil
}

// UsageSummaryRow holds hourly aggregated statistics for the dashboard.
type UsageSummaryRow struct {
	HourTs       int64  `json:"hour_ts"`
	Model        string `json:"model"`
	RequestCount int64  `json:"request_count"`
	InputTokens  int64  `json:"total_input_tokens"`
	OutputTokens int64  `json:"total_output_tokens"`
}

// UsageSummary returns per-hour-per-model aggregates for the last 30 days,
// ordered by hour ascending. The dashboard uses this for the 24 h chart and
// today's stats cards.
func UsageSummary(db *sql.DB) ([]UsageSummaryRow, error) {
	const q = `
SELECT (ts / 3600) * 3600        AS hour_ts,
       requested_model            AS model,
       COUNT(*)                   AS request_count,
       SUM(input_tokens)          AS total_input_tokens,
       SUM(output_tokens)         AS total_output_tokens
FROM   usage
WHERE  ts >= (CAST(strftime('%s','now') AS INTEGER) - 30 * 86400)
GROUP  BY hour_ts, model
ORDER  BY hour_ts ASC`

	rows, err := db.Query(q)
	if err != nil {
		return nil, fmt.Errorf("UsageSummary: %w", err)
	}
	defer rows.Close()

	var out []UsageSummaryRow
	for rows.Next() {
		var s UsageSummaryRow
		if err := rows.Scan(&s.HourTs, &s.Model, &s.RequestCount, &s.InputTokens, &s.OutputTokens); err != nil {
			return nil, fmt.Errorf("UsageSummary scan: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("UsageSummary rows: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Client-key management
// ---------------------------------------------------------------------------

// CreateKey inserts a new, active client key.
func CreateKey(db *sql.DB, id, name string) error {
	const q = `INSERT INTO client_keys (id, name, created_at, revoked) VALUES (?, ?, ?, 0)`
	if _, err := db.Exec(q, id, name, time.Now().Unix()); err != nil {
		return fmt.Errorf("CreateKey: %w", err)
	}
	return nil
}

// GetKey returns the key with the given id, or nil if it does not exist.
func GetKey(db *sql.DB, id string) (*ClientKey, error) {
	const q = `SELECT id, name, created_at, last_used, revoked FROM client_keys WHERE id = ?`
	row := db.QueryRow(q, id)

	var (
		k       ClientKey
		revoked int
	)
	err := row.Scan(&k.ID, &k.Name, &k.CreatedAt, &k.LastUsed, &revoked)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("GetKey: %w", err)
	}
	k.Revoked = revoked != 0
	return &k, nil
}

// ListKeys returns all client keys ordered by creation time descending.
func ListKeys(db *sql.DB) ([]ClientKey, error) {
	const q = `SELECT id, name, created_at, last_used, revoked FROM client_keys ORDER BY created_at DESC`

	rows, err := db.Query(q)
	if err != nil {
		return nil, fmt.Errorf("ListKeys: %w", err)
	}
	defer rows.Close()

	var out []ClientKey
	for rows.Next() {
		var (
			k       ClientKey
			revoked int
		)
		if err := rows.Scan(&k.ID, &k.Name, &k.CreatedAt, &k.LastUsed, &revoked); err != nil {
			return nil, fmt.Errorf("ListKeys scan: %w", err)
		}
		k.Revoked = revoked != 0
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListKeys rows: %w", err)
	}
	return out, nil
}

// RevokeKey marks the key as revoked.  It is a no-op if the key does not
// exist.
func RevokeKey(db *sql.DB, id string) error {
	const q = `UPDATE client_keys SET revoked = 1 WHERE id = ?`
	if _, err := db.Exec(q, id); err != nil {
		return fmt.Errorf("RevokeKey: %w", err)
	}
	return nil
}

// UpdateLastUsed sets last_used to the current unix timestamp for the given
// key.  It is a no-op if the key does not exist.
func UpdateLastUsed(db *sql.DB, id string) error {
	const q = `UPDATE client_keys SET last_used = ? WHERE id = ?`
	if _, err := db.Exec(q, time.Now().Unix(), id); err != nil {
		return fmt.Errorf("UpdateLastUsed: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Provider key management
// ---------------------------------------------------------------------------

// ProviderKey is one row in the provider_keys table.
type ProviderKey struct {
	ID         int64  `json:"id"`
	ProviderID string `json:"provider_id"`
	KeyValue   string `json:"key_value"`
	CreatedAt  int64  `json:"created_at"`
}

// AddProviderKey inserts a new key for the given provider.
func AddProviderKey(db *sql.DB, providerID, keyValue string) (int64, error) {
	const q = `INSERT INTO provider_keys (provider_id, key_value, created_at) VALUES (?, ?, ?)`
	res, err := db.Exec(q, providerID, keyValue, time.Now().Unix())
	if err != nil {
		return 0, fmt.Errorf("AddProviderKey: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// ListProviderKeys returns all keys for a provider, ordered by insertion.
func ListProviderKeys(db *sql.DB, providerID string) ([]ProviderKey, error) {
	const q = `SELECT id, provider_id, key_value, created_at FROM provider_keys WHERE provider_id = ? ORDER BY id ASC`
	rows, err := db.Query(q, providerID)
	if err != nil {
		return nil, fmt.Errorf("ListProviderKeys: %w", err)
	}
	defer rows.Close()
	var out []ProviderKey
	for rows.Next() {
		var k ProviderKey
		if err := rows.Scan(&k.ID, &k.ProviderID, &k.KeyValue, &k.CreatedAt); err != nil {
			return nil, fmt.Errorf("ListProviderKeys scan: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// DeleteProviderKey removes a key by its row ID.
func DeleteProviderKey(db *sql.DB, id int64) error {
	const q = `DELETE FROM provider_keys WHERE id = ?`
	if _, err := db.Exec(q, id); err != nil {
		return fmt.Errorf("DeleteProviderKey: %w", err)
	}
	return nil
}

// GetProviderKeyValues returns the raw key strings for a provider (used by the router).
func GetProviderKeyValues(db *sql.DB, providerID string) ([]string, error) {
	const q = `SELECT key_value FROM provider_keys WHERE provider_id = ? ORDER BY id ASC`
	rows, err := db.Query(q, providerID)
	if err != nil {
		return nil, fmt.Errorf("GetProviderKeyValues: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var kv string
		if err := rows.Scan(&kv); err != nil {
			return nil, fmt.Errorf("GetProviderKeyValues scan: %w", err)
		}
		out = append(out, kv)
	}
	return out, rows.Err()
}

// CountProviderKeys returns the number of keys stored for a provider.
func CountProviderKeys(db *sql.DB, providerID string) (int, error) {
	const q = `SELECT COUNT(*) FROM provider_keys WHERE provider_id = ?`
	var n int
	err := db.QueryRow(q, providerID).Scan(&n)
	return n, err
}
