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
	collaborators map[string]struct{}
}

// NewImmuneSystem 实例化免疫系统
func NewImmuneSystem(partnerID string, collaborators ...string) *ImmuneSystem {
	im := &ImmuneSystem{
		partnerID:     strings.TrimSpace(partnerID),
		collaborators: make(map[string]struct{}),
	}
	for _, c := range collaborators {
		c = strings.TrimSpace(c)
		if c != "" {
			im.collaborators[c] = struct{}{}
		}
	}
	return im
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

// ResolveRole 评估感知事件发起人的角色权限
func (im *ImmuneSystem) ResolveRole(senderID string) UserRole {
	senderID = strings.TrimSpace(senderID)
	im.mu.RLock()
	defer im.mu.RUnlock()

	if im.partnerID != "" && senderID == im.partnerID {
		return RolePartner
	}

	if _, ok := im.collaborators[senderID]; ok {
		return RoleCollaborator
	}

	return RoleGuest
}

// VerifyDelegationPerm 校验是否具备派发重型工程工单 (AGY) 的权限
func (im *ImmuneSystem) VerifyDelegationPerm(role UserRole) error {
	if role != RolePartner && role != RoleCollaborator {
		return fmt.Errorf("权限不足：该系统工程操作仅限核心搭档与协作者发起")
	}
	return nil
}
