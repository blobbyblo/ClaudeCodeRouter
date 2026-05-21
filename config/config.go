package config

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/BurntSushi/toml"
	"github.com/fsnotify/fsnotify"
)

// Manager holds the active Config and provides thread-safe access with hot-reload support.
type Manager struct {
	mu       sync.RWMutex
	current  *Config
	path     string
	onReload func() // called (in a goroutine) after each successful hot-reload
}

// SetReloadCallback registers a function to be called whenever the config is
// successfully reloaded from disk. It is called in a separate goroutine.
func (m *Manager) SetReloadCallback(fn func()) {
	m.mu.Lock()
	m.onReload = fn
	m.mu.Unlock()
}

// Load reads the config file at path and returns an initialised Manager.
// An error is returned only when the initial load fails; subsequent reload
// failures are logged and the last good config is kept.
func Load(path string) (*Manager, error) {
	cfg, err := parseFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: initial load from %q: %w", path, err)
	}

	m := &Manager{
		current: cfg,
		path:    path,
	}
	return m, nil
}

// Get returns a pointer to the most recently loaded Config.
// Callers must treat the returned value as read-only; it must not be modified.
func (m *Manager) Get() *Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current
}

// Watch starts a background goroutine that monitors the config file for changes
// and hot-reloads it. It blocks until ctx is cancelled or a fatal watcher error
// occurs. Call it in a separate goroutine if you don't want to block.
func (m *Manager) Watch(ctx context.Context) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("config: create fsnotify watcher: %w", err)
	}
	defer watcher.Close()

	if err := watcher.Add(m.path); err != nil {
		return fmt.Errorf("config: watch %q: %w", m.path, err)
	}

	slog.Info("config: watching for changes", "path", m.path)

	for {
		select {
		case <-ctx.Done():
			slog.Info("config: watcher stopped", "reason", ctx.Err())
			return ctx.Err()

		case event, ok := <-watcher.Events:
			if !ok {
				return fmt.Errorf("config: watcher events channel closed unexpectedly")
			}
			// React to write or rename events (editors often use rename-to-save).
			if event.Has(fsnotify.Write) || event.Has(fsnotify.Rename) {
				m.reload()
				// After a rename the old watch descriptor may be invalidated;
				// re-add to keep tracking the canonical path.
				if event.Has(fsnotify.Rename) {
					_ = watcher.Add(m.path)
				}
			}

		case watchErr, ok := <-watcher.Errors:
			if !ok {
				return fmt.Errorf("config: watcher errors channel closed unexpectedly")
			}
			slog.Error("config: watcher error", "err", watchErr)
		}
	}
}

// reload attempts to parse the config file and, on success, atomically swaps
// the current config. On failure it logs the error and leaves the current
// config unchanged so the server keeps running with a known-good config.
func (m *Manager) reload() {
	cfg, err := parseFile(m.path)
	if err != nil {
		slog.Error("config: reload failed, keeping last good config", "path", m.path, "err", err)
		return
	}

	m.mu.Lock()
	m.current = cfg
	cb := m.onReload
	m.mu.Unlock()

	slog.Info("config: reloaded successfully", "path", m.path)
	if cb != nil {
		go cb()
	}
}

// GenerateIfMissing writes a default config to path if the file doesn't exist.
func GenerateIfMissing(path string) error {
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return fmt.Errorf("config: create directory for %q: %w", path, err)
	}

	defaultCfg := Config{
		Server: ServerConfig{
			ClientPort: 4080,
			AdminPort:  4081,
			LogLevel:   "info",
		},
		Providers: make(map[string]ProviderConfig),
		Models:    []ModelConfig{},
	}

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("config: create %q: %w", path, err)
	}
	defer f.Close()

	enc := toml.NewEncoder(f)
	if err := enc.Encode(defaultCfg); err != nil {
		return fmt.Errorf("config: encode default config: %w", err)
	}
	return nil
}

func parseFile(path string) (*Config, error) {
	var cfg Config
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, err
	}
	applyDefaults(&cfg)
	return &cfg, nil
}

// applyDefaults fills in zero-value fields with sensible defaults.
func applyDefaults(cfg *Config) {
	if cfg.Server.ClientPort == 0 {
		cfg.Server.ClientPort = 4080
	}
	if cfg.Server.AdminPort == 0 {
		cfg.Server.AdminPort = 4081
	}
	if cfg.Server.LogLevel == "" {
		cfg.Server.LogLevel = "info"
	}
	if cfg.Providers == nil {
		cfg.Providers = make(map[string]ProviderConfig)
	}
}
