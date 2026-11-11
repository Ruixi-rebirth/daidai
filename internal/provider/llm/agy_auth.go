package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unsafe"

	"daidai/internal/core"
	"daidai/internal/entity"

	"golang.org/x/sys/unix"
)

// ErrAGYNotLoggedIn 当 AGY 尚未登录或凭据损坏时返回的统一前置哨兵错误
var ErrAGYNotLoggedIn = errors.New("尚未登录 Google Antigravity 或认证凭据已失效，请先在对话中输入 /agy login 完成登录")

const maxAGYAuthPTYOutputBytes = 256 << 10
const maxAGYTokenFileBytes = 1 << 20

const agyOAuthURLPrefix = "https://accounts.google.com/o/oauth2/auth?"

func findAGYOAuthURL(text string) string {
	for offset := 0; offset < len(text); {
		match := strings.Index(text[offset:], agyOAuthURLPrefix)
		if match < 0 {
			return ""
		}
		start := offset + match
		end := start + len(agyOAuthURLPrefix)
		for end < len(text) && !isOAuthURLTerminator(text[end]) {
			end++
		}
		if end > start+len(agyOAuthURLPrefix) {
			return text[start:end]
		}
		offset = end
	}
	return ""
}

func isOAuthURLTerminator(char byte) bool {
	return char <= ' ' || char == 0x1b || char == '"' || char == '\'' || char == '<' || char == '>'
}

func stripANSIColorCodes(text string) string {
	var out strings.Builder
	out.Grow(len(text))
	for index := 0; index < len(text); {
		if text[index] == 0x1b && index+1 < len(text) && text[index+1] == '[' {
			end := index + 2
			for end < len(text) && ((text[end] >= '0' && text[end] <= '9') || text[end] == ';') {
				end++
			}
			if end < len(text) && ((text[end] >= 'a' && text[end] <= 'z') || (text[end] >= 'A' && text[end] <= 'Z')) {
				index = end + 1
				continue
			}
		}
		out.WriteByte(text[index])
		index++
	}
	return out.String()
}

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

type authSession struct {
	cmd       *exec.Cmd
	master    *os.File
	startedAt time.Time
}

// AGYAuthManager 管理 AGY 命令行环境的 Google OAuth 交互式登录
type AGYAuthManager struct {
	operationMu sync.Mutex
	mu          sync.Mutex
	command     string
	tokenPath   string
	active      *authSession
	timer       *time.Timer
}

// NewAGYAuthManager 实例化 AGY 认证管理器
func NewAGYAuthManager(command, customTokenPath string) *AGYAuthManager {
	if command == "" {
		command = "agy"
	}
	tPath := customTokenPath
	if tPath == "" {
		tPath = DefaultAGYTokenPath()
	}
	return &AGYAuthManager{
		command:   command,
		tokenPath: tPath,
	}
}

// GetStatus 查询当前 AGY 的 Google 账号认证状态
func (m *AGYAuthManager) GetStatus() AGYAuthStatus {
	m.mu.Lock()
	p := m.tokenPath
	m.mu.Unlock()
	return InspectAGYToken(p)
}

// CheckAuth 检查当前凭据是否有效可用
func (m *AGYAuthManager) CheckAuth() error {
	m.mu.Lock()
	p := m.tokenPath
	m.mu.Unlock()
	return CheckAGYAuth(p)
}

// StartLogin 发起 AGY Google OAuth 登录，返回授权网址
func (m *AGYAuthManager) StartLogin(ctx context.Context) (string, error) {
	m.operationMu.Lock()
	defer m.operationMu.Unlock()

	m.mu.Lock()

	// 清理旧会话
	m.cancelLocked()

	master, slave, err := openPTY(4096, 200)
	if err != nil {
		m.mu.Unlock()
		return "", fmt.Errorf("创建伪终端失败: %w", err)
	}

	// 独立于请求上下文启动子进程，避免请求返回后进程被提前终止
	cmd := exec.Command(m.command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	if err := cmd.Start(); err != nil {
		_ = slave.Close()
		_ = master.Close()
		m.mu.Unlock()
		return "", fmt.Errorf("启动 %s 失败: %w", m.command, err)
	}
	_ = slave.Close()

	sess := &authSession{
		cmd:       cmd,
		master:    master,
		startedAt: time.Now(),
	}
	m.active = sess

	// 5 分钟后未完成自动清理
	m.timer = time.AfterFunc(5*time.Minute, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.active == sess {
			slog.Info("agy_auth: 登录会话超时已自动清理")
			m.cancelLocked()
		}
	})
	m.mu.Unlock()

	fullOutput := core.NewLimitedBuffer(maxAGYAuthPTYOutputBytes)
	buf := make([]byte, 4096)
	sentEnter := false

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			m.mu.Lock()
			if m.active == sess {
				m.cancelLocked()
			}
			m.mu.Unlock()
			return "", ctx.Err()
		default:
		}
		m.mu.Lock()
		stillActive := m.active == sess
		m.mu.Unlock()
		if !stillActive {
			return "", fmt.Errorf("当前登录会话已取消或过期，请重新发起 /agy login")
		}

		_ = master.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		n, err := master.Read(buf)
		if n > 0 {
			_, _ = fullOutput.Write(buf[:n])
			if fullOutput.Truncated() {
				m.mu.Lock()
				if m.active == sess {
					m.cancelLocked()
				}
				m.mu.Unlock()
				return "", fmt.Errorf("AGY 登录终端输出超过 %d 字节上限，登录会话已终止", maxAGYAuthPTYOutputBytes)
			}
			str := fullOutput.String()
			cleanStr := stripANSIColorCodes(str)

			// 自动确认默认选项（1. Google OAuth）
			if !sentEnter && strings.Contains(cleanStr, "Select login method") {
				_, _ = master.Write([]byte("\r\n"))
				sentEnter = true
			}

			if match := findAGYOAuthURL(cleanStr); match != "" {
				return match, nil
			}
		}

		// 兜底：如果运行超过 2 秒仍未检测到提示词，也尝试发送回车
		if !sentEnter && time.Since(sess.startedAt) >= 2*time.Second {
			_, _ = master.Write([]byte("\r\n"))
			sentEnter = true
		}

		if err != nil {
			time.Sleep(50 * time.Millisecond)
		}
	}

	m.mu.Lock()
	if m.active == sess {
		m.cancelLocked()
	}
	m.mu.Unlock()
	return "", fmt.Errorf("等待提取 Google 登录授权链接超时，未检测到 OAuth 网址")
}

// SubmitCode 提交用户在网页登录后拿到的授权码
func (m *AGYAuthManager) SubmitCode(ctx context.Context, code string) (string, error) {
	m.operationMu.Lock()
	defer m.operationMu.Unlock()

	m.mu.Lock()
	sess := m.active
	m.mu.Unlock()

	trimmedCode := strings.TrimSpace(code)
	if trimmedCode == "" {
		return "", fmt.Errorf("授权码不能为空")
	}
	if strings.IndexFunc(trimmedCode, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("授权码包含不支持的控制字符")
	}
	if sess == nil || sess.master == nil {
		return "", fmt.Errorf("当前没有等待输入授权码的登录会话，请先输入 /agy login 发起登录")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	// 向 PTY 写入授权码并按回车
	_, err := sess.master.Write([]byte(trimmedCode + "\r\n"))
	if err != nil {
		return "", fmt.Errorf("写入授权码失败: %w", err)
	}

	// 等待最多 15 秒检测凭证更新，同时异步监听 PTY 输出捕获 Google OAuth 错误
	deadline := time.Now().Add(15 * time.Second)
	var finalEmail string
	ptyOut := core.NewLimitedBuffer(maxAGYAuthPTYOutputBytes)
	readBuf := make([]byte, 1024)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			m.mu.Lock()
			if m.active == sess {
				m.cancelLocked()
			}
			m.mu.Unlock()
			return "", ctx.Err()
		default:
		}
		m.mu.Lock()
		stillActive := m.active == sess
		m.mu.Unlock()
		if !stillActive {
			return "", fmt.Errorf("当前登录会话已取消或过期，请重新发起 /agy login")
		}

		_ = sess.master.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		if n, _ := sess.master.Read(readBuf); n > 0 {
			_, _ = ptyOut.Write(readBuf[:n])
			if ptyOut.Truncated() {
				m.mu.Lock()
				if m.active == sess {
					m.cancelLocked()
				}
				m.mu.Unlock()
				return "", fmt.Errorf("AGY 授权终端输出超过 %d 字节上限，登录会话已终止", maxAGYAuthPTYOutputBytes)
			}
			str := stripANSIColorCodes(ptyOut.String())
			if strings.Contains(str, "Got an error:") || strings.Contains(str, "token exchange failed") {
				m.mu.Lock()
				m.cancelLocked()
				m.mu.Unlock()
				return "", fmt.Errorf("授权交换失败（Google OAuth），请确认授权码来自最近一次 /agy login 链接且未过期")
			}
		}

		st := m.GetStatus()
		if st.IsLoggedIn && st.Email != "" {
			finalEmail = st.Email
			break
		}
	}

	m.mu.Lock()
	m.cancelLocked()
	m.mu.Unlock()

	if finalEmail != "" {
		return finalEmail, nil
	}

	// 兜底再次检查一次
	st := m.GetStatus()
	if st.IsLoggedIn && st.Email != "" {
		return st.Email, nil
	}

	return "", fmt.Errorf("授权码已提交，但未在预期时间内检测到凭据写入，请检查授权码是否正确或已过期")
}

// Cancel 取消当前登录会话
func (m *AGYAuthManager) Cancel() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cancelLocked()
}

// Logout 注销当前 Google 账号，安全清除本地 OAuth 凭据文件并终止在途登录会话
func (m *AGYAuthManager) Logout() error {
	m.operationMu.Lock()
	defer m.operationMu.Unlock()

	m.mu.Lock()
	defer m.mu.Unlock()

	m.cancelLocked()

	if m.tokenPath != "" {
		if err := os.Remove(m.tokenPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("清除凭据文件失败: %w", err)
		}
	}

	return nil
}

func (m *AGYAuthManager) cancelLocked() {
	if m.timer != nil {
		m.timer.Stop()
		m.timer = nil
	}
	if m.active != nil {
		if m.active.master != nil {
			_ = m.active.master.Close()
		}
		if m.active.cmd != nil && m.active.cmd.Process != nil {
			pid := m.active.cmd.Process.Pid
			if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
				_ = m.active.cmd.Process.Kill()
			}
			_ = m.active.cmd.Wait()
		}
		m.active = nil
	}
}

// openPTY 使用 Linux 原生系统调用分配虚拟伪终端并设置超宽终端和非阻塞模式
func openPTY(cols, rows uint16) (master, slave *os.File, err error) {
	mFd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/ptmx failed: %w", err)
	}

	unlock := 0
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(mFd), uintptr(unix.TIOCSPTLCK), uintptr(unsafe.Pointer(&unlock))); errno != 0 {
		_ = unix.Close(mFd)
		return nil, nil, fmt.Errorf("ioctl TIOCSPTLCK failed: %v", errno)
	}

	var ptn int32
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(mFd), uintptr(unix.TIOCGPTN), uintptr(unsafe.Pointer(&ptn))); errno != 0 {
		_ = unix.Close(mFd)
		return nil, nil, fmt.Errorf("ioctl TIOCGPTN failed: %v", errno)
	}

	slavePath := fmt.Sprintf("/dev/pts/%d", ptn)
	sFd, err := unix.Open(slavePath, unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = unix.Close(mFd)
		return nil, nil, fmt.Errorf("open slave %s failed: %w", slavePath, err)
	}

	if cols > 0 && rows > 0 {
		ws := unix.Winsize{Row: rows, Col: cols}
		_ = unix.IoctlSetWinsize(sFd, unix.TIOCSWINSZ, &ws)
	}
	_ = unix.SetNonblock(mFd, true)

	return os.NewFile(uintptr(mFd), "/dev/ptmx"), os.NewFile(uintptr(sFd), slavePath), nil
}
