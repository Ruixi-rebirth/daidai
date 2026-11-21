package media

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"daidai/internal/core"
)

type AttachmentContent struct {
	Data      []byte // 读取到的前部内容（最大不超过 maxBytes）
	TotalSize int64  // 远程声明的总大小（若未知则为 len(Data)）
	Truncated bool   // 是否因超过读取预算而发生截断
}

// IsTextContent 判断数据是否为人类可读纯文本 (无 \x00 空字符，且有效 UTF-8)
// 遵循标准 Unix 与 Git 内容嗅探哲学，绝不依赖任何文件扩展名白名单。
// 针对流式截断场景，会自动容忍并剥离尾部因字节截断产生的不完整 UTF-8 字符。
func IsTextContent(data []byte) bool {
	if len(data) == 0 || bytes.IndexByte(data, 0) != -1 {
		return false
	}
	clean := trimIncompleteUTF8(data)
	return len(clean) > 0 && utf8.Valid(clean)
}

func trimIncompleteUTF8(data []byte) []byte {
	for len(data) > 0 {
		r, size := utf8.DecodeLastRune(data)
		if r != utf8.RuneError || size > 1 {
			break
		}
		data = data[:len(data)-1]
	}
	return data
}

// FetchContent 以内存安全的方式流式获取附件数据（最大不超过 maxBytes 字节），并感知真实文件大小与截断状态
func (i *Inbox) FetchContent(ctx context.Context, attachment *core.Attachment, maxBytes int64) (*AttachmentContent, error) {
	if attachment == nil || strings.TrimSpace(attachment.URL) == "" {
		return nil, fmt.Errorf("invalid attachment")
	}
	if maxBytes <= 0 {
		maxBytes = 256 * 1024
	}
	if i != nil && i.Sources != nil {
		data, handled, err := i.Sources.Fetch(ctx, attachment.URL, maxBytes)
		if handled {
			if err != nil {
				return nil, err
			}
			truncated := int64(len(data)) >= maxBytes
			return &AttachmentContent{
				Data:      data,
				TotalSize: int64(len(data)),
				Truncated: truncated,
			}, nil
		}
	}
	data, totalSize, truncated, err := fetchAttachmentStream(ctx, attachment.URL, maxBytes)
	if err != nil {
		return nil, err
	}
	return &AttachmentContent{
		Data:      data,
		TotalSize: totalSize,
		Truncated: truncated,
	}, nil
}

func (i *Inbox) removeStored(attachments []*core.Attachment) {
	for _, attachment := range attachments {
		if attachment != nil && attachment.Path != "" {
			_ = os.Remove(attachment.Path)
		}
	}
}

func fetchAttachmentStream(ctx context.Context, rawURL string, maxBytes int64) ([]byte, int64, bool, error) {
	if maxBytes <= 0 {
		maxBytes = 256 * 1024
	}
	if len(rawURL) > core.MaxAttachmentURLBytes {
		return nil, 0, false, fmt.Errorf("attachment URL exceeds %d byte limit", core.MaxAttachmentURLBytes)
	}
	if err := core.ValidateTrustedMediaURL(rawURL); err != nil {
		return nil, 0, false, err
	}
	if err := core.ValidateSafeExternalURL(ctx, rawURL); err != nil {
		return nil, 0, false, fmt.Errorf("unsafe attachment URL: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, false, err
	}
	client := core.NewProtectedHTTPClient(30 * time.Second)
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("stopped after 5 attachment redirects")
		}
		if err := core.ValidateTrustedMediaURL(req.URL.String()); err != nil {
			return fmt.Errorf("untrusted attachment redirect: %w", err)
		}
		if err := core.ValidateSafeExternalURL(req.Context(), req.URL.String()); err != nil {
			return fmt.Errorf("unsafe attachment redirect: %w", err)
		}
		return nil
	}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, 0, false, fmt.Errorf("attachment download status: %d", resp.StatusCode)
	}

	totalSize := resp.ContentLength
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, 0, false, err
	}
	truncated := false
	if int64(len(data)) > maxBytes {
		data = data[:maxBytes]
		truncated = true
	} else if totalSize > int64(len(data)) {
		truncated = true
	}
	return data, totalSize, truncated, nil
}

func (i *Inbox) writeFromPath(dir, sourcePath, name string) (string, error) {
	ext := safeAttachmentExtension(name)
	file, err := os.CreateTemp(dir, "qq-attachment-*"+ext)
	if err != nil {
		return "", fmt.Errorf("create attachment file: %w", err)
	}
	path := file.Name()
	if err := file.Chmod(0400); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("secure attachment file: %w", err)
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("open staged attachment: %w", err)
	}
	_, copyErr := io.Copy(file, source)
	closeSourceErr := source.Close()
	if copyErr != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("copy staged attachment: %w", copyErr)
	}
	if closeSourceErr != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("close staged attachment: %w", closeSourceErr)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("close attachment file: %w", err)
	}
	return path, nil
}

func (i *Inbox) seal(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read attachment directory before sealing: %w", err)
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		if err := os.Chmod(filepath.Join(dir, entry.Name()), 0400); err != nil {
			return fmt.Errorf("make attachment read-only: %w", err)
		}
	}
	if err := os.Chmod(dir, 0500); err != nil {
		return fmt.Errorf("make attachment directory read-only: %w", err)
	}
	return nil
}

func (i *Inbox) cleanupExpired(now time.Time) error {
	entries, err := os.ReadDir(i.root)
	if err != nil {
		return fmt.Errorf("read attachment inbox: %w", err)
	}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
			continue
		}
		dir := filepath.Join(i.root, entry.Name())
		if strings.HasPrefix(entry.Name(), stagingPrefix) {
			if _, active := i.activeStaging[dir]; active {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				return fmt.Errorf("inspect temporary attachment staging directory: %w", err)
			}
			if now.Sub(info.ModTime()) > stagingRetention {
				if err := os.Chmod(dir, 0700); err != nil {
					return fmt.Errorf("open stale attachment staging directory for cleanup: %w", err)
				}
				if err := os.RemoveAll(dir); err != nil {
					return fmt.Errorf("remove stale attachment staging directory: %w", err)
				}
			}
			continue
		}
		if i.activeDirs[dir] > 0 {
			continue
		}
		if !isScopedInboxDirectory(entry.Name()) {
			continue
		}
		info, err := os.Lstat(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("inspect scoped attachment inbox: %w", err)
		}
		expired := info.Mode()&os.ModeSymlink != 0 || (info.IsDir() && now.Sub(info.ModTime()) > inboxRetention)
		if info.IsDir() {
			walkErr := filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
					return nil
				}
				fileInfo, err := entry.Info()
				if err != nil {
					return err
				}
				if fileInfo.Mode().IsRegular() && now.Sub(fileInfo.ModTime()) > inboxRetention {
					expired = true
				}
				return nil
			})
			if walkErr != nil {
				return fmt.Errorf("inspect scoped attachment inbox: %w", walkErr)
			}
		}
		if expired {
			if err := i.removeScopedDirectory(entry.Name()); err != nil {
				return fmt.Errorf("remove expired attachment inbox: %w", err)
			}
			continue
		}
	}
	return nil
}

func (i *Inbox) usage() (int64, error) {
	entries, err := os.ReadDir(i.root)
	if err != nil {
		return 0, fmt.Errorf("read attachment inbox: %w", err)
	}
	var total int64
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() || !isScopedInboxDirectory(entry.Name()) {
			continue
		}
		dir := filepath.Join(i.root, entry.Name())
		walkErr := filepath.WalkDir(dir, func(path string, file os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if file.IsDir() || file.Type()&os.ModeSymlink != 0 {
				return nil
			}
			info, err := file.Info()
			if err != nil {
				return err
			}
			if info.Mode().IsRegular() {
				total += info.Size()
			}
			return nil
		})
		if walkErr != nil {
			return 0, fmt.Errorf("inspect scoped attachment inbox: %w", walkErr)
		}
	}
	return total, nil
}

func safeAttachmentName(name string) string {
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(name))
	if name == "" {
		return "未命名文件"
	}
	return core.TruncateRunes(name, 160, "")
}

func safeAttachmentExtension(name string) string {
	ext := filepath.Ext(strings.TrimSpace(name))
	if len(ext) < 2 || len(ext) > 13 {
		return ""
	}
	for _, r := range ext[1:] {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return ""
		}
	}
	return strings.ToLower(ext)
}
