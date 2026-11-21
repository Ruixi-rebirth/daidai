package tool

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

type resolvedTarget struct {
	UserID      string
	DisplayName string
	Role        string
	Index       int
	IsPending   bool
}

func (target *resolvedTarget) displayDesc() string {
	if target == nil {
		return ""
	}
	prefix := ""
	if target.Index > 0 {
		if target.IsPending {
			prefix = fmt.Sprintf("申请者 #%d（", target.Index)
		} else {
			prefix = fmt.Sprintf("用户 #%d（", target.Index)
		}
	}
	name := strings.TrimSpace(target.DisplayName)
	if name != "" && name != target.UserID {
		if prefix != "" {
			return fmt.Sprintf("%s%s · `%s`）", prefix, name, target.UserID)
		}
		return fmt.Sprintf("%s（`%s`）", name, target.UserID)
	}
	if prefix != "" {
		return fmt.Sprintf("%s`%s`）", prefix, target.UserID)
	}
	return fmt.Sprintf("`%s`", target.UserID)
}

func parseApplicantIndex(s string) int {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "#")
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "第")
	s = strings.TrimSuffix(s, "个申请者")
	s = strings.TrimSuffix(s, "位申请者")
	s = strings.TrimSuffix(s, "名申请者")
	s = strings.TrimSuffix(s, "个用户")
	s = strings.TrimSuffix(s, "位用户")
	s = strings.TrimSuffix(s, "名用户")
	s = strings.TrimSuffix(s, "个申请")
	s = strings.TrimSuffix(s, "位申请")
	s = strings.TrimSuffix(s, "个")
	s = strings.TrimSuffix(s, "位")
	s = strings.TrimSuffix(s, "名")
	s = strings.TrimSuffix(s, "号")
	s = strings.TrimSpace(s)
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return n
	}
	chineseNumerals := map[string]int{
		"一": 1, "二": 2, "两": 2, "三": 3, "四": 4, "五": 5,
		"六": 6, "七": 7, "八": 8, "九": 9, "十": 10,
	}
	if n, ok := chineseNumerals[s]; ok {
		return n
	}
	return 0
}

func (t *AccessTool) resolveTarget(ctx context.Context, input string, preferAuthorized bool) (*resolvedTarget, error) {
	if t.db == nil {
		return nil, fmt.Errorf("权限管理存储未配置")
	}
	requests, err := t.db.ListPendingPrivateAccessRequests(ctx, "qq", 50)
	if err != nil {
		return nil, fmt.Errorf("读取权限申请失败，未选择用户: %w", err)
	}
	roles, err := t.db.ListUserRoles(ctx, "qq")
	if err != nil {
		return nil, fmt.Errorf("读取已授权用户失败，未选择用户: %w", err)
	}
	pending := make([]resolvedTarget, 0, len(requests))
	authorized := make([]resolvedTarget, 0, len(roles))
	for i, r := range roles {
		authorized = append(authorized, resolvedTarget{UserID: r.UserID, DisplayName: r.DisplayName, Role: r.Role, Index: i + 1})
	}
	for i, r := range requests {
		pending = append(pending, resolvedTarget{UserID: r.UserID, DisplayName: r.DisplayName, Index: i + 1, IsPending: true})
	}
	return resolveAccessTarget(input, preferAuthorized, pending, authorized)
}

// Identity wins over display text; names must identify exactly one person.
// Indexes never fall through to another list when they are out of range.
func resolveAccessTarget(input string, preferAuthorized bool, pending, authorized []resolvedTarget) (*resolvedTarget, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil, fmt.Errorf("请提供目标用户标识（序号、昵称或 OpenID）")
	}
	all := append(append([]resolvedTarget(nil), authorized...), pending...)
	for _, target := range all {
		if target.UserID == input {
			return &target, nil
		}
	}

	list, label := pending, "待处理申请"
	if preferAuthorized {
		list, label = authorized, "已授权用户"
	}
	if strings.Contains(input, "申请") {
		list, label = pending, "待处理申请"
	} else if strings.Contains(input, "用户") {
		list, label = authorized, "已授权用户"
	}
	lower := strings.ToLower(input)
	pronoun := lower == "他" || lower == "她" || lower == "ta" || input == "申请者" || input == "该用户" || input == "该申请"
	if pronoun {
		if len(list) != 1 {
			return nil, fmt.Errorf("当前有 %d 位%s，请指定完整 OpenID 或明确的名单序号", len(list), label)
		}
		target := list[0]
		return &target, nil
	}
	if index := parseApplicantIndex(input); index > 0 {
		if index > len(list) {
			return nil, fmt.Errorf("未找到第 %d 位%s（当前共有 %d 位）", index, label, len(list))
		}
		target := list[index-1]
		return &target, nil
	}

	name := strings.TrimSpace(strings.TrimPrefix(input, "@"))
	var matched *resolvedTarget
	for _, target := range all {
		if strings.TrimSpace(target.DisplayName) == "" || !strings.EqualFold(strings.TrimSpace(target.DisplayName), name) {
			continue
		}
		if matched != nil && matched.UserID != target.UserID {
			return nil, fmt.Errorf("昵称 %q 对应多位用户，请提供完整 OpenID 或明确的名单序号", name)
		}
		if matched == nil {
			copy := target
			matched = &copy
		}
	}
	if matched != nil {
		return matched, nil
	}
	if validAccessUserID(input) {
		return &resolvedTarget{UserID: input}, nil
	}
	return nil, fmt.Errorf("未能匹配到用户，请核对序号、昵称或完整 OpenID")
}

func validAccessUserID(userID string) bool {
	if userID == "" || len(userID) > 256 || len(strings.Fields(userID)) != 1 {
		return false
	}
	for _, r := range userID {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
