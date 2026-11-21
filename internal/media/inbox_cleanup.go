package media

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

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
	hash, suffix, ok := strings.Cut(name, "-")
	if !ok || len(hash) != sha256.Size*2 || suffix == "" {
		return false
	}
	for _, char := range hash {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	for _, char := range suffix {
		valid := (char >= '0' && char <= '9') || (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z')
		if !valid {
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
