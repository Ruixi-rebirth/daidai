package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig_SopsAndSecretFiles(t *testing.T) {
	tmpDir := t.TempDir()

	secretAppIDFile := filepath.Join(tmpDir, "qq_appid")
	_ = os.WriteFile(secretAppIDFile, []byte("10249999\n"), 0600)

	secretKeyFile := filepath.Join(tmpDir, "llm_key")
	_ = os.WriteFile(secretKeyFile, []byte("sk-secret-test-key   \n"), 0600)

	yamlContent := `
qq:
  enabled: true
  app_id_file: "` + secretAppIDFile + `"
llm:
  api_key_file: "` + secretKeyFile + `"
`
	confPath := filepath.Join(tmpDir, "config.yaml")
	_ = os.WriteFile(confPath, []byte(yamlContent), 0600)

	cfg, err := LoadConfig(confPath)
	if err != nil {
		t.Fatalf("unexpected load error: %v", err)
	}

	if cfg.QQ.AppID != "10249999" {
		t.Errorf("expected AppID '10249999', got %q", cfg.QQ.AppID)
	}
	if cfg.LLM.APIKey != "sk-secret-test-key" {
		t.Errorf("expected APIKey 'sk-secret-test-key', got %q", cfg.LLM.APIKey)
	}

	// 验证环境变量回退
	t.Setenv("QQ_PARTNER_ID", "partner_env_123")
	cfgEnv, err := LoadConfig(confPath)
	if err != nil {
		t.Fatalf("unexpected load error: %v", err)
	}
	if cfgEnv.PartnerID != "partner_env_123" {
		t.Errorf("expected PartnerID 'partner_env_123', got %q", cfgEnv.PartnerID)
	}
}
