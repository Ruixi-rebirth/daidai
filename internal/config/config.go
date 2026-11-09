package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	PartnerID     string   `yaml:"partner_id"`
	PartnerIDFile string   `yaml:"partner_id_file"`
	Collaborators []string `yaml:"collaborators"`

	Server struct {
		Port int `yaml:"port"`
	} `yaml:"server"`

	QQ struct {
		Enabled    bool   `yaml:"enabled"`
		AppID      string `yaml:"app_id"`
		AppIDFile  string `yaml:"app_id_file"`
		Secret     string `yaml:"secret"`
		SecretFile string `yaml:"secret_file"`
	} `yaml:"qq"`

	LLM struct {
		BaseURL    string `yaml:"base_url"`
		APIKey     string `yaml:"api_key"`
		APIKeyFile string `yaml:"api_key_file"`
		Model      string `yaml:"model"`
		Vision     struct {
			BaseURL    string `yaml:"base_url"`
			APIKey     string `yaml:"api_key"`
			APIKeyFile string `yaml:"api_key_file"`
			Model      string `yaml:"model"`
		} `yaml:"vision"`
		Embedding struct {
			BaseURL    string `yaml:"base_url"`
			APIKey     string `yaml:"api_key"`
			APIKeyFile string `yaml:"api_key_file"`
			Model      string `yaml:"model"`
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

func readSecretFileOrEnv(fileField, envVar, fallbackSopsPath string) string {
	if fileField != "" {
		if data, err := os.ReadFile(fileField); err == nil {
			if s := strings.TrimSpace(string(data)); s != "" {
				return s
			}
		}
	}
	if envVar != "" {
		if v := strings.TrimSpace(os.Getenv(envVar)); v != "" {
			return v
		}
	}
	if fallbackSopsPath != "" {
		if data, err := os.ReadFile(fallbackSopsPath); err == nil {
			if s := strings.TrimSpace(string(data)); s != "" {
				return s
			}
		}
	}
	return ""
}

// LoadConfig 从指定路径加载 yaml 配置，并自动穿透 sops-nix 机密路径与环境变量
func LoadConfig(path string) (*Config, error) {
	var cfg Config
	if path != "" {
		if data, err := os.ReadFile(path); err == nil {
			if err := yaml.Unmarshal(data, &cfg); err != nil {
				return nil, fmt.Errorf("unmarshal config error: %w", err)
			}
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read config file error: %w", err)
		}
	}

	if cfg.Server.Port <= 0 {
		cfg.Server.Port = 8080
	}
	if cfg.Agent.MaxConcurrent <= 0 {
		cfg.Agent.MaxConcurrent = 1
	}

	// sops-nix 自动解密路径与环境变量无感直通
	if cfg.PartnerID == "" {
		cfg.PartnerID = readSecretFileOrEnv(cfg.PartnerIDFile, "QQ_PARTNER_ID", "/run/secrets/qq_bot_owner_openid")
	}
	if cfg.QQ.AppID == "" {
		cfg.QQ.AppID = readSecretFileOrEnv(cfg.QQ.AppIDFile, "QQ_APP_ID", "/run/secrets/qq_bot_appid")
	}
	if cfg.QQ.Secret == "" {
		cfg.QQ.Secret = readSecretFileOrEnv(cfg.QQ.SecretFile, "QQ_SECRET", "/run/secrets/qq_bot_secret")
	}
	if cfg.LLM.APIKey == "" {
		cfg.LLM.APIKey = readSecretFileOrEnv(cfg.LLM.APIKeyFile, "LLM_API_KEY", "")
		if cfg.LLM.APIKey == "" {
			cfg.LLM.APIKey = readSecretFileOrEnv("", "DEEPSEEK_API_KEY", "/run/secrets/DEEPSEEK_API_KEY")
		}
		if cfg.LLM.APIKey == "" {
			cfg.LLM.APIKey = readSecretFileOrEnv("", "OPENAI_API_KEY", "/run/secrets/OPENAI_API_KEY")
		}
		if cfg.LLM.APIKey == "" {
			cfg.LLM.APIKey = readSecretFileOrEnv("", "GEMINI_API_KEY", "/run/secrets/GEMINI_API_KEY")
		}
	}
	if cfg.LLM.Vision.APIKey == "" {
		cfg.LLM.Vision.APIKey = readSecretFileOrEnv(cfg.LLM.Vision.APIKeyFile, "VISION_API_KEY", "/run/secrets/GEMINI_API_KEY")
	}
	if cfg.LLM.Embedding.APIKey == "" {
		cfg.LLM.Embedding.APIKey = readSecretFileOrEnv(cfg.LLM.Embedding.APIKeyFile, "EMBEDDING_API_KEY", "/run/secrets/SILICONFLOW_API_KEY")
	}

	return &cfg, nil
}
