package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"daidai/internal/core"
)

const (
	inboxRetention                = 7 * 24 * time.Hour
	inboxMaxBytes                 = 512 << 20
	pendingTTL                    = 10 * time.Minute
	pendingMaxScopes              = 4096
	pendingMaxAttachmentsPerScope = 12
	pendingMaxDropRecordsPerScope = 64
	stagingPrefix                 = ".staging-"
	stagingRetention              = time.Hour
	inboxCleanupInterval          = time.Hour
)

// Inbox stores user supplied files privately for short lived AGY work.
type Inbox struct {
	root          string
	mu            sync.Mutex
	pendingMu     sync.Mutex
	pending       map[string]pendingAttachments
	activeStaging map[string]struct{}
	activeDirs    map[string]int
	downloadSlots chan struct{}
}

type pendingAttachments struct {
	files   []pendingAttachment
	dropped []pendingDrop
}

type pendingAttachment struct {
	file      *core.Attachment
	expiresAt time.Time
}

type pendingDrop struct {
	kind      core.AttachmentKind
	count     int
	expiresAt time.Time
}

// AttachmentScope isolates pending attachments to one sender in one conversation.
func AttachmentScope(platform string, isGroup bool, channelID, senderID string) string {
	chatType := "private"
	if isGroup {
		chatType = "group"
	}
	return platform + "\x00" + chatType + "\x00" + channelID + "\x00" + senderID
}

// RememberPending retains only attachment metadata and URLs for a short follow-up window.
// It does not download or inspect the files.
func (i *Inbox) RememberPending(scope string, attachments []*core.Attachment) {
	if i == nil || strings.TrimSpace(scope) == "" || len(attachments) == 0 {
		return
	}
	files := make([]pendingAttachment, 0, len(attachments))
	for _, attachment := range attachments {
		if attachment == nil || strings.TrimSpace(attachment.URL) == "" || len(attachment.URL) > core.MaxAttachmentURLBytes {
			continue
		}
		copy := *attachment
		copy.Path = ""
		files = append(files, pendingAttachment{file: &copy})
	}
	if len(files) == 0 {
		return
	}
	i.pendingMu.Lock()
	defer i.pendingMu.Unlock()
	if i.pending == nil {
		i.pending = make(map[string]pendingAttachments)
	}
	now := time.Now()
	for key, pending := range i.pending {
		pending.files = retainUnexpiredPending(pending.files, now)
		pending.dropped = retainUnexpiredDrops(pending.dropped, now)
		if len(pending.files) == 0 && len(pending.dropped) == 0 {
			delete(i.pending, key)
		} else {
			i.pending[key] = pending
		}
	}
	current, exists := i.pending[scope]
	if !exists && len(i.pending) >= pendingMaxScopes {
		var oldestKey string
		var oldestExpiry time.Time
		for key, pending := range i.pending {
			expiresAt := time.Time{}
			if len(pending.files) > 0 {
				expiresAt = pending.files[0].expiresAt
			}
			for _, drop := range pending.dropped {
				if expiresAt.IsZero() || drop.expiresAt.Before(expiresAt) {
					expiresAt = drop.expiresAt
				}
			}
			if oldestKey == "" || expiresAt.Before(oldestExpiry) {
				oldestKey = key
				oldestExpiry = expiresAt
			}
		}
		delete(i.pending, oldestKey)
	}
	type pendingKey struct {
		kind core.AttachmentKind
		url  string
	}
	existingURLs := make(map[pendingKey]int, len(current.files)+len(files))
	for index, pendingFile := range current.files {
		if pendingFile.file != nil {
			existingURLs[pendingKey{kind: pendingFile.file.Kind, url: pendingFile.file.URL}] = index
		}
	}
	for _, pendingFile := range files {
		key := pendingKey{kind: pendingFile.file.Kind, url: pendingFile.file.URL}
		pendingFile.expiresAt = now.Add(pendingTTL)
		if index, exists := existingURLs[key]; exists {
			current.files = append(current.files[:index], current.files[index+1:]...)
			for existingKey, existingIndex := range existingURLs {
				if existingIndex > index {
					existingURLs[existingKey] = existingIndex - 1
				}
			}
			existingURLs[key] = len(current.files)
			current.files = append(current.files, pendingFile)
			continue
		}
		existingURLs[key] = len(current.files)
		current.files = append(current.files, pendingFile)
	}
	if len(current.files) > pendingMaxAttachmentsPerScope {
		dropped := len(current.files) - pendingMaxAttachmentsPerScope
		for _, removed := range current.files[:dropped] {
			if removed.file != nil {
				current.dropped = append(current.dropped, pendingDrop{kind: removed.file.Kind, count: 1, expiresAt: removed.expiresAt})
			}
		}
		current.files = append([]pendingAttachment(nil), current.files[dropped:]...)
	}
	compactPendingDrops(&current.dropped)
	i.pending[scope] = current
}

// Pending returns a copy of unexpired attachment metadata for a follow-up message.
func (i *Inbox) Pending(scope string) ([]*core.Attachment, int) {
	return i.PendingKind(scope, "")
}

// ForgetConversationPending clears all unprocessed attachment metadata for one
// conversation, including every sender in a group conversation.
func (i *Inbox) ForgetConversationPending(platform string, isGroup bool, channelID string) {
	if i == nil || strings.TrimSpace(platform) == "" || strings.TrimSpace(channelID) == "" {
		return
	}
	chatType := "private"
	if isGroup {
		chatType = "group"
	}
	prefix := platform + "\x00" + chatType + "\x00" + channelID + "\x00"
	i.pendingMu.Lock()
	for scope := range i.pending {
		if strings.HasPrefix(scope, prefix) {
			delete(i.pending, scope)
		}
	}
	i.pendingMu.Unlock()
}

// PendingKind returns only recent attachments of the requested kind. An empty
// kind returns every pending attachment and the aggregate omitted count.
func (i *Inbox) PendingKind(scope string, kind core.AttachmentKind) ([]*core.Attachment, int) {
	if i == nil || strings.TrimSpace(scope) == "" {
		return nil, 0
	}
	i.pendingMu.Lock()
	defer i.pendingMu.Unlock()
	now := time.Now()
	pending, ok := i.pending[scope]
	if !ok {
		return nil, 0
	}
	pending.files = retainUnexpiredPending(pending.files, now)
	pending.dropped = retainUnexpiredDrops(pending.dropped, now)
	if len(pending.files) == 0 && len(pending.dropped) == 0 {
		delete(i.pending, scope)
		return nil, 0
	}
	i.pending[scope] = pending
	dropped := 0
	for _, item := range pending.dropped {
		if kind == "" || item.kind == kind {
			count := item.count
			if count < 1 {
				count = 1
			}
			dropped += count
		}
	}
	files := make([]*core.Attachment, 0, len(pending.files))
	for _, pendingFile := range pending.files {
		if pendingFile.file == nil || (kind != "" && pendingFile.file.Kind != kind) {
			continue
		}
		copy := *pendingFile.file
		files = append(files, &copy)
	}
	return files, dropped
}

// PendingReceivedBetween returns unexpired attachments received during a
// bounded interval. It lets a debounced request pair with a QQ attachment that
// arrives as a separate gateway event just after the request text.
func (i *Inbox) PendingReceivedBetween(scope string, kind core.AttachmentKind, start, end time.Time) []*core.Attachment {
	if i == nil || strings.TrimSpace(scope) == "" || start.IsZero() || !end.After(start) {
		return nil
	}
	i.pendingMu.Lock()
	defer i.pendingMu.Unlock()
	pending, ok := i.pending[scope]
	if !ok {
		return nil
	}
	now := time.Now()
	pending.files = retainUnexpiredPending(pending.files, now)
	selected := make([]*core.Attachment, 0, len(pending.files))
	for _, pendingFile := range pending.files {
		if pendingFile.file == nil || (kind != "" && pendingFile.file.Kind != kind) {
			continue
		}
		receivedAt := pendingFile.expiresAt.Add(-pendingTTL)
		if receivedAt.Before(start) || receivedAt.After(end) {
			continue
		}
		copy := *pendingFile.file
		selected = append(selected, &copy)
	}
	pending.dropped = retainUnexpiredDrops(pending.dropped, now)
	if len(pending.files) == 0 && len(pending.dropped) == 0 {
		delete(i.pending, scope)
	} else {
		i.pending[scope] = pending
	}
	return selected
}

func retainUnexpiredPending(files []pendingAttachment, now time.Time) []pendingAttachment {
	remaining := files[:0]
	for _, pendingFile := range files {
		if pendingFile.file != nil && pendingFile.expiresAt.After(now) {
			remaining = append(remaining, pendingFile)
		}
	}
	return remaining
}

func retainUnexpiredDrops(drops []pendingDrop, now time.Time) []pendingDrop {
	remaining := drops[:0]
	for _, drop := range drops {
		if drop.expiresAt.After(now) {
			remaining = append(remaining, drop)
		}
	}
	return remaining
}

func compactPendingDrops(drops *[]pendingDrop) {
	if drops == nil {
		return
	}
	for len(*drops) > pendingMaxDropRecordsPerScope {
		first, second := -1, -1
		for i := 0; i < len(*drops) && first < 0; i++ {
			for j := i + 1; j < len(*drops); j++ {
				if (*drops)[i].kind == (*drops)[j].kind {
					first, second = i, j
					break
				}
			}
		}
		if first < 0 {
			return
		}
		left, right := &(*drops)[first], (*drops)[second]
		left.count += right.count
		if right.expiresAt.After(left.expiresAt) {
			left.expiresAt = right.expiresAt
		}
		copy((*drops)[second:], (*drops)[second+1:])
		*drops = (*drops)[:len(*drops)-1]
	}
}

// ForgetPending drops only the retained metadata for attachments successfully
// processed by this task. New uploads made while the worker runs stay pending.
func (i *Inbox) ForgetPending(scope string, processed []*core.Attachment) {
	if i == nil || strings.TrimSpace(scope) == "" {
		return
	}
	type attachmentKey struct {
		kind            core.AttachmentKind
		url             string
		sourceMessageID string
	}
	processedKeys := make(map[attachmentKey]struct{}, len(processed))
	for _, attachment := range processed {
		if attachment != nil && strings.TrimSpace(attachment.URL) != "" {
			processedKeys[attachmentKey{kind: attachment.Kind, url: attachment.URL, sourceMessageID: attachment.SourceMessageID}] = struct{}{}
		}
	}
	if len(processedKeys) == 0 {
		return
	}
	i.pendingMu.Lock()
	current, ok := i.pending[scope]
	if ok {
		remaining := current.files[:0]
		for _, pendingFile := range current.files {
			if pendingFile.file == nil {
				continue
			}
			key := attachmentKey{kind: pendingFile.file.Kind, url: pendingFile.file.URL, sourceMessageID: pendingFile.file.SourceMessageID}
			if _, wasProcessed := processedKeys[key]; !wasProcessed {
				remaining = append(remaining, pendingFile)
			}
		}
		current.files = remaining
		if len(current.files) == 0 && len(current.dropped) == 0 {
			delete(i.pending, scope)
		} else {
			i.pending[scope] = current
		}
	}
	i.pendingMu.Unlock()
}

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
func (i *Inbox) RunCleanup(ctx context.Context) {
	if i == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		ticker := time.NewTicker(inboxCleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				i.mu.Lock()
				err := i.cleanupExpired(time.Now())
				i.mu.Unlock()
				if err != nil {
					slog.Warn("media inbox: 定期清理附件失败", "err", err)
				}
			}
		}
	}()
}

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
		data, err := fetchQQAttachment(ctx, attachment.URL)
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

func (i *Inbox) Remove(dir string) error {
	if i == nil {
		return nil
	}
	absDir, err := filepath.Abs(filepath.Clean(strings.TrimSpace(dir)))
	if err != nil || filepath.Dir(absDir) != i.root || !isScopedInboxDirectory(filepath.Base(absDir)) {
		return fmt.Errorf("attachment directory is outside the managed inbox")
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.activeDirs[absDir] > 0 {
		return fmt.Errorf("attachment directory is still active")
	}
	return i.removeScopedDirectory(filepath.Base(absDir))
}

// removeScopedDirectory removes one managed attachment directory through an
// opened directory handle, so symlink replacements cannot redirect cleanup.
func (i *Inbox) removeScopedDirectory(base string) error {
	if !isScopedInboxDirectory(base) {
		return fmt.Errorf("attachment directory is not managed by the inbox")
	}
	root, err := os.OpenRoot(i.root)
	if err != nil {
		return fmt.Errorf("open attachment inbox: %w", err)
	}
	defer func() { _ = root.Close() }()

	info, err := root.Lstat(base)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect attachment directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		if err := root.Remove(base); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove replaced attachment directory link: %w", err)
		}
		return nil
	}
	if !info.IsDir() {
		return fmt.Errorf("attachment path is not a directory")
	}

	dirRoot, err := root.OpenRoot(base)
	if err != nil {
		return fmt.Errorf("open scoped attachment directory: %w", err)
	}
	defer func() { _ = dirRoot.Close() }()
	openedInfo, err := dirRoot.Stat(".")
	if err != nil || !os.SameFile(info, openedInfo) {
		return fmt.Errorf("attachment directory changed during cleanup")
	}
	if err := dirRoot.Chmod(".", 0700); err != nil {
		return fmt.Errorf("make attachment directory removable: %w", err)
	}
	if err := removeInboxRootContents(dirRoot); err != nil {
		return fmt.Errorf("remove scoped attachment contents: %w", err)
	}
	if err := root.Remove(base); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove empty attachment directory: %w", err)
	}
	return nil
}

func removeInboxRootContents(root *os.Root) error {
	dirHandle, err := root.Open(".")
	if err != nil {
		return err
	}
	children, readErr := dirHandle.ReadDir(-1)
	closeErr := dirHandle.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	for _, child := range children {
		name := child.Name()
		info, err := root.Lstat(name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			if err := root.Remove(name); err != nil && !os.IsNotExist(err) {
				return err
			}
			continue
		}
		nestedRoot, err := root.OpenRoot(name)
		if err != nil {
			return err
		}
		nestedInfo, statErr := nestedRoot.Stat(".")
		if statErr != nil || !os.SameFile(info, nestedInfo) {
			_ = nestedRoot.Close()
			return fmt.Errorf("directory %q changed during cleanup", name)
		}
		if err := nestedRoot.Chmod(".", 0700); err != nil {
			_ = nestedRoot.Close()
			return err
		}
		if err := removeInboxRootContents(nestedRoot); err != nil {
			_ = nestedRoot.Close()
			return err
		}
		if err := nestedRoot.Close(); err != nil {
			return err
		}
		if err := root.Remove(name); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func isScopedInboxDirectory(name string) bool {
	if len(name) != sha256.Size*2+1 || name[len(name)-1] != '-' {
		return false
	}
	for _, char := range name[:len(name)-1] {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

// Activate protects a stored attachment directory from expiry cleanup while a
// worker may still be reading it. Call Deactivate when that worker finishes.
func (i *Inbox) Activate(dir string) error {
	if i == nil {
		return fmt.Errorf("attachment inbox is unavailable")
	}
	absDir, err := filepath.Abs(filepath.Clean(strings.TrimSpace(dir)))
	if err != nil || filepath.Dir(absDir) != i.root || !isScopedInboxDirectory(filepath.Base(absDir)) {
		return fmt.Errorf("attachment directory is outside the inbox")
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	info, err := os.Lstat(absDir)
	if err != nil {
		return fmt.Errorf("inspect attachment directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || strings.HasPrefix(filepath.Base(absDir), stagingPrefix) {
		return fmt.Errorf("attachment directory is not a stored inbox directory")
	}
	if i.activeDirs == nil {
		i.activeDirs = make(map[string]int)
	}
	i.activeDirs[absDir]++
	return nil
}

func (i *Inbox) Deactivate(dir string) {
	if i == nil {
		return
	}
	absDir, err := filepath.Abs(filepath.Clean(strings.TrimSpace(dir)))
	if err != nil || filepath.Dir(absDir) != i.root || !isScopedInboxDirectory(filepath.Base(absDir)) {
		return
	}
	i.mu.Lock()
	if i.activeDirs[absDir] <= 1 {
		delete(i.activeDirs, absDir)
	} else {
		i.activeDirs[absDir]--
	}
	i.mu.Unlock()
}

func (i *Inbox) removeStored(attachments []*core.Attachment) {
	for _, attachment := range attachments {
		if attachment != nil && attachment.Path != "" {
			_ = os.Remove(attachment.Path)
		}
	}
}

func fetchQQAttachment(ctx context.Context, rawURL string) ([]byte, error) {
	if len(rawURL) > core.MaxAttachmentURLBytes {
		return nil, fmt.Errorf("attachment URL exceeds %d byte limit", core.MaxAttachmentURLBytes)
	}
	if err := core.ValidateTrustedMediaURL(rawURL); err != nil {
		return nil, err
	}
	if err := core.ValidateSafeExternalURL(ctx, rawURL); err != nil {
		return nil, fmt.Errorf("unsafe attachment URL: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
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
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("attachment download status: %d", resp.StatusCode)
	}
	if resp.ContentLength > core.MaxIncomingFileBytes {
		return nil, fmt.Errorf("attachment exceeds %d byte limit", core.MaxIncomingFileBytes)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, core.MaxIncomingFileBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > core.MaxIncomingFileBytes {
		return nil, fmt.Errorf("attachment exceeds %d byte limit", core.MaxIncomingFileBytes)
	}
	return data, nil
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
