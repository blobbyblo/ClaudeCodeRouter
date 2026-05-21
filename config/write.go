package config

import (
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
)

// UpsertModel adds or replaces a model entry and persists the config file.
func (m *Manager) UpsertModel(model ModelConfig, path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	cfg := m.current.clone()
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

	cfg := m.current.clone()
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

	cfg := m.current.clone()
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

	cfg := m.current.clone()
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

// writeConfig serialises cfg to the given TOML file.
func writeConfig(cfg *Config, path string) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("config.write: create %q: %w", path, err)
	}
	defer f.Close()

	enc := toml.NewEncoder(f)
	if err := enc.Encode(cfg); err != nil {
		return fmt.Errorf("config.write: encode: %w", err)
	}
	return nil
}
