package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"daidai/internal/config"
	"daidai/internal/core"
	"daidai/internal/db"
	"daidai/internal/engine/agent"
	"daidai/internal/engine/memory"
	"daidai/internal/entity"
	"daidai/internal/provider/llm"
)

func newMemoryEngine(cfg *config.Config, database *db.Manager, provider core.LLMProvider) *memory.MemoryEngine {
	memEngine := memory.NewMemoryEngine(provider)
	memEngine.SetDBManager(database)
	embBaseURL := strings.TrimSpace(cfg.LLM.Embedding.BaseURL)
	if embBaseURL == "" {
		embBaseURL = "https://api.siliconflow.cn/v1"
	}
	embModel := strings.TrimSpace(cfg.LLM.Embedding.Model)
	if embModel == "" {
		embModel = "BAAI/bge-m3"
	}
	embAPIKey := strings.TrimSpace(cfg.LLM.Embedding.APIKey)
	if embAPIKey == "" {
		embAPIKey = strings.TrimSpace(os.Getenv("SILICONFLOW_API_KEY"))
	}
	if embAPIKey == "" {
		embAPIKey = strings.TrimSpace(os.Getenv("EMBEDDING_API_KEY"))
	}
	memEngine.SetEmbedder(llm.NewOpenAIProvider(llm.OpenAIConfig{
		BaseURL: embBaseURL,
		APIKey:  embAPIKey,
		Model:   embModel,
	}))

	return memEngine
}

func newDelegates(cfg *config.Config, database *db.Manager, agyWorker *agent.AGYWorker) (*entity.Delegates, error) {
	delegates := entity.NewDelegatesWithLimit(cfg.Agent.MaxConcurrent, agyWorker)
	executionTimeout, err := time.ParseDuration(cfg.Agent.ExecutionTimeout)
	if err != nil {
		return nil, fmt.Errorf("解析 agent.execution_timeout 失败: %w", err)
	}
	if err := delegates.SetExecutionTimeout(executionTimeout); err != nil {
		return nil, fmt.Errorf("配置工单执行超时失败: %w", err)
	}
	if err := delegates.AttachTicketStore(context.Background(), database, "qq"); err != nil {
		return nil, fmt.Errorf("初始化工程工单历史失败: %w", err)
	}
	for _, r := range cfg.Agent.Runners {
		if r.Name == "" || r.Name == "agy" || r.Command == "" {
			continue
		}
		delegates.Register(agent.NewCLIWorker(agent.CLIAgentConfig{
			Name:        r.Name,
			Description: r.Description,
			Command:     r.Command,
			Args:        r.Args,
			DefaultDir:  r.DefaultDir,
		}))
	}

	return delegates, nil
}
