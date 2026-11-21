package entity

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func ResolveSandboxDir(defaultDir string) string {
	def := strings.TrimSpace(defaultDir)
	home, _ := os.UserHomeDir()
	if home != "" {
		home = filepath.Clean(home)
	}

	if def == "" {
		if home != "" {
			def = filepath.Join(home, "sandbox")
		} else {
			def = "data/workspace/sandbox"
		}
	} else if def == "~" || strings.HasPrefix(def, "~/") || strings.HasPrefix(def, "~"+string(filepath.Separator)) {
		sub := strings.TrimPrefix(def, "~")
		sub = strings.TrimPrefix(sub, "/")
		sub = strings.TrimPrefix(sub, string(filepath.Separator))
		if sub == "" {
			if home != "" {
				def = filepath.Join(home, "sandbox")
			} else {
				def = "data/workspace/sandbox"
			}
		} else if home != "" {
			def = filepath.Join(home, sub)
		} else {
			def = filepath.Clean(sub)
		}
	}

	if !filepath.IsAbs(def) {
		if abs, err := filepath.Abs(def); err == nil {
			def = abs
		}
	}
	def = filepath.Clean(def)
	if home != "" {
		home = resolvePathThroughExistingParent(home)
	}

	if def == "/" || (home != "" && def == home) {
		if home != "" {
			def = filepath.Join(home, "sandbox")
		} else {
			def = "/tmp/daidai/sandbox"
		}
	}

	resolved := resolvePathThroughExistingParent(def)
	if resolved == string(filepath.Separator) || (home != "" && resolved == home) {
		fallback := filepath.Join(home, "sandbox")
		if home == "" {
			fallback = "data/workspace/sandbox"
		}
		resolved = resolvePathThroughExistingParent(fallback)
		if resolved == string(filepath.Separator) || (home != "" && resolved == home) {
			resolved = resolvePathThroughExistingParent("data/workspace/sandbox")
		}
	}
	return resolved
}

func resolvePathThroughExistingParent(path string) string {
	absolute, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return filepath.Clean(path)
	}
	current := absolute
	var suffix []string
	for {
		if realPath, err := filepath.EvalSymlinks(current); err == nil {
			resolved := realPath
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return absolute
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

func canonicalWorkspacePath(path string) (string, error) {
	resolved := resolvePathThroughExistingParent(path)
	home, _ := os.UserHomeDir()
	if realHome, err := filepath.EvalSymlinks(home); err == nil {
		home = realHome
	}
	if resolved == string(filepath.Separator) || (home != "" && resolved == filepath.Clean(home)) {
		return "", fmt.Errorf("不能将根目录或整个用户主目录设为工作空间")
	}
	return resolved, nil
}

// NewWorkspaceManager 实例化工作空间中枢
