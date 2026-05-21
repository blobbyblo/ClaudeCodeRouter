package config

// Config is the top-level config struct loaded from config.toml
type Config struct {
	Server    ServerConfig              `toml:"server"`
	Providers map[string]ProviderConfig `toml:"providers"`
	Models    []ModelConfig             `toml:"models"`
}

type ServerConfig struct {
	ClientPort int    `toml:"client_port"`
	AdminPort  int    `toml:"admin_port"`
	LogLevel   string `toml:"log_level"`
}

type ProviderConfig struct {
	BaseURL    string `toml:"base_url"   json:"base_url"`
	Convention string `toml:"convention" json:"convention"`
}

type ModelConfig struct {
	Alias      string          `toml:"alias"       json:"alias"`
	FallbackTo string          `toml:"fallback_to" json:"fallback_to"`
	Providers  []ModelProvider `toml:"providers"   json:"providers"`
}

type ModelProvider struct {
	Provider string `toml:"provider"  json:"provider"`
	ModelID  string `toml:"model_id"  json:"model_id"`
}
