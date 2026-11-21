package llm

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

const maxOpenAIResponseBytes = 16 << 20

// OpenAIConfig 通用 OpenAI 兼容协议配置
type OpenAIConfig struct {
	BaseURL string // 例如 "https://api.deepseek.com/v1"
	APIKey  string
	Model   string // 例如 "deepseek-chat"
	Timeout time.Duration
}

// OpenAIProvider 统一 OpenAI 兼容驱动
type OpenAIProvider struct {
	mu              sync.RWMutex
	cfg             OpenAIConfig
	configuredModel string
	httpClient      *http.Client
}

// NewOpenAIProvider 实例化 OpenAI 协议驱动
func NewOpenAIProvider(cfg OpenAIConfig) *OpenAIProvider {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.openai.com/v1"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 4 * time.Minute
	}
	return &OpenAIProvider{
		cfg:             cfg,
		configuredModel: strings.TrimSpace(cfg.Model),
		httpClient: &http.Client{
			Timeout: cfg.Timeout,
			// Model APIs should not redirect requests, prompts, or credentials to
			// another host or to an internal service.
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// SetModel 动态切换当前模型
func (p *OpenAIProvider) SetModel(model string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cfg.Model = model
}

// GetModel 获取当前模型标识
func (p *OpenAIProvider) GetModel() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cfg.Model
}

func (p *OpenAIProvider) Name() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return "API (" + p.cfg.Model + ")"
}
