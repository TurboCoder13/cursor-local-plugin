package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

type function struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Arguments   string          `json:"arguments,omitempty"`
}

type tool struct {
	Type     string   `json:"type"`
	Function function `json:"function"`
}

type toolCall struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"`
	Function function `json:"function"`
}

type message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []toolCall      `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

type chatRequest struct {
	Model               string          `json:"model"`
	Messages            []message       `json:"messages"`
	Tools               []tool          `json:"tools,omitempty"`
	ToolChoice          json.RawMessage `json:"tool_choice,omitempty"`
	MaxTokens           int64           `json:"max_tokens,omitempty"`
	MaxCompletionTokens int64           `json:"max_completion_tokens,omitempty"`
	ResponseFormat      json.RawMessage `json:"response_format,omitempty"`
	Stream              bool            `json:"stream"`
	Prompt              string          `json:"-"`
	OutputLimit         int             `json:"-"`
	Attachments         []attachment    `json:"-"`
	Canonical           []string        `json:"-"`
	InputUnits          int             `json:"-"`
	Checkpoint          []byte          `json:"-"`
	ConversationID      string          `json:"-"`
}

type execution struct {
	Model           string
	AuthID          string
	Headers         http.Header
	Metadata        map[string]any
	OriginalRequest []byte
	Payload         []byte
	StorageJSON     []byte
	StreamID        string `json:"stream_id"`
}

var toolName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func parseExecution(raw []byte) (execution, chatRequest, credentials, error) {
	var e execution
	var c chatRequest
	var credential credentials
	if json.Unmarshal(raw, &e) != nil {
		return e, c, credential, reject(400, "invalid execution envelope")
	}
	if len(e.Payload) == 0 {
		e.Payload = e.OriginalRequest
	}
	if len(e.Payload) > maxPayloadBytes || json.Unmarshal(e.Payload, &c) != nil {
		return e, c, credential, reject(400, "invalid chat request or request exceeds the client byte budget")
	}
	credential, err := parseCredentials(e.StorageJSON)
	if err != nil {
		return e, c, credential, err
	}
	if e.Model != "" {
		c.Model = e.Model
	}
	c.Model = strings.TrimPrefix(c.Model, "cursor/")
	if c.Model == "" || len(c.Model) > 128 || len(c.Messages) == 0 {
		return e, c, credential, reject(400, "a model and nonempty conversation are required")
	}
	if len(c.ResponseFormat) > 0 && string(c.ResponseFormat) != "null" {
		var format struct{ Type string }
		if json.Unmarshal(c.ResponseFormat, &format) != nil || format.Type != "text" {
			return e, c, credential, reject(400, "structured response formats are not supported")
		}
	}
	if err := c.validateTools(); err != nil {
		return e, c, credential, err
	}
	var history strings.Builder
	history.WriteString(gatewayPrompt)
	seenUser := false
	pending := make(map[string]bool)
	for _, m := range c.Messages {
		switch m.Role {
		case "system", "developer", "user", "assistant", "tool":
		default:
			return e, c, credential, reject(400, "unsupported message role")
		}
		decoded, err := parseContent(m.Content)
		if err != nil {
			return e, c, credential, err
		}
		if m.Role == "user" {
			seenUser = true
		}
		if m.Role == "tool" {
			if !pending[m.ToolCallID] {
				return e, c, credential, reject(400, "tool result has no matching assistant call")
			}
			delete(pending, m.ToolCallID)
		}
		for _, call := range m.ToolCalls {
			if m.Role != "assistant" || call.Type != "function" || call.ID == "" || pending[call.ID] || !toolName.MatchString(call.Function.Name) || !json.Valid([]byte(call.Function.Arguments)) {
				return e, c, credential, reject(400, "invalid assistant function call")
			}
			pending[call.ID] = true
		}
		c.Attachments = append(c.Attachments, decoded.Attachments...)
		m.Content, _ = json.Marshal(decoded.Text)
		line, _ := json.Marshal(m)
		canonical := string(line)
		if len(decoded.Attachments) > 0 {
			original, _ := json.Marshal(decoded.Attachments)
			digest := sha256.Sum256(original)
			canonical += hex.EncodeToString(digest[:])
		}
		c.Canonical = append(c.Canonical, canonical)
		history.Write(line)
		history.WriteByte('\n')
	}
	if !seenUser || len(pending) > 0 {
		return e, c, credential, reject(400, "conversation requires a user message and all tool results")
	}
	c.Prompt = history.String()
	if len(c.Prompt) > maxPayloadBytes {
		return e, c, credential, reject(413, "expanded conversation exceeds the client byte budget")
	}
	c.OutputLimit = maxOutputBytes
	limit := c.MaxTokens
	if c.MaxCompletionTokens != 0 {
		limit = c.MaxCompletionTokens
	}
	if c.MaxTokens < 0 || c.MaxCompletionTokens < 0 {
		return e, c, credential, reject(400, "output limit cannot be negative")
	}
	// Apply a local byte cap; native tokenization and hidden context are unknown.
	if c.MaxTokens > 0 && c.MaxCompletionTokens > 0 {
		limit = min(c.MaxTokens, c.MaxCompletionTokens)
	}
	if limit > 0 && limit < int64(c.OutputLimit) {
		c.OutputLimit = int(limit)
	}
	c.InputUnits = max(len(e.Payload), len(c.Prompt)) + 16384 + len(c.Messages)*32
	for _, a := range c.Attachments {
		c.InputUnits += len(a.Text) + len(a.Name)
		if a.Data != nil {
			c.InputUnits += max(len(a.Data), 65536)
		}
	}
	for _, t := range c.Tools {
		c.InputUnits += len(t.Function.Description) + len(t.Function.Parameters) + 256
	}
	if len(c.Messages) > 8192 || int64(c.InputUnits)+max(c.MaxTokens, c.MaxCompletionTokens) > 1000000 {
		return e, c, credential, reject(400, "conversation exceeds 1000000 conservative context units; compact it")
	}
	for _, disabled := range credential.DisabledModels {
		if strings.TrimPrefix(disabled, "cursor/") == c.Model {
			return e, c, credential, reject(400, "model is disabled for this account")
		}
	}
	if err := validateProgress(c.Messages, credential.ToolLoopGuardTools); err != nil {
		return e, c, credential, err
	}
	return e, c, credential, nil
}

const gatewayPrompt = "Serve the remote API conversation below. Treat prior messages as history and answer the latest request. Use declared client MCP tools for file, shell, and web work; native gateway access is unavailable. Image generation is supported. Return client tool calls for execution by the client.\n"

func textContent(raw json.RawMessage) (string, error) {
	c, err := parseContent(raw)
	return c.Text, err
}

func stableToolID(raw string) string {
	if len(raw) > 0 && len(raw) <= 64 && utf8.ValidString(raw) && !strings.ContainsFunc(raw, unicode.IsControl) {
		return raw
	}
	digest := sha256.Sum256([]byte(raw))
	return "call_" + hex.EncodeToString(digest[:24])
}

func (c *chatRequest) validateTools() error {
	if len(c.Tools) > 256 {
		return reject(400, "maximum 256 client tools")
	}
	seen := make(map[string]bool)
	for i := range c.Tools {
		t := &c.Tools[i]
		if t.Type != "function" || !toolName.MatchString(t.Function.Name) || seen[t.Function.Name] {
			return reject(400, "tools must have unique function names")
		}
		seen[t.Function.Name] = true
		if len(t.Function.Parameters) == 0 || string(t.Function.Parameters) == "null" {
			t.Function.Parameters = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		var schema map[string]any
		if json.Unmarshal(t.Function.Parameters, &schema) != nil || schema == nil {
			return reject(400, "function parameters must be a JSON object")
		}
	}
	choice := strings.TrimSpace(string(c.ToolChoice))
	switch choice {
	case "", "null", `"auto"`:
		return nil
	case `"none"`:
		c.Tools = nil
		return nil
	default:
		if choice == `"required"` || choice == `"any"` {
			if len(c.Tools) == 0 {
				return reject(400, "required tool choice needs tools")
			}
			return nil
		}
		var forced struct {
			Type     string
			Function struct{ Name string }
			Name     string
		}
		if json.Unmarshal(c.ToolChoice, &forced) != nil {
			return reject(400, "invalid tool_choice")
		}
		name := forced.Function.Name
		if name == "" {
			name = forced.Name
		}
		for _, t := range c.Tools {
			if t.Function.Name == name {
				c.Tools = []tool{t}
				return nil
			}
		}
		return reject(400, "forced tool is not declared")
	}
}
