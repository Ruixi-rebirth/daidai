package entity

import (
	"fmt"
	"strings"
	"sync"
)

// ImmuneSystem 免疫防御与安全护栏中枢
type ImmuneSystem struct {
	mu            sync.RWMutex
	partnerID     string
	partners      map[string]struct{}
	collaborators map[string]struct{}
	privateUsers  map[string]struct{}
}

// NewImmuneSystem 实例化免疫系统
func NewImmuneSystem(partnerID string, collaborators ...string) *ImmuneSystem {
	im := &ImmuneSystem{
		partnerID:     strings.TrimSpace(partnerID),
		partners:      make(map[string]struct{}),
		collaborators: make(map[string]struct{}),
		privateUsers:  make(map[string]struct{}),
	}
	for _, c := range collaborators {
		c = strings.TrimSpace(c)
		if c != "" {
			im.collaborators[c] = struct{}{}
		}
	}
	return im
}

// SetPartner updates the tool authorization for a platform-verified identity.
func (im *ImmuneSystem) SetPartner(id string, allowed bool) {
	im.mu.Lock()
	defer im.mu.Unlock()
	if im.partners == nil {
		im.partners = make(map[string]struct{})
	}
	if strings.TrimSpace(id) != "" {
		if allowed {
			im.partners[id] = struct{}{}
		} else {
			delete(im.partners, id)
		}
	}
}

func (im *ImmuneSystem) AddPrivateUser(userID string) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return
	}
	im.mu.Lock()
	defer im.mu.Unlock()
	im.privateUsers[userID] = struct{}{}
}

func (im *ImmuneSystem) RemovePrivateUser(userID string) {
	userID = strings.TrimSpace(userID)
	im.mu.Lock()
	defer im.mu.Unlock()
	delete(im.privateUsers, userID)
}

func (im *ImmuneSystem) HasPrivateAccess(userID string) bool {
	userID = strings.TrimSpace(userID)
	im.mu.RLock()
	defer im.mu.RUnlock()
	_, ok := im.privateUsers[userID]
	return ok
}

// AddCollaborator 授权协作者
func (im *ImmuneSystem) AddCollaborator(userID string) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return
	}
	im.mu.Lock()
	defer im.mu.Unlock()
	im.collaborators[userID] = struct{}{}
}

func (im *ImmuneSystem) RemoveCollaborator(userID string) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return
	}
	im.mu.Lock()
	defer im.mu.Unlock()
	delete(im.collaborators, userID)
}

// ResolveRole 评估感知事件发起人的角色权限
func (im *ImmuneSystem) ResolveRole(senderID string) UserRole {
	senderID = strings.TrimSpace(senderID)
	im.mu.RLock()
	defer im.mu.RUnlock()

	if im.partnerID != "" && senderID == im.partnerID {
		return RolePartner
	}
	if _, ok := im.partners[senderID]; ok {
		return RolePartner
	}

	if _, ok := im.collaborators[senderID]; ok {
		return RoleCollaborator
	}

	if _, ok := im.privateUsers[senderID]; ok {
		return RoleFriend
	}

	return RoleGuest
}

// VerifyDelegationPerm 校验是否具备系统控制、运维与重型工程工单 (AGY) 的权限（仅限核心搭档）
func (im *ImmuneSystem) VerifyDelegationPerm(role UserRole) error {
	if role != RolePartner {
		return fmt.Errorf("权限不足：该系统操作仅限核心搭档发起")
	}
	return nil
}

// VerifyFaceGenPerm checks whether a role may dynamically generate and cache new bot face images.
func (im *ImmuneSystem) VerifyFaceGenPerm(role UserRole) error {
	if role != RolePartner && role != RoleCollaborator {
		return fmt.Errorf("权限不足：动态生成形象图片仅限核心搭档与协作者发起")
	}
	return nil
}
