package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

func (s *Service) discoverCapacity(parent context.Context, token string) map[string]int64 {
	out := make(map[string]int64)
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+"/aiserver.v1.AiService/AvailableModels", bytes.NewBufferString(`{"useModelParameters":true,"useReactModelPicker":true,"excludeMaxNamedModels":true}`))
	if err != nil {
		return out
	}
	headers(req, token, "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return out
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return out
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil || len(raw) > 4<<20 {
		return out
	}
	var data struct {
		Models []struct {
			Name, ServerModelName  string
			LegacySlugs, IDAliases []string
			ContextTokenLimit      int64
			SupportsNonMaxMode     *bool
			Variants               []struct {
				LegacySlug string
				IsMaxMode  bool
			}
		}
	}
	if json.Unmarshal(raw, &data) != nil {
		return out
	}
	for _, m := range data.Models {
		if m.ContextTokenLimit <= 0 || (m.SupportsNonMaxMode != nil && !*m.SupportsNonMaxMode) {
			continue
		}
		aliases := append([]string{m.Name, m.ServerModelName}, m.LegacySlugs...)
		aliases = append(aliases, m.IDAliases...)
		hasNonMax := len(m.Variants) == 0
		for _, v := range m.Variants {
			if !v.IsMaxMode {
				hasNonMax = true
				aliases = append(aliases, v.LegacySlug)
			}
		}
		if !hasNonMax {
			continue
		}
		for _, id := range aliases {
			if id != "" && !strings.HasSuffix(strings.ToLower(id), "-max") {
				out[id] = m.ContextTokenLimit
			}
		}
	}
	return out
}

func (s *Service) countTokens(raw []byte) (any, error) {
	// Counting uses the same admission calculation, with a non-secret placeholder.
	var request map[string]json.RawMessage
	if json.Unmarshal(raw, &request) != nil {
		return nil, reject(400, "invalid counting request")
	}
	placeholder, _ := json.Marshal(credentials{Type: ID, AccessToken: "count", RefreshToken: "count", AccountID: "count"})
	request["StorageJSON"], _ = json.Marshal(placeholder)
	encoded, _ := json.Marshal(request)
	_, c, _, err := parseExecution(encoded)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(map[string]any{"total_tokens": c.InputUnits, "estimated": true, "count_method": "conservative_utf8_bytes_v1", "client_context_limit": 1000000})
	return map[string]any{"Payload": body, "Headers": http.Header{"Content-Type": {"application/json"}}}, nil
}
