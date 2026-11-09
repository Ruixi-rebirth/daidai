package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	PartnerID     string   `yaml:"partner_id"`
	Collaborators []string `yaml:"collaborators"`

	Server struct {
		Port int `yaml:"port"`
	} `yaml:"server"`

	QQ struct {
		Enabled bool   `yaml:"enabled"`
		AppID   string `yaml:"app_id"`
		Secret  string `yaml:"secret"`
	} `yaml:"qq"`

	LLM struct {
		BaseURL string `yaml:"base_url"`
		APIKey  string `yaml:"api_key"`
		Model   string `yaml:"model"`
		Vision  struct {
			BaseURL string `yaml:"base_url"`
			APIKey  string `yaml:"api_key"`
			Model   string `yaml:"model"`
		} `yaml:"vision"`
		Embedding struct {
			BaseURL string `yaml:"base_url"`
			APIKey  string `yaml:"api_key"`
			Model   string `yaml:"model"`
		} `yaml:"embedding"`
	} `yaml:"llm"`

	Memory struct {
		WorkingLimit int `yaml:"working_limit"`
	} `yaml:"memory"`

	Plugins struct {
		NixSearch struct {
			Enabled bool `yaml:"enabled"`
		} `yaml:"nixsearch"`
	} `yaml:"plugins"`

	Agent struct {
		MaxConcurrent int `yaml:"max_concurrent"`
		Runners       []struct {
			Name        string   `yaml:"name"`
			Description string   `yaml:"description"`
			Command     string   `yaml:"command"`
			Args        []string `yaml:"args"`
			DefaultDir  string   `yaml:"default_dir"`
		} `yaml:"runners"`
	} `yaml:"agent"`
}

// LoadConfig 从指定路径加载 yaml 配置
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file error: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config error: %w", err)
	}

	if cfg.Server.Port <= 0 {
		cfg.Server.Port = 8080
	}
	if cfg.Agent.MaxConcurrent <= 0 {
		cfg.Agent.MaxConcurrent = 1
	}
	return &cfg, nil
}
