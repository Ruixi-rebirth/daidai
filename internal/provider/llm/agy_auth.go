package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unsafe"

	"daidai/internal/entity"

	"golang.org/x/sys/unix"
)

// AGYAuthStatus AGY 认证凭据状态
type AGYAuthStatus = entity.AGYAuthStatusInfo

type authSession struct {
	cmd       *exec.Cmd
	master    *os.File
	startedAt time.Time
}

// AGYAuthManager 管理 AGY 命令行环境的 Google OAuth 交互式登录
type AGYAuthManager struct {
	mu        sync.Mutex
	command   string
	tokenPath string
	active    *authSession
	timer     *time.Timer
}

// NewAGYAuthManager 实例化 AGY 认证管理器
func NewAGYAuthManager(command, customTokenPath string) *AGYAuthManager {
	if command == "" {
		command = "agy"
	}
	tPath := customTokenPath
	if tPath == "" {
		home, err := os.UserHomeDir()
		if err == nil && home != "" {
			tPath = filepath.Join(home, ".gemini", "antigravity-cli", "antigravity-oauth-token")
		}
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

	var status AGYAuthStatus

	if p == "" {
		return status
	}

	data, err := os.ReadFile(p)
	if err != nil {
		return status
	}

	var parsed struct {
		IDTokenEmail string `json:"id_token_email"`
		Token        struct {
			AccessToken string    `json:"access_token"`
			Expiry      time.Time `json:"expiry"`
		} `json:"token"`
	}

	if err := json.Unmarshal(data, &parsed); err != nil {
		return status
	}

	if parsed.IDTokenEmail != "" {
		status.IsLoggedIn = true
		status.Email = parsed.IDTokenEmail
		status.Expiry = parsed.Token.Expiry
	}

	return status
}

// StartLogin 发起 AGY Google OAuth 登录，返回授权网址
func (m *AGYAuthManager) StartLogin(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 清理旧会话
	m.cancelLocked()

	master, slave, err := openPTY(4096, 200)
	if err != nil {
		return "", fmt.Errorf("创建伪终端失败: %w", err)
	}

	// 独立于请求上下文启动子进程，避免请求返回后进程被提前终止
	cmd := exec.Command(m.command)
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	if err := cmd.Start(); err != nil {
		slave.Close()
		master.Close()
		return "", fmt.Errorf("启动 %s 失败: %w", m.command, err)
	}
	slave.Close()

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

	urlRegex := regexp.MustCompile(`https://accounts\.google\.com/o/oauth2/auth\?[^\s\x1b\x00\x07\"\'<>]+`)
	var fullOutput strings.Builder
	buf := make([]byte, 4096)
	sentEnter := false

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			m.cancelLocked()
			return "", ctx.Err()
		default:
		}

		_ = master.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		n, err := master.Read(buf)
		if n > 0 {
			fullOutput.Write(buf[:n])
			str := fullOutput.String()

			// 自动确认默认选项（1. Google OAuth）
			if !sentEnter && strings.Contains(str, "Select login method") {
				_, _ = master.Write([]byte("\r\n"))
				sentEnter = true
			}

			if match := urlRegex.FindString(str); match != "" {
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

	m.cancelLocked()
	return "", fmt.Errorf("等待提取 Google 登录授权链接超时，未检测到 OAuth 网址")
}

// SubmitCode 提交用户在网页登录后拿到的授权码
func (m *AGYAuthManager) SubmitCode(ctx context.Context, code string) (string, error) {
	m.mu.Lock()
	sess := m.active
	m.mu.Unlock()

	trimmedCode := strings.TrimSpace(code)
	if trimmedCode == "" {
		return "", fmt.Errorf("授权码不能为空")
	}

	if sess == nil || sess.master == nil {
		return "", fmt.Errorf("当前没有等待输入授权码的登录会话，请先输入 /agy login 发起登录")
	}

	// 向 PTY 写入授权码并按回车
	_, err := sess.master.Write([]byte(trimmedCode + "\r\n"))
	if err != nil {
		return "", fmt.Errorf("写入授权码失败: %w", err)
	}

	// 等待最多 15 秒检测凭证更新，同时异步监听 PTY 输出捕获 Google OAuth 错误
	deadline := time.Now().Add(15 * time.Second)
	var finalEmail string
	var ptyOut strings.Builder
	readBuf := make([]byte, 1024)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}

		_ = sess.master.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		if n, _ := sess.master.Read(readBuf); n > 0 {
			ptyOut.Write(readBuf[:n])
			str := ptyOut.String()
			if strings.Contains(str, "Got an error:") || strings.Contains(str, "token exchange failed") {
				m.mu.Lock()
				m.cancelLocked()
				m.mu.Unlock()

				clean := regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`).ReplaceAllString(str, "")
				for _, line := range strings.Split(clean, "\n") {
					line = strings.TrimSpace(line)
					if strings.Contains(line, "Got an error:") || strings.Contains(line, "token exchange failed") {
						return "", fmt.Errorf("%s (请确认授权码来自最近一次 /agy login 链接且未过期)", line)
					}
				}
				return "", fmt.Errorf("授权交换失败: %s", strings.TrimSpace(clean))
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
			_ = m.active.cmd.Process.Kill()
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
		unix.Close(mFd)
		return nil, nil, fmt.Errorf("ioctl TIOCSPTLCK failed: %v", errno)
	}

	var ptn int32
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(mFd), uintptr(unix.TIOCGPTN), uintptr(unsafe.Pointer(&ptn))); errno != 0 {
		unix.Close(mFd)
		return nil, nil, fmt.Errorf("ioctl TIOCGPTN failed: %v", errno)
	}

	slavePath := fmt.Sprintf("/dev/pts/%d", ptn)
	sFd, err := unix.Open(slavePath, unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		unix.Close(mFd)
		return nil, nil, fmt.Errorf("open slave %s failed: %w", slavePath, err)
	}

	if cols > 0 && rows > 0 {
		ws := unix.Winsize{Row: rows, Col: cols}
		_ = unix.IoctlSetWinsize(sFd, unix.TIOCSWINSZ, &ws)
	}
	_ = unix.SetNonblock(mFd, true)

	return os.NewFile(uintptr(mFd), "/dev/ptmx"), os.NewFile(uintptr(sFd), slavePath), nil
}
