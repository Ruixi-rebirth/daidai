package entity

import "daidai/internal/core"

type toolScope uint8

const (
	scopeAny toolScope = iota
	scopePrivateOnly
	scopeGroupOnly
)

type toolPolicy struct {
	scope       toolScope
	platform    string
	roles       map[core.UserRole]bool
	customCheck func(*SensoryEvent) bool
}

var (
	partnerOnlyRoles = map[core.UserRole]bool{
		RolePartner: true,
	}
	publicToolRoles = map[core.UserRole]bool{
		RolePartner:      true,
		RoleCollaborator: true,
		RoleFriend:       true,
		RoleGuest:        true,
	}
)

var defaultToolPolicies = map[string]toolPolicy{
	// Partner-only private management, credentials, workspaces, engineering tickets, and infrastructure controls.
	"manage_access":    {scope: scopePrivateOnly, platform: "qq", roles: partnerOnlyRoles},
	"singbox_links":    {scope: scopePrivateOnly, roles: partnerOnlyRoles},
	"oci_bill":         {scope: scopePrivateOnly, roles: partnerOnlyRoles},
	"oci_ip":           {scope: scopePrivateOnly, roles: partnerOnlyRoles},
	"switch_workspace": {scope: scopePrivateOnly, roles: partnerOnlyRoles},
	"delegate_ticket":  {scope: scopePrivateOnly, roles: partnerOnlyRoles},

	// Partner-only group configuration tools.
	"manage_group_expressions": {scope: scopeGroupOnly, roles: partnerOnlyRoles},
	"manage_group_image_quota": {scope: scopeGroupOnly, roles: partnerOnlyRoles},

	// Partner-only system, model, media, and briefing controls.
	"manage_geek_briefing": {scope: scopeAny, roles: partnerOnlyRoles},
	"get_system_status":    {scope: scopeAny, roles: partnerOnlyRoles},
	"switch_model":         {scope: scopeAny, roles: partnerOnlyRoles},
	"generate_video":       {scope: scopeAny, roles: partnerOnlyRoles},

	// Shared tools (per-tool role/quota rules remain enforced inside execution).
	"render_card":        {scope: scopeAny, roles: publicToolRoles},
	"manage_memory":      {scope: scopeAny, roles: publicToolRoles},
	"summarize_chat":     {scope: scopeAny, roles: publicToolRoles},
	"search_web":         {scope: scopeAny, roles: publicToolRoles},
	"read_link":          {scope: scopeAny, roles: publicToolRoles},
	"screenshot_web":     {scope: scopeAny, roles: publicToolRoles},
	"search_nix_package": {scope: scopeAny, roles: publicToolRoles},
	"analyze_images":     {scope: scopeAny, roles: publicToolRoles},
	"express_face":       {scope: scopeAny, roles: publicToolRoles},
	"generate_image":     {scope: scopeAny, roles: publicToolRoles},
	"get_geek_briefing": {
		scope: scopeAny,
		roles: publicToolRoles,
		customCheck: func(event *SensoryEvent) bool {
			return event.IsGroup || event.Role == RolePartner
		},
	},
}

// ToolAllowed is the single deny-by-default authorization policy shared by tool
// discovery (toolsForEvent) and execution (HandsRegistry.Execute).
func ToolAllowed(name string, event *SensoryEvent) bool {
	if event == nil {
		return false
	}
	policy, ok := defaultToolPolicies[name]
	if !ok {
		return false
	}
	role := event.Role
	if role == "" {
		role = RoleGuest
	}
	if !policy.roles[role] {
		return false
	}
	if policy.platform != "" && event.Platform != policy.platform {
		return false
	}
	switch policy.scope {
	case scopePrivateOnly:
		if event.IsGroup {
			return false
		}
	case scopeGroupOnly:
		if !event.IsGroup || event.ChannelID == "" {
			return false
		}
	}
	if policy.customCheck != nil && !policy.customCheck(event) {
		return false
	}
	return true
}
