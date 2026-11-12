package config

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server struct {
		Port int `yaml:"port"`
	} `yaml:"server"`

	QQ struct {
		Enabled       bool     `yaml:"enabled"`
		AppID         string   `yaml:"app_id"`
		Secret        string   `yaml:"secret"`
		OwnerOpenID   string   `yaml:"owner_openid"`
		Collaborators []string `yaml:"collaborators"`
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
		MaxConcurrent    int    `yaml:"max_concurrent"`
		ExecutionTimeout string `yaml:"execution_timeout"`
		Runners          []struct {
			Name        string   `yaml:"name"`
			Description string   `yaml:"description"`
			Command     string   `yaml:"command"`
			Args        []string `yaml:"args"`
			DefaultDir  string   `yaml:"default_dir"`
		} `yaml:"runners"`
	} `yaml:"agent"`

	OCI OCIConfig `yaml:"oci"`
}

// OCIConfig Oracle Cloud 账单、资源巡检与 IP 管理配置
type OCIConfig struct {
	Enabled           bool    `yaml:"enabled"`
	PrivateKey        string  `yaml:"private_key"`
	UserOCID          string  `yaml:"user_ocid"`
	TenancyOCID       string  `yaml:"tenancy_ocid"`
	Region            string  `yaml:"region"`
	CompartmentID     string  `yaml:"compartment_id"`
	DailyReportHour   *int    `yaml:"daily_report_hour"`
	DailyReportMinute *int    `yaml:"daily_report_minute"`
	CostAlertDelta    float64 `yaml:"cost_alert_delta"`
	Timezone          string  `yaml:"timezone"` // Local timezone used to format on-demand OCI bill reports.
}

// LoadConfig 从指定路径加载 yaml 配置
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file error: %w", err)
	}

	var cfg Config
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config error: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return nil, fmt.Errorf("unmarshal trailing config data: %w", err)
		}
		return nil, fmt.Errorf("config file must contain only one YAML document")
	}

	if cfg.Server.Port < 0 || cfg.Server.Port > 65535 {
		return nil, fmt.Errorf("server.port must be 0 (default) or between 1 and 65535")
	}
	if cfg.Server.Port == 0 {
		cfg.Server.Port = 8080
	}
	if cfg.Agent.MaxConcurrent < 0 {
		return nil, fmt.Errorf("agent.max_concurrent cannot be negative")
	}
	if cfg.Agent.MaxConcurrent == 0 {
		cfg.Agent.MaxConcurrent = 1
	}
	if cfg.Memory.WorkingLimit < 0 {
		return nil, fmt.Errorf("memory.working_limit cannot be negative")
	}
	if strings.TrimSpace(cfg.Agent.ExecutionTimeout) == "" {
		cfg.Agent.ExecutionTimeout = "30m"
	}
	executionTimeout, err := time.ParseDuration(strings.TrimSpace(cfg.Agent.ExecutionTimeout))
	if err != nil || executionTimeout < 0 {
		return nil, fmt.Errorf("agent.execution_timeout must be a non-negative Go duration (for example 30m or 0)")
	}
	cfg.Agent.ExecutionTimeout = executionTimeout.String()
	runnerNames := make(map[string]struct{}, len(cfg.Agent.Runners))
	for i := range cfg.Agent.Runners {
		runner := &cfg.Agent.Runners[i]
		runner.Name = strings.TrimSpace(runner.Name)
		runner.Description = strings.TrimSpace(runner.Description)
		runner.Command = strings.TrimSpace(runner.Command)
		runner.DefaultDir = strings.TrimSpace(runner.DefaultDir)
		if runner.Name == "" {
			return nil, fmt.Errorf("agent.runners[%d].name cannot be empty", i)
		}
		if strings.EqualFold(runner.Name, "agy") && runner.Name != "agy" {
			return nil, fmt.Errorf("agent.runners[%d].name %q conflicts with the reserved built-in name %q", i, runner.Name, "agy")
		}
		if runner.Name != "agy" && runner.Command == "" {
			return nil, fmt.Errorf("agent.runners[%d].command is required for runner %q", i, runner.Name)
		}
		if runner.Name != "agy" && runner.DefaultDir != "" && filepath.Clean(runner.DefaultDir) == "~" {
			return nil, fmt.Errorf("agent.runners[%d].default_dir cannot be the entire user home for runner %q", i, runner.Name)
		}
		if runner.Name != "agy" && !runnerPassesGoal(runner.Args) {
			return nil, fmt.Errorf("agent.runners[%d].args must include {{GOAL}} for runner %q", i, runner.Name)
		}
		nameKey := strings.ToLower(runner.Name)
		if _, exists := runnerNames[nameKey]; exists {
			return nil, fmt.Errorf("agent.runners contains duplicate runner name %q", runner.Name)
		}
		runnerNames[nameKey] = struct{}{}
	}
	if cfg.QQ.Enabled {
		var missing []string
		if strings.TrimSpace(cfg.QQ.AppID) == "" {
			missing = append(missing, "qq.app_id")
		}
		if strings.TrimSpace(cfg.QQ.Secret) == "" {
			missing = append(missing, "qq.secret")
		}
		if strings.TrimSpace(cfg.QQ.OwnerOpenID) == "" {
			missing = append(missing, "qq.owner_openid")
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("qq.enabled requires %s", strings.Join(missing, ", "))
		}
	}
	if cfg.OCI.Enabled {
		var missing []string
		if strings.TrimSpace(cfg.OCI.PrivateKey) == "" {
			missing = append(missing, "oci.private_key")
		}
		if strings.TrimSpace(cfg.OCI.UserOCID) == "" {
			missing = append(missing, "oci.user_ocid")
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("oci.enabled requires %s", strings.Join(missing, ", "))
		}
		if cfg.OCI.DailyReportHour != nil && (*cfg.OCI.DailyReportHour < 0 || *cfg.OCI.DailyReportHour > 23) {
			return nil, fmt.Errorf("oci.daily_report_hour must be between 0 and 23")
		}
		if cfg.OCI.DailyReportMinute != nil && (*cfg.OCI.DailyReportMinute < 0 || *cfg.OCI.DailyReportMinute > 59) {
			return nil, fmt.Errorf("oci.daily_report_minute must be between 0 and 59")
		}
		if cfg.OCI.CostAlertDelta < 0 {
			return nil, fmt.Errorf("oci.cost_alert_delta cannot be negative")
		}
		if cfg.OCI.CostAlertDelta == 0 {
			cfg.OCI.CostAlertDelta = 1
		}
		if timezone := strings.TrimSpace(cfg.OCI.Timezone); timezone != "" {
			cfg.OCI.Timezone = timezone
			if _, err := time.LoadLocation(timezone); err != nil {
				return nil, fmt.Errorf("oci.timezone is invalid: %w", err)
			}
		}
	}
	return &cfg, nil
}

func runnerPassesGoal(args []string) bool {
	for _, arg := range args {
		if strings.Contains(arg, "{{GOAL}}") {
			return true
		}
	}
	return false
}
