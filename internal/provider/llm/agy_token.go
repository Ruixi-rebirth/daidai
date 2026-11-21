package llm

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"daidai/internal/core"
	"daidai/internal/entity"

	"golang.org/x/sys/unix"
)

const maxAGYTokenFileBytes = 1 << 20

// AGYAuthStatus AGY 认证凭据状态
type AGYAuthStatus = entity.AGYAuthStatusInfo

// DefaultAGYTokenPath 返回系统标准 AGY OAuth 凭据文件路径 (~/.gemini/antigravity-cli/antigravity-oauth-token)
func DefaultAGYTokenPath() string {
	home, err := os.UserHomeDir()
	if err == nil && home != "" {
		return filepath.Join(home, ".gemini", "antigravity-cli", "antigravity-oauth-token")
	}
	return ""
}

// InspectAGYToken 从指定或默认路径读取并解析 AGY 凭据状态
func InspectAGYToken(customTokenPath ...string) AGYAuthStatus {
	p := ""
	if len(customTokenPath) > 0 && customTokenPath[0] != "" {
		p = customTokenPath[0]
	} else {
		p = DefaultAGYTokenPath()
	}

	var status AGYAuthStatus
	if p == "" {
		return status
	}

	data, err := readAGYTokenFile(p)
	if err != nil {
		return status
	}

	var parsed struct {
		IDTokenEmail string `json:"id_token_email"`
		Token        struct {
			AccessToken  string    `json:"access_token"`
			RefreshToken string    `json:"refresh_token"`
			Expiry       time.Time `json:"expiry"`
		} `json:"token"`
	}

	if err := json.Unmarshal(data, &parsed); err != nil {
		return status
	}

	accessToken := strings.TrimSpace(parsed.Token.AccessToken)
	refreshToken := strings.TrimSpace(parsed.Token.RefreshToken)
	email := strings.TrimSpace(parsed.IDTokenEmail)
	if email == "" || (accessToken == "" && refreshToken == "") {
		return status
	}
	if accessToken != "" && refreshToken == "" && !parsed.Token.Expiry.IsZero() && !parsed.Token.Expiry.After(time.Now()) {
		return status
	}

	status.IsLoggedIn = true
	status.Email = email
	status.Expiry = parsed.Token.Expiry

	return status
}

func readAGYTokenFile(path string) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("invalid AGY token file descriptor")
	}
	defer func() { _ = file.Close() }()

	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		return nil, err
	}
	if info.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, fmt.Errorf("AGY token path is not a regular file")
	}
	if info.Uid != uint32(os.Getuid()) {
		return nil, fmt.Errorf("AGY token file is not owned by the current user")
	}
	if info.Mode&0077 != 0 {
		if err := unix.Fchmod(fd, 0600); err != nil {
			return nil, fmt.Errorf("secure AGY token file permissions: %w", err)
		}
	}
	if info.Size < 0 || info.Size > maxAGYTokenFileBytes {
		return nil, fmt.Errorf("AGY token file exceeds %d byte limit", maxAGYTokenFileBytes)
	}
	return core.ReadBounded(file, maxAGYTokenFileBytes)
}

// CheckAGYAuth 统一前置校验 AGY 认证状态。若未登录则立即 fail-fast 返回 ErrAGYNotLoggedIn，避免死等超时
func CheckAGYAuth(customTokenPath ...string) error {
	status := InspectAGYToken(customTokenPath...)
	if !status.IsLoggedIn {
		return ErrAGYNotLoggedIn
	}
	return nil
}
