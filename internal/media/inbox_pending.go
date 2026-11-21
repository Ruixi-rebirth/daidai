package media

import (
	"strings"
	"sync"
	"time"

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
	Sources       *Sources
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
