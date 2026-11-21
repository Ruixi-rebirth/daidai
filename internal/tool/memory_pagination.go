package tool

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"daidai/internal/entity"
)

const maxProfilePageStates = 4096

func memorySearchPageKey(event *entity.SensoryEvent, query string) string {
	queryHash := sha256.Sum256([]byte(query))
	return profilePageKey(event) + ":search:" + hex.EncodeToString(queryHash[:])
}

func (t *MemoryTool) resolveProfilePage(key string, requestedPage int, direction string) int {
	if requestedPage > 0 {
		return requestedPage
	}
	t.pageMu.Lock()
	defer t.pageMu.Unlock()
	currentPage := t.profilePage[key]
	if currentPage < 1 {
		currentPage = 1
	}
	switch strings.ToUpper(strings.TrimSpace(direction)) {
	case "NEXT":
		return currentPage + 1
	case "PREVIOUS":
		if currentPage > 1 {
			return currentPage - 1
		}
	case "FIRST":
		return 1
	}
	return 1
}

func (t *MemoryTool) setProfilePage(key string, page int) {
	t.pageMu.Lock()
	t.setProfilePageLocked(key, page)
	t.pageMu.Unlock()
}

func (t *MemoryTool) setProfilePageLocked(key string, page int) {
	if t.profilePage == nil {
		t.profilePage = make(map[string]int)
	}
	if _, exists := t.profilePage[key]; !exists && len(t.profilePage) >= maxProfilePageStates {
		for existingKey := range t.profilePage {
			delete(t.profilePage, existingKey)
			break
		}
	}
	t.profilePage[key] = page
}

// reconcileProfilePage keeps explicit page requests strict while recovering
// cached pagination after deletions shrink the available result set.
func (t *MemoryTool) reconcileProfilePage(key string, page, totalPages, requestedPage int, direction string) (int, bool) {
	if page <= totalPages {
		if requestedPage == 0 {
			t.setProfilePage(key, page)
		}
		return page, false
	}
	if requestedPage > 0 {
		return page, false
	}
	t.pageMu.Lock()
	defer t.pageMu.Unlock()
	currentPage := t.profilePage[key]
	if currentPage < 1 {
		currentPage = 1
	}
	if strings.EqualFold(strings.TrimSpace(direction), "next") && currentPage == totalPages {
		return page, true
	}
	page = totalPages
	if page < 1 {
		page = 1
	}
	t.setProfilePageLocked(key, page)
	return page, false
}
