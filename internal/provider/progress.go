package provider

import (
	"encoding/json"
	"strings"
)

func normalizedJSON(raw string) string {
	var value any
	if json.Unmarshal([]byte(raw), &value) != nil {
		return strings.TrimSpace(raw)
	}
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
func validateProgress(messages []message, guarded []string) error {
	if len(guarded) == 0 || len(messages) < 6 {
		return nil
	}
	tail := messages[len(messages)-6:]
	var fingerprint string
	seen := make(map[string]bool)
	for i := 0; i < 6; i += 2 {
		assistant, result := tail[i], tail[i+1]
		if assistant.Role != "assistant" || result.Role != "tool" || len(assistant.ToolCalls) != 1 {
			return nil
		}
		call := assistant.ToolCalls[0]
		enabled := false
		for _, name := range guarded {
			enabled = enabled || name == call.Function.Name
		}
		if !enabled || call.ID == "" || call.ID != result.ToolCallID || seen[call.ID] {
			return nil
		}
		seen[call.ID] = true
		decoded, err := parseContent(result.Content)
		if err != nil {
			return nil
		}
		attachmentState, _ := json.Marshal(decoded.Attachments)
		current := string(attachmentState) + "\x00" + call.Function.Name + "\x00" + normalizedJSON(call.Function.Arguments) + "\x00" + normalizedJSON(decoded.Text)
		if i == 0 {
			fingerprint = current
		} else if fingerprint != current {
			return nil
		}
	}
	return reject(400, "configured tool repeated three exchanges without progress")
}
