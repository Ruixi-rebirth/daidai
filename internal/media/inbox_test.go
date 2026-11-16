package media

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"daidai/internal/core"
)

func TestStoredAttachmentDirectoryCanBeActivatedAndRemoved(t *testing.T) {
	inbox, err := NewInbox(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sources := &Sources{}
	sources.Register("test-file", func(context.Context, string, int64) ([]byte, error) { return []byte("file contents"), nil })
	inbox.Sources = sources
	stored, dir, err := inbox.Store(context.Background(), "test-scope", []*core.Attachment{{Kind: core.AttachmentFile, URL: "test-file:one", Name: "report.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || filepath.Dir(stored[0].Path) != dir {
		t.Fatal("missing staged file")
	}
	if err := inbox.Activate(dir); err != nil {
		t.Fatalf("stored directory cannot be used by a worker: %v", err)
	}
	expired := time.Now().Add(-inboxRetention - time.Hour)
	if err := os.Chtimes(dir, expired, expired); err != nil {
		t.Fatal(err)
	}
	if err := inbox.cleanupExpired(time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stored[0].Path); err != nil {
		t.Fatal("active attachment removed by cleanup")
	}
	if err := inbox.Remove(dir); err == nil {
		t.Fatal("active attachment directory was removed")
	}
	inbox.Deactivate(dir)
	if err := inbox.Remove(dir); err != nil {
		t.Fatalf("stored directory cannot be removed: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("attachment directory survived removal")
	}
}

func TestStoredAttachmentsCountTowardsQuotaAndExpire(t *testing.T) {
	inbox, err := NewInbox(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sources := &Sources{}
	sources.Register("test-file", func(context.Context, string, int64) ([]byte, error) { return []byte("1234567890"), nil })
	inbox.Sources = sources
	_, dir, err := inbox.Store(context.Background(), "scope", []*core.Attachment{{Kind: core.AttachmentFile, URL: "test-file:one", Name: "report.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	used, err := inbox.usage()
	if err != nil || used != 10 {
		t.Fatalf("stored attachment not counted in quota: %d %v", used, err)
	}
	old := time.Now().Add(-inboxRetention - time.Hour)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
	if err := inbox.cleanupExpired(time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("expired stored attachment directory not removed")
	}
	for _, bad := range []string{"unmanaged", strings.Repeat("a", 64) + "-", strings.Repeat("a", 64) + "-../outside", strings.Repeat("a", 64) + "-/outside"} {
		if isScopedInboxDirectory(bad) {
			t.Fatalf("unmanaged directory accepted: %s", bad)
		}
	}
}
