package entity

import "strings"

// CommandMenuEntry is shared by native platform menus and the help registry.
type CommandMenuEntry struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

func CommandMenu(platform string) []CommandMenuEntry {
	var menu []CommandMenuEntry
	seen := make(map[string]bool)
	for _, command := range slashCommandRegistry(&SuperAgent{}) {
		if command.platform != "" && command.platform != platform {
			continue
		}
		if platform == "telegram" && strings.HasPrefix(command.usage, "/voice") {
			continue
		}
		for _, alias := range strings.Split(command.usage, " · ") {
			fields := strings.Fields(alias)
			if len(fields) == 0 {
				continue
			}
			name := strings.TrimPrefix(fields[0], "/")
			if seen[name] {
				continue
			}
			seen[name] = true
			description := command.help
			if command.access != "" {
				description += "（" + command.access + "）"
			}
			menu = append(menu, CommandMenuEntry{Command: name, Description: description})
		}
	}
	return menu
}
