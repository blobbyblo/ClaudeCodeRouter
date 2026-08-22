package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// UpsertModel adds or replaces a model entry and persists the config file.
func (m *Manager) UpsertModel(model ModelConfig, path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	cfg, err := writableConfig(path)
	if err != nil {
		return err
	}
	updated := false
	for i, mod := range cfg.Models {
		if mod.Alias == model.Alias {
			cfg.Models[i] = model
			updated = true
			break
		}
	}
	if !updated {
		cfg.Models = append(cfg.Models, model)
	}

	if err := writeConfig(cfg, path); err != nil {
		return err
	}
	m.current = cfg
	return nil
}

// DeleteModel removes a model entry by alias and persists the config file.
func (m *Manager) DeleteModel(alias, path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	cfg, err := writableConfig(path)
	if err != nil {
		return err
	}
	models := cfg.Models[:0]
	for _, mod := range cfg.Models {
		if mod.Alias != alias {
			models = append(models, mod)
		}
	}
	cfg.Models = models

	if err := writeConfig(cfg, path); err != nil {
		return err
	}
	m.current = cfg
	return nil
}

// DeleteProvider removes a provider entry by ID and persists the config file.
func (m *Manager) DeleteProvider(id, path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	cfg, err := writableConfig(path)
	if err != nil {
		return err
	}
	delete(cfg.Providers, id)

	if err := writeConfig(cfg, path); err != nil {
		return err
	}
	m.current = cfg
	return nil
}

// UpsertProvider adds or replaces a provider entry and persists the config file.
func (m *Manager) UpsertProvider(id string, prov ProviderConfig, path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	cfg, err := writableConfig(path)
	if err != nil {
		return err
	}
	if cfg.Providers == nil {
		cfg.Providers = make(map[string]ProviderConfig)
	}
	cfg.Providers[id] = prov

	if err := writeConfig(cfg, path); err != nil {
		return err
	}
	m.current = cfg
	return nil
}

// writableConfig starts each dashboard mutation from the latest complete
// on-disk config. This prevents a delayed file-watcher reload from causing a
// later request to overwrite a model or provider that was just saved.
func writableConfig(path string) (*Config, error) {
	cfg, err := parseFile(path)
	if err != nil {
		return nil, fmt.Errorf("config.write: load current config: %w", err)
	}
	return cfg, nil
}

// clone makes a shallow copy of Config so mutations don't affect the live config
// until we've written to disk successfully.
func (c *Config) clone() *Config {
	cp := *c
	cp.Models = make([]ModelConfig, len(c.Models))
	copy(cp.Models, c.Models)
	cp.Providers = make(map[string]ProviderConfig, len(c.Providers))
	for k, v := range c.Providers {
		cp.Providers[k] = v
	}
	return &cp
}

// writeConfig atomically replaces the config file, so the hot-reload watcher
// never observes an empty or partially written document.
func writeConfig(cfg *Config, path string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.toml")
	if err != nil {
		return fmt.Errorf("config.write: create temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	enc := toml.NewEncoder(tmp)
	if err := enc.Encode(cfg); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("config.write: encode: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("config.write: close temporary file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("config.write: replace %q: %w", path, err)
	}
	return nil
}
