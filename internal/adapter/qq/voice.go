package qq

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"daidai/internal/core"
)

const (
	maxVoiceAudioBytes = 10 << 20 // 10MB
	maxVoiceWAVBytes   = 32 << 20
	voiceTimeout       = 25 * time.Second
)

// transcribeVoiceAudio 下载音频，通过 FFmpeg 规范化后交给 AGY 转录。
func (q *QQChannel) transcribeVoiceAudio(ctx context.Context, audioURL, fileName string) string {
	if strings.TrimSpace(audioURL) == "" {
		return ""
	}
	if ctx == nil {
		ctx = context.Background()
	}
	callCtx, cancel := context.WithTimeout(ctx, voiceTimeout)
	defer cancel()

	audioData, err := fetchVoiceAudio(callCtx, audioURL)
	if err != nil {
		slog.Warn("qq: 下载语音附件失败", "err", err)
		return ""
	}
	if len(audioData) == 0 {
		return ""
	}

	tmpDir, err := os.MkdirTemp("", "daidai-voice-*")
	if err != nil {
		slog.Warn("qq: 创建语音临时目录失败", "err", err)
		return ""
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	inputExt := strings.ToLower(filepath.Ext(fileName))
	if inputExt == "" {
		inputExt = ".wav"
	}
	inputPath := filepath.Join(tmpDir, "input"+inputExt)
	if err := os.WriteFile(inputPath, audioData, 0600); err != nil {
		slog.Warn("qq: 保存语音输入文件失败", "err", err)
		return ""
	}

	wavPath := filepath.Join(tmpDir, "audio.wav")
	wav, err := normalizeVoiceAudio(callCtx, audioData)
	if err != nil {
		slog.Warn("qq: ffmpeg 转码语音音频失败，尝试直接使用原文件", "err", err)
		wavPath = inputPath
	} else if err := os.WriteFile(wavPath, wav, 0600); err != nil {
		slog.Warn("qq: 保存转码语音失败", "err", err)
		return ""
	}

	// 检查是否有 agy 多模态 CLI
	if _, err := exec.LookPath("agy"); err != nil {
		slog.Warn("qq: 系统未安装 agy，无法进行模型语音转录")
		return ""
	}

	prompt := "请仅调用 read_file 工具读取以下音频文件并直接输出转录出的文字内容（无论说话者使用的是中文、日语、英语还是其他任何语言，请忠实输出说话者所用原语言的文本）。严禁运行其他工具或命令。如果无法识别或听不清则输出空，不要包含任何多余解释：\n" + wavPath
	args := []string{"-p", prompt, "--output-format", "text", "--mode", "plan", "--sandbox"}

	cmd := exec.CommandContext(callCtx, "agy", args...)
	cmd.Dir = tmpDir
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil && cmd.Process.Pid > 0 {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = 2 * time.Second
	stdout := core.NewLimitedBuffer(64 << 10)
	stderr := core.NewLimitedBuffer(16 << 10)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Run(); err != nil {
		slog.Warn("qq: agy 语音转录失败", "err", err, "stderr", stderr.String())
		return ""
	}

	return strings.TrimSpace(stdout.String())
}

// normalizeVoiceAudio decodes only the supplied bytes. Formats that reference
// other files or network resources cannot open them through FFmpeg protocols.
func normalizeVoiceAudio(ctx context.Context, data []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin",
		"-protocol_whitelist", "pipe,data", "-i", "pipe:0", "-vn", "-ar", "16000", "-ac", "1",
		"-c:a", "pcm_s16le", "-f", "wav", "pipe:1")
	cmd.Stdin = bytes.NewReader(data)
	cmd.WaitDelay = 2 * time.Second
	output := core.NewLimitedBuffer(maxVoiceWAVBytes)
	stderr := core.NewLimitedBuffer(16 << 10)
	cmd.Stdout, cmd.Stderr = output, stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("normalize voice: %s (%w)", strings.TrimSpace(stderr.String()), err)
	}
	if output.Truncated() {
		return nil, fmt.Errorf("normalized voice exceeds %d byte limit", maxVoiceWAVBytes)
	}
	return output.Bytes(), nil
}

func fetchVoiceAudio(ctx context.Context, audioURL string) ([]byte, error) {
	if err := core.ValidateTrustedMediaURL(audioURL); err != nil {
		return nil, fmt.Errorf("untrusted voice audio URL: %w", err)
	}
	if err := core.ValidateSafeExternalURL(ctx, audioURL); err != nil {
		return nil, fmt.Errorf("unsafe voice audio URL: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, audioURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	client := core.NewProtectedHTTPClient(15 * time.Second)
	defer client.CloseIdleConnections()
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("stopped after 5 voice redirects")
		}
		if err := core.ValidateTrustedMediaURL(req.URL.String()); err != nil {
			return fmt.Errorf("untrusted voice redirect: %w", err)
		}
		if err := core.ValidateSafeExternalURL(req.Context(), req.URL.String()); err != nil {
			return fmt.Errorf("unsafe voice redirect: %w", err)
		}
		return nil
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch voice status: %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxVoiceAudioBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxVoiceAudioBytes {
		return nil, fmt.Errorf("voice audio exceeded %d byte limit", maxVoiceAudioBytes)
	}
	return data, nil
}
