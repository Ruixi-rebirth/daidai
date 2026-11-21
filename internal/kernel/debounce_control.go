package kernel

import (
	"strings"
)

func isImmediateControlEvent(content string) bool {
	command := strings.TrimSpace(content)
	if isStopCommand(command) {
		return true
	}
	switch command {
	case "继续", "/continue",
		"/tasks", "/status", "/sysinfo", "/usage", "/model", "/models",
		"/agy login", "/agy auth", "/agy login force", "/agy login --force",
		"/agy status", "/agy logout", "/stop",
		"/reset", "/new", "/clear", "/ws clear", "/ws reset",
		"/pwd", "/workspaces", "/ws", "/ws exit", "/workspaces exit",
		"/ws clean", "/clean", "/help", "/task",
		"/voice":
		return true
	}
	if strings.HasPrefix(command, "/task ") || strings.HasPrefix(command, "/cd ") || command == "/cd" ||
		strings.HasPrefix(command, "/ws rm ") || strings.HasPrefix(command, "/workspaces rm ") ||
		strings.HasPrefix(command, "/voice ") {
		return true
	}
	// Authorization-code parsing is case-insensitive in SuperAgent.
	lowerCommand := strings.ToLower(command)
	return strings.HasPrefix(lowerCommand, "/agy code ") || strings.HasPrefix(lowerCommand, "/agy auth ")
}

func isStopCommand(command string) bool {
	return command == "/stop" || strings.HasPrefix(command, "/stop ")
}

func controlCommandName(command string) string {
	if command == "/stop" {
		return command
	}
	if end := strings.IndexByte(command, ' '); end >= 0 {
		return command[:end]
	}
	return command
}

func invalidatesPendingTurns(content string) bool {
	switch strings.TrimSpace(content) {
	case "/reset", "/new", "/clear", "/ws clear", "/ws reset":
		return true
	default:
		return false
	}
}

func invalidatesPendingSenderTurns(content string) bool {
	return strings.TrimSpace(content) == "/stop"
}

func requiresControlBarrier(content string) bool {
	switch strings.TrimSpace(content) {
	case "/reset", "/new", "/clear", "/ws clear", "/ws reset":
		return true
	default:
		return false
	}
}
