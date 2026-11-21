package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"daidai/internal/config"
	"daidai/internal/entity"
)

func resolveAGYRuntime(cfg *config.Config) (string, string, string, error) {
	var agyCommand, agyDesc, defaultWorkDir string
	for _, r := range cfg.Agent.Runners {
		if r.Name == "agy" {
			agyCommand, agyDesc, defaultWorkDir = r.Command, r.Description, r.DefaultDir
			break
		}
	}
	agyCommand = strings.TrimSpace(agyCommand)
	if agyCommand == "" {
		agyCommand = "agy"
	}
	// All AGY consumers use the service's working directory for configured
	// relative paths, before selecting their own workspaces or temporary dirs.
	if strings.ContainsRune(agyCommand, filepath.Separator) && !filepath.IsAbs(agyCommand) {
		absoluteCommand, err := filepath.Abs(agyCommand)
		if err != nil {
			return "", "", "", fmt.Errorf("无法解析 AGY 命令路径: %w", err)
		}
		agyCommand = absoluteCommand
	}
	defaultWorkDir = entity.ResolveSandboxDir(defaultWorkDir)
	if err := os.MkdirAll(defaultWorkDir, 0755); err != nil {
		return "", "", "", fmt.Errorf("初始化默认工作区 %q 失败: %w", defaultWorkDir, err)
	}

	return agyCommand, agyDesc, defaultWorkDir, nil
}
