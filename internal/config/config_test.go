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
