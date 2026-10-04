package provider

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

type usageRecord struct {
	Executions       uint64 `json:"executions"`
	PromptTokens     uint64 `json:"estimated_prompt_tokens"`
	CompletionTokens uint64 `json:"estimated_completion_tokens"`
	Success          uint64 `json:"logical_success"`
	Failed           uint64 `json:"logical_failed"`
}
type selection struct {
	Auth string
	At   time.Time
}

func (s *Service) recordExecution(id string, input, output int) {
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.usage) >= 4096 && s.usage[id] == nil {
		return
	}
	u := s.usage[id]
	if u == nil {
		u = &usageRecord{}
		s.usage[id] = u
	}
	u.Executions++
	u.PromptTokens += uint64(max(0, input))
	u.CompletionTokens += uint64(max(0, output))
}
func (s *Service) lifecycle(method string, raw []byte) (any, error) {
	var request struct {
		RequestID string
		Headers   http.Header
		Body      []byte
		Metadata  map[string]any
		Outcome   string
	}
	if json.Unmarshal(raw, &request) != nil || request.RequestID == "" {
		return nil, reject(400, "request lifecycle ID is required")
	}
	passthrough := map[string]any{"Headers": request.Headers, "Body": request.Body}
	if method == "request.intercept_before" {
		return passthrough, nil
	}
	id, present := request.Metadata["selected_auth_id"].(string)
	if !strings.HasPrefix(id, ID+"-") {
		id = ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for key, entry := range s.selections {
		if now.Sub(entry.At) >= 15*time.Minute {
			delete(s.selections, key)
		}
	}
	if method == "request.intercept_after" {
		if len(s.selections) < 65536 {
			s.selections[request.RequestID] = selection{Auth: id, At: now}
		}
		return passthrough, nil
	}
	if s.completions.seen(request.RequestID, now) {
		return struct{}{}, nil
	}
	if !present {
		id = s.selections[request.RequestID].Auth
	}
	delete(s.selections, request.RequestID)

	if id != "" {
		u := s.usage[id]
		if u == nil {
			u = &usageRecord{}
			s.usage[id] = u
		}
		if strings.EqualFold(request.Outcome, "succeeded") || strings.EqualFold(request.Outcome, "success") {
			u.Success++
		} else {
			u.Failed++
		}
	}
	return struct{}{}, nil
}
