package chat

import (
	"fmt"
	"maps"
	"strings"

	"daidai/internal/core"
	"daidai/internal/entity"
)

type eventDefinitionsHands interface {
	DefinitionsForEvent(event *entity.SensoryEvent) []core.ToolDefinition
}

func toolsForEvent(hands entity.Hands, event *entity.SensoryEvent, imageSources []string) []core.ToolDefinition {
	if hands == nil {
		return nil
	}
	var definitions []core.ToolDefinition
	if scoped, ok := hands.(eventDefinitionsHands); ok {
		definitions = scoped.DefinitionsForEvent(event)
	} else {
		definitions = hands.Definitions()
	}
	var available []core.ToolDefinition
	for _, definition := range definitions {
		name := definition.Function.Name
		if !entity.ToolAllowed(name, event) {
			continue
		}
		if name == "analyze_images" {
			if len(imageSources) == 0 {
				continue
			}
			definition.Function.Parameters = imageSourceParameters(definition.Function.Parameters, imageSources)
		}
		available = append(available, definition)
	}
	return available
}

// Image parameters belong to the current request. Leave the registered schema intact.
func imageSourceParameters(schema map[string]any, sources []string) map[string]any {
	properties, _ := schema["properties"].(map[string]any)
	source, _ := properties["source"].(map[string]any)
	if source == nil {
		return schema
	}
	result := maps.Clone(schema)
	fields := maps.Clone(properties)
	source = maps.Clone(source)
	source["enum"] = sources
	fields["source"] = source
	result["properties"] = fields
	return result
}

func toolAvailable(tools []core.ToolDefinition, name string) bool {
	for _, tool := range tools {
		if tool.Function.Name == name {
			return true
		}
	}
	return false
}

func validateToolCalls(calls []core.ToolCall) error {
	seenIDs := make(map[string]struct{}, len(calls))
	for i, call := range calls {
		if strings.TrimSpace(call.ID) == "" {
			return fmt.Errorf("tool call %d has an empty ID", i)
		}
		if _, exists := seenIDs[call.ID]; exists {
			return fmt.Errorf("tool call ID %q is duplicated", call.ID)
		}
		seenIDs[call.ID] = struct{}{}
		if strings.TrimSpace(call.Function.Name) == "" {
			return fmt.Errorf("tool call %q has an empty function name", call.ID)
		}
		if call.Type != "" && call.Type != "function" {
			return fmt.Errorf("tool call %q has unsupported type %q", call.ID, call.Type)
		}
	}
	return nil
}
