package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"daidai/internal/core"
)

func NewInbox(root string) (*Inbox, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, fmt.Errorf("attachment inbox directory cannot be empty")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve attachment inbox: %w", err)
	}
	if err := os.MkdirAll(absRoot, 0700); err != nil {
		return nil, fmt.Errorf("create attachment inbox: %w", err)
	}
	info, err := os.Lstat(absRoot)
	if err != nil {
		return nil, fmt.Errorf("inspect attachment inbox: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("attachment inbox must be a real directory")
	}
	if err := os.Chmod(absRoot, 0700); err != nil {
		return nil, fmt.Errorf("secure attachment inbox: %w", err)
	}
	inbox := &Inbox{root: absRoot, downloadSlots: make(chan struct{}, 2)}
	if err := inbox.cleanupExpired(time.Now()); err != nil {
		return nil, fmt.Errorf("clean expired attachment inbox entries: %w", err)
	}
	return inbox, nil
}

// RunCleanup periodically removes expired stored and abandoned staging files.
// Store downloads trusted QQ attachment URLs and returns private local paths.
func (i *Inbox) Store(ctx context.Context, scope string, attachments []*core.Attachment) ([]*core.Attachment, string, error) {
	if i == nil || len(attachments) == 0 {
		return nil, "", nil
	}
	if strings.TrimSpace(scope) == "" {
		return nil, "", fmt.Errorf("attachment scope cannot be empty")
	}
	for _, attachment := range attachments {
		if attachment != nil && len(attachment.URL) > core.MaxAttachmentURLBytes {
			return nil, "", fmt.Errorf("attachment URL exceeds %d byte limit", core.MaxAttachmentURLBytes)
		}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case i.downloadSlots <- struct{}{}:
		defer func() { <-i.downloadSlots }()
	case <-ctx.Done():
		return nil, "", ctx.Err()
	}
	type downloadedAttachment struct {
		metadata *core.Attachment
		path     string
		size     int64
	}
	downloaded := make([]downloadedAttachment, 0, len(attachments))
	stagingDir, err := os.MkdirTemp(i.root, stagingPrefix)
	if err != nil {
		return nil, "", fmt.Errorf("create temporary attachment staging directory: %w", err)
	}
	if err := os.Chmod(stagingDir, 0700); err != nil {
		_ = os.RemoveAll(stagingDir)
		return nil, "", fmt.Errorf("secure temporary attachment staging directory: %w", err)
	}
	i.mu.Lock()
	if i.activeStaging == nil {
		i.activeStaging = make(map[string]struct{})
	}
	i.activeStaging[stagingDir] = struct{}{}
	i.mu.Unlock()
	defer func() {
		i.mu.Lock()
		defer i.mu.Unlock()
		_ = os.Chmod(stagingDir, 0700)
		_ = os.RemoveAll(stagingDir)
		delete(i.activeStaging, stagingDir)
	}()
	var stagedBytes int64
	for _, attachment := range attachments {
		if attachment == nil || strings.TrimSpace(attachment.URL) == "" {
			continue
		}
		data, handled, err := i.Sources.Fetch(ctx, attachment.URL, core.MaxIncomingFileBytes)
		if !handled {
			var truncated bool
			data, _, truncated, err = fetchAttachmentStream(ctx, attachment.URL, core.MaxIncomingFileBytes)
			if err == nil && truncated {
				err = fmt.Errorf("attachment exceeds %d byte limit", core.MaxIncomingFileBytes)
			}
		}
		if err != nil {
			return nil, "", fmt.Errorf("download attachment %q: %w", safeAttachmentName(attachment.Name), err)
		}
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		if stagedBytes+int64(len(data)) > inboxMaxBytes {
			return nil, "", fmt.Errorf("attachment request exceeds inbox quota")
		}
		stagedFile, err := os.CreateTemp(stagingDir, "attachment-*")
		if err != nil {
			return nil, "", fmt.Errorf("create temporary attachment file: %w", err)
		}
		stagedPath := stagedFile.Name()
		if err := stagedFile.Chmod(0600); err != nil {
			_ = stagedFile.Close()
			_ = os.Remove(stagedPath)
			return nil, "", fmt.Errorf("secure temporary attachment file: %w", err)
		}
		if _, err := stagedFile.Write(data); err != nil {
			_ = stagedFile.Close()
			_ = os.Remove(stagedPath)
			return nil, "", fmt.Errorf("stage temporary attachment: %w", err)
		}
		if err := stagedFile.Close(); err != nil {
			_ = os.Remove(stagedPath)
			return nil, "", fmt.Errorf("close temporary attachment: %w", err)
		}
		copy := *attachment
		copy.Path = ""
		downloaded = append(downloaded, downloadedAttachment{metadata: &copy, path: stagedPath, size: int64(len(data))})
		stagedBytes += int64(len(data))
	}
	if len(downloaded) == 0 {
		return nil, "", nil
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if err := i.cleanupExpired(time.Now()); err != nil {
		return nil, "", err
	}
	used, err := i.usage()
	if err != nil {
		return nil, "", err
	}
	scopeHash := sha256.Sum256([]byte(scope))
	scopePrefix := hex.EncodeToString(scopeHash[:]) + "-"
	var scopeDir string
	stored := make([]*core.Attachment, 0, len(downloaded))
	cleanup := func() {
		i.removeStored(stored)
		if scopeDir != "" {
			_ = os.Chmod(scopeDir, 0700)
			_ = os.Remove(scopeDir)
		}
	}
	if used+stagedBytes > inboxMaxBytes {
		return nil, "", fmt.Errorf("attachment inbox is full")
	}
	for _, file := range downloaded {
		if err := ctx.Err(); err != nil {
			cleanup()
			return nil, "", err
		}
		if scopeDir == "" {
			scopeDir, err = os.MkdirTemp(i.root, scopePrefix)
			if err != nil {
				cleanup()
				return nil, "", fmt.Errorf("create scoped attachment inbox: %w", err)
			}
			if err := os.Chmod(scopeDir, 0700); err != nil {
				cleanup()
				return nil, "", fmt.Errorf("secure scoped attachment inbox: %w", err)
			}
		}
		path, err := i.writeFromPath(scopeDir, file.path, file.metadata.Name)
		if err != nil {
			cleanup()
			return nil, "", err
		}
		copy := *file.metadata
		copy.Path = path
		stored = append(stored, &copy)
		used += file.size
	}
	if err := ctx.Err(); err != nil {
		cleanup()
		return nil, "", err
	}
	if err := i.seal(scopeDir); err != nil {
		cleanup()
		return nil, "", err
	}
	if err := ctx.Err(); err != nil {
		cleanup()
		return nil, "", err
	}
	return stored, scopeDir, nil
}

// AttachmentContent 包含流式读取的附件内容与元数据
