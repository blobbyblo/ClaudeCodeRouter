package config

import (
	"os"
	"path/filepath"
	"testing"
)

// writeTempConfig writes content to a temp file and returns the path.
// The file is removed when the test completes.
func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "config-*.toml")
	if err != nil {
		t.Fatalf("create temp config: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close temp config: %v", err)
	}
	return f.Name()
}

// -----------------------------------------------------------------------------
// Tests
// -----------------------------------------------------------------------------

func TestLoad_ValidConfig(t *testing.T) {
	const content = `
[server]
client_port = 3000
admin_port  = 4000
log_level   = "debug"

[providers.anthropic]
base_url   = "https://api.anthropic.com"
convention = "anthropic"
api_keys   = ["ANTHROPIC_API_KEY"]

[providers.openai]
base_url   = "https://api.openai.com"
convention = "openai"
api_keys   = ["OPENAI_API_KEY"]

[[models]]
alias       = "fast"
fallback_to = "smart"

  [[models.providers]]
  provider = "anthropic"
  model_id = "claude-haiku-4-5"

[[models]]
alias = "smart"

  [[models.providers]]
  provider = "anthropic"
  model_id = "claude-sonnet-4-5"

  [[models.providers]]
  provider = "openai"
  model_id = "gpt-4o"
`
	path := writeTempConfig(t, content)

	mgr, err := Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	cfg := mgr.Get()
	if cfg == nil {
		t.Fatal("Get() returned nil")
	}

	// Server
	if cfg.Server.ClientPort != 3000 {
		t.Errorf("ClientPort: got %d, want 3000", cfg.Server.ClientPort)
	}
	if cfg.Server.AdminPort != 4000 {
		t.Errorf("AdminPort: got %d, want 4000", cfg.Server.AdminPort)
	}
	if cfg.Server.LogLevel != "debug" {
		t.Errorf("LogLevel: got %q, want \"debug\"", cfg.Server.LogLevel)
	}

	// Providers
	if len(cfg.Providers) != 2 {
		t.Errorf("Providers length: got %d, want 2", len(cfg.Providers))
	}
	ant, ok := cfg.Providers["anthropic"]
	if !ok {
		t.Fatal("Providers: missing \"anthropic\" key")
	}
	if ant.BaseURL != "https://api.anthropic.com" {
		t.Errorf("anthropic BaseURL: got %q, want \"https://api.anthropic.com\"", ant.BaseURL)
	}
	if ant.Convention != "anthropic" {
		t.Errorf("anthropic Convention: got %q, want \"anthropic\"", ant.Convention)
	}
	// APIKeys are now stored in the database, not in the config struct.

	// Models
	if len(cfg.Models) != 2 {
		t.Errorf("Models length: got %d, want 2", len(cfg.Models))
	}
	fast := cfg.Models[0]
	if fast.Alias != "fast" {
		t.Errorf("Models[0].Alias: got %q, want \"fast\"", fast.Alias)
	}
	if fast.FallbackTo != "smart" {
		t.Errorf("Models[0].FallbackTo: got %q, want \"smart\"", fast.FallbackTo)
	}
	if len(fast.Providers) != 1 {
		t.Errorf("Models[0].Providers length: got %d, want 1", len(fast.Providers))
	}

	smart := cfg.Models[1]
	if len(smart.Providers) != 2 {
		t.Errorf("Models[1].Providers length: got %d, want 2", len(smart.Providers))
	}
}

func TestLoad_MissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nonexistent.toml")

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() expected error for missing file, got nil")
	}
}

func TestLoad_EmptyFile_AppliesDefaults(t *testing.T) {
	// An empty TOML file is valid; applyDefaults should fill in zero-value fields.
	path := writeTempConfig(t, "")

	mgr, err := Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error on empty file: %v", err)
	}

	cfg := mgr.Get()

	if cfg.Server.ClientPort != 4080 {
		t.Errorf("default ClientPort: got %d, want 4080", cfg.Server.ClientPort)
	}
	if cfg.Server.AdminPort != 4081 {
		t.Errorf("default AdminPort: got %d, want 4081", cfg.Server.AdminPort)
	}
	if cfg.Server.LogLevel != "info" {
		t.Errorf("default LogLevel: got %q, want \"info\"", cfg.Server.LogLevel)
	}
	if cfg.Providers == nil {
		t.Error("default Providers: got nil, want non-nil map")
	}
}

func TestLoad_PartialServerConfig_DefaultsApplied(t *testing.T) {
	// Only one field set; the others should receive defaults.
	const content = `
[server]
log_level = "warn"
`
	path := writeTempConfig(t, content)

	mgr, err := Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	cfg := mgr.Get()

	if cfg.Server.LogLevel != "warn" {
		t.Errorf("LogLevel: got %q, want \"warn\"", cfg.Server.LogLevel)
	}
	if cfg.Server.ClientPort != 4080 {
		t.Errorf("default ClientPort: got %d, want 4080", cfg.Server.ClientPort)
	}
	if cfg.Server.AdminPort != 4081 {
		t.Errorf("default AdminPort: got %d, want 4081", cfg.Server.AdminPort)
	}
}

func TestLoad_InvalidTOML(t *testing.T) {
	const content = `
[server
client_port = "not-a-number"
`
	path := writeTempConfig(t, content)

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() expected error for invalid TOML, got nil")
	}
}

func TestManager_Get_ReturnsSamePointerWhileUnchanged(t *testing.T) {
	path := writeTempConfig(t, "")

	mgr, err := Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	first := mgr.Get()
	second := mgr.Get()

	if first != second {
		t.Error("Get() returned different pointers for unchanged config; expected same pointer")
	}
}

func TestManager_reload_SwapsPointerOnSuccess(t *testing.T) {
	const initial = `
[server]
client_port = 1111
`
	path := writeTempConfig(t, initial)

	mgr, err := Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	before := mgr.Get()
	if before.Server.ClientPort != 1111 {
		t.Fatalf("initial ClientPort: got %d, want 1111", before.Server.ClientPort)
	}

	// Overwrite the file with new content and call reload directly.
	const updated = `
[server]
client_port = 2222
`
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		t.Fatalf("write updated config: %v", err)
	}

	mgr.reload()

	after := mgr.Get()
	if after.Server.ClientPort != 2222 {
		t.Errorf("reloaded ClientPort: got %d, want 2222", after.Server.ClientPort)
	}
	if before == after {
		t.Error("reload() should have swapped the config pointer")
	}
}

func TestManager_reload_KeepsLastGoodConfigOnFailure(t *testing.T) {
	const initial = `
[server]
client_port = 5555
`
	path := writeTempConfig(t, initial)

	mgr, err := Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	good := mgr.Get()

	// Overwrite with broken TOML.
	if err := os.WriteFile(path, []byte("[[[[invalid"), 0o644); err != nil {
		t.Fatalf("write invalid config: %v", err)
	}

	mgr.reload()

	// The manager must still return the last good config.
	current := mgr.Get()
	if current != good {
		t.Error("reload() with bad config should keep the last good config pointer")
	}
	if current.Server.ClientPort != 5555 {
		t.Errorf("ClientPort after failed reload: got %d, want 5555", current.Server.ClientPort)
	}
}
