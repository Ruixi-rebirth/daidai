package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"daidai/internal/core"
)

const sandboxHomeGuestPath = "/sandbox/home"

var safeSystemEnvKeys = [...]string{
	"LOCALE_ARCHIVE",
	"TZ",
	"TZDIR",
	"SSL_CERT_FILE",
	"SSL_CERT_DIR",
	"FONTCONFIG_FILE",
	"TYPST_FONT_PATHS",
	"DAIDAI_MITEX_PATH",
}

// AGYCredentialGuard holds sensitive OAuth material loaded from the host token
// file so sandboxed subprocess output can be checked before leaving the provider.
type AGYCredentialGuard struct {
	needles []string
}

// ContainsSecret reports whether text contains raw or base64-encoded OAuth secrets.
func (g AGYCredentialGuard) ContainsSecret(text string) bool {
	if text == "" || len(g.needles) == 0 {
		return false
	}
	for _, needle := range g.needles {
		if needle != "" && strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

func addGuardSecret(needles []string, raw string) []string {
	s := strings.TrimSpace(raw)
	if len(s) < 6 {
		return needles
	}
	for _, variant := range []string{
		s,
		base64.StdEncoding.EncodeToString([]byte(s)),
		base64.RawStdEncoding.EncodeToString([]byte(s)),
		base64.URLEncoding.EncodeToString([]byte(s)),
		base64.RawURLEncoding.EncodeToString([]byte(s)),
	} {
		if len(variant) >= 6 {
			needles = append(needles, variant)
		}
	}
	return needles
}

// LoadAGYCredentialGuard validates the AGY OAuth token file and extracts
// credential needles for outbound exfiltration protection.
func LoadAGYCredentialGuard(customTokenPath ...string) ([]byte, AGYCredentialGuard, error) {
	tokenPath := DefaultAGYTokenPath()
	if len(customTokenPath) > 0 && strings.TrimSpace(customTokenPath[0]) != "" {
		tokenPath = strings.TrimSpace(customTokenPath[0])
	}
	if tokenPath == "" {
		return nil, AGYCredentialGuard{}, ErrAGYNotLoggedIn
	}
	data, err := readAGYTokenFile(tokenPath)
	if err != nil {
		return nil, AGYCredentialGuard{}, ErrAGYNotLoggedIn
	}
	var parsed struct {
		IDTokenEmail string `json:"id_token_email"`
		IDToken      string `json:"id_token"`
		Token        struct {
			AccessToken  string    `json:"access_token"`
			RefreshToken string    `json:"refresh_token"`
			IDToken      string    `json:"id_token"`
			Expiry       time.Time `json:"expiry"`
		} `json:"token"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, AGYCredentialGuard{}, ErrAGYNotLoggedIn
	}
	email := strings.TrimSpace(parsed.IDTokenEmail)
	accessToken := strings.TrimSpace(parsed.Token.AccessToken)
	refreshToken := strings.TrimSpace(parsed.Token.RefreshToken)
	if email == "" || (accessToken == "" && refreshToken == "") {
		return nil, AGYCredentialGuard{}, ErrAGYNotLoggedIn
	}
	if accessToken != "" && refreshToken == "" && !parsed.Token.Expiry.IsZero() && !parsed.Token.Expiry.After(time.Now()) {
		return nil, AGYCredentialGuard{}, ErrAGYNotLoggedIn
	}

	var needles []string
	needles = addGuardSecret(needles, email)
	needles = addGuardSecret(needles, parsed.IDToken)
	needles = addGuardSecret(needles, accessToken)
	needles = addGuardSecret(needles, refreshToken)
	needles = addGuardSecret(needles, parsed.Token.IDToken)
	return data, AGYCredentialGuard{needles: needles}, nil
}

// ShouldIsolateAGYCommand reports whether command refers to the real agy CLI
// rather than a unit-test fixture binary.
func ShouldIsolateAGYCommand(command string) bool {
	base := filepath.Base(strings.TrimSpace(command))
	return base == "agy"
}

func sanitizedSystemPath() string {
	var allowed []string
	seen := make(map[string]struct{})
	for _, entry := range filepath.SplitList(os.Getenv("PATH")) {
		clean := filepath.Clean(strings.TrimSpace(entry))
		if clean == "" || clean == "." {
			continue
		}
		if strings.HasPrefix(clean, "/nix/store/") ||
			strings.HasPrefix(clean, "/run/current-system/") ||
			clean == "/usr/bin" || clean == "/bin" {
			if _, ok := seen[clean]; !ok {
				seen[clean] = struct{}{}
				allowed = append(allowed, clean)
			}
		}
	}
	for _, def := range []string{"/run/current-system/sw/bin", "/usr/bin", "/bin"} {
		if _, ok := seen[def]; !ok {
			allowed = append(allowed, def)
		}
	}
	return strings.Join(allowed, string(filepath.ListSeparator))
}

func isSystemMountedBinary(path string) bool {
	for _, prefix := range []string{"/nix/", "/run/current-system/", "/usr/", "/bin/", "/lib/", "/lib64/"} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// IsolatedAGYCommand wraps an agy CLI invocation in a bubblewrap mount/user/pid
// namespace where /home/ruixi, /run/secrets, and /var/lib/daidai do not exist.
// If bwrap is unavailable on the host, it fails closed instead of running raw.
func IsolatedAGYCommand(ctx context.Context, command string, args []string, workDir string) (*exec.Cmd, func(), AGYCredentialGuard, error) {
	bwrapPath, err := exec.LookPath("bwrap")
	if err != nil {
		return nil, nil, AGYCredentialGuard{}, fmt.Errorf("未找到 bwrap 内核隔离沙箱，拒绝在无隔离环境下执行只读推理: %w", err)
	}
	rawCmd := strings.TrimSpace(command)
	if rawCmd == "" {
		rawCmd = "agy"
	}
	resolvedCmd, err := exec.LookPath(rawCmd)
	if err != nil {
		return nil, nil, AGYCredentialGuard{}, err
	}
	if evalCmd, evalErr := filepath.EvalSymlinks(resolvedCmd); evalErr == nil && evalCmd != "" {
		resolvedCmd = evalCmd
	}

	tokenBytes, guard, err := LoadAGYCredentialGuard()
	if err != nil {
		return nil, nil, AGYCredentialGuard{}, err
	}

	sandboxHome, err := os.MkdirTemp("", "daidai-agy-home-*")
	if err != nil {
		return nil, nil, AGYCredentialGuard{}, fmt.Errorf("创建 AGY 沙箱隔离主目录失败: %w", err)
	}
	var createdWorkDir string
	targetWorkDir := strings.TrimSpace(workDir)
	if targetWorkDir == "" {
		createdWorkDir, err = os.MkdirTemp("", "daidai-agy-work-*")
		if err != nil {
			_ = os.RemoveAll(sandboxHome)
			return nil, nil, AGYCredentialGuard{}, fmt.Errorf("创建 AGY 沙箱隔离工作目录失败: %w", err)
		}
		targetWorkDir = createdWorkDir
	} else {
		targetWorkDir, err = filepath.Abs(targetWorkDir)
		if err != nil {
			_ = os.RemoveAll(sandboxHome)
			return nil, nil, AGYCredentialGuard{}, fmt.Errorf("解析沙箱工作目录失败: %w", err)
		}
	}

	cleanup := func() {
		_ = os.RemoveAll(sandboxHome)
		if createdWorkDir != "" {
			_ = os.RemoveAll(createdWorkDir)
		}
	}

	sandboxTokenDir := filepath.Join(sandboxHome, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(sandboxTokenDir, 0700); err != nil {
		cleanup()
		return nil, nil, AGYCredentialGuard{}, fmt.Errorf("初始化沙箱认证目录失败: %w", err)
	}
	sandboxTokenFile := filepath.Join(sandboxTokenDir, "antigravity-oauth-token")
	if err := os.WriteFile(sandboxTokenFile, tokenBytes, 0600); err != nil {
		cleanup()
		return nil, nil, AGYCredentialGuard{}, fmt.Errorf("写入沙箱临时凭据失败: %w", err)
	}

	bwrapArgs := []string{
		"--unshare-all",
		"--share-net",
		"--die-with-parent",
		"--clearenv",
		"--setenv", "HOME", sandboxHomeGuestPath,
		"--setenv", "TMPDIR", "/tmp",
		"--setenv", "PATH", sanitizedSystemPath(),
	}
	for _, key := range safeSystemEnvKeys {
		val := strings.TrimSpace(os.Getenv(key))
		if val == "" {
			continue
		}
		if key == "TZ" || strings.HasPrefix(val, "/nix/store/") || strings.HasPrefix(val, "/etc/") {
			bwrapArgs = append(bwrapArgs, "--setenv", key, val)
		}
	}

	bwrapArgs = append(bwrapArgs,
		"--ro-bind-try", "/nix", "/nix",
		"--ro-bind-try", "/run/current-system", "/run/current-system",
		"--ro-bind-try", "/bin", "/bin",
		"--ro-bind-try", "/usr", "/usr",
		"--ro-bind-try", "/lib", "/lib",
		"--ro-bind-try", "/lib64", "/lib64",
		"--ro-bind-try", "/etc/resolv.conf", "/etc/resolv.conf",
		"--ro-bind-try", "/etc/hosts", "/etc/hosts",
		"--ro-bind-try", "/etc/nsswitch.conf", "/etc/nsswitch.conf",
		"--ro-bind-try", "/etc/localtime", "/etc/localtime",
		"--ro-bind-try", "/etc/ssl", "/etc/ssl",
		"--ro-bind-try", "/etc/static/ssl", "/etc/static/ssl",
		"--ro-bind-try", "/etc/ca-certificates", "/etc/ca-certificates",
		"--dev", "/dev",
		"--proc", "/proc",
		"--tmpfs", "/tmp",
		"--bind", sandboxHome, sandboxHomeGuestPath,
	)

	if hostHome, homeErr := os.UserHomeDir(); homeErr == nil && hostHome != "" {
		hostCLIDir := filepath.Join(hostHome, ".gemini", "antigravity-cli")
		guestCLIDir := filepath.Join(sandboxHomeGuestPath, ".gemini", "antigravity-cli")
		bwrapArgs = append(bwrapArgs,
			"--ro-bind-try", filepath.Join(hostCLIDir, "builtin"), filepath.Join(guestCLIDir, "builtin"),
			"--ro-bind-try", filepath.Join(hostCLIDir, "installation_id"), filepath.Join(guestCLIDir, "installation_id"),
		)
	}

	if !isSystemMountedBinary(resolvedCmd) {
		bwrapArgs = append(bwrapArgs, "--ro-bind", resolvedCmd, resolvedCmd)
	}

	bwrapArgs = append(bwrapArgs,
		"--bind", targetWorkDir, targetWorkDir,
		"--chdir", targetWorkDir,
		"--",
		resolvedCmd,
	)
	bwrapArgs = append(bwrapArgs, args...)

	cmd := core.CommandProcessGroup(ctx, bwrapPath, bwrapArgs...)
	cmd.Dir = targetWorkDir
	return cmd, cleanup, guard, nil
}
