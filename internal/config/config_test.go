package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultAndFileConfigShareStartupDefaults(t *testing.T) {
	defaults := DefaultConfig()
	for _, content := range []string{"{}", "server:\n  port: 0\nagent:\n  max_concurrent: 0\n  execution_timeout: ''\n"} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		loaded, err := LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, cfg := range []*Config{defaults, loaded} {
			timeout, err := time.ParseDuration(cfg.Agent.ExecutionTimeout)
			if err != nil || timeout != 30*time.Minute {
				t.Fatalf("startup requires a valid execution timeout, got %q (%v)", cfg.Agent.ExecutionTimeout, err)
			}
			if cfg.Server.Port != 8080 || cfg.Agent.MaxConcurrent != 1 || cfg.QQ.Enabled || cfg.Telegram.Enabled {
				t.Fatalf("unexpected startup defaults: port=%d concurrent=%d qq=%v telegram=%v", cfg.Server.Port, cfg.Agent.MaxConcurrent, cfg.QQ.Enabled, cfg.Telegram.Enabled)
			}
		}
	}
}

func TestExplicitZeroExecutionTimeoutDisablesDeadline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("agent:\n  execution_timeout: '0'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if timeout, err := time.ParseDuration(cfg.Agent.ExecutionTimeout); err != nil || timeout != 0 {
		t.Fatalf("explicit zero timeout must stay disabled, got %q (%v)", cfg.Agent.ExecutionTimeout, err)
	}
}

func TestBriefingDefaultsAndValidation(t *testing.T) {
	if !DefaultConfig().Briefing.Enabled || DefaultConfig().Briefing.Time != "08:00" || DefaultConfig().Briefing.Timezone != "Asia/Singapore" {
		t.Fatal("incorrect briefing defaults")
	}
	for _, body := range []string{"briefing:\n  time: '25:00'\n", "briefing:\n  timezone: invalid/place\n"} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path); err == nil {
			t.Fatal("invalid briefing schedule accepted")
		}
	}
}

func TestBotNameHierarchy(t *testing.T) {
	var nilCfg *Config
	if nilCfg.GetBotName() != "呆呆" || nilCfg.QQBotName() != "呆呆" || nilCfg.TelegramBotName() != "呆呆" {
		t.Fatal("nil config must return 呆呆")
	}

	cfg := &Config{}
	if cfg.GetBotName() != "呆呆" || cfg.QQBotName() != "呆呆" || cfg.TelegramBotName() != "呆呆" {
		t.Fatal("empty config must default to 呆呆")
	}

	cfg.Name = "大白"
	if cfg.GetBotName() != "大白" || cfg.QQBotName() != "大白" || cfg.TelegramBotName() != "大白" {
		t.Fatal("global name must propagate to platforms")
	}

	cfg.QQ.BotName = "Q宝"
	if cfg.QQBotName() != "Q宝" || cfg.TelegramBotName() != "大白" {
		t.Fatal("platform override must take precedence")
	}
}
