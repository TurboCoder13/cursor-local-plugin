package provider

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"
)

//go:embed management.html
var managementPage []byte

const managementRoot = "/plugins/" + ID
const resourceRoot = "/v0/resource/plugins/" + ID

type managedFile struct {
	ID          string `json:"id"`
	Index       string `json:"auth_index"`
	Name        string `json:"name"`
	Path        string `json:"path"`
	Source      string `json:"source"`
	Type        string `json:"type"`
	Provider    string `json:"provider"`
	Label       string `json:"label"`
	Status      string `json:"status"`
	Success     int64  `json:"success"`
	Failed      int64  `json:"failed"`
	RuntimeOnly bool   `json:"runtime_only"`
}
type storedAuth struct {
	Name string          `json:"name"`
	JSON json.RawMessage `json:"json"`
}
type managementResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

func managementRegistration() any {
	return map[string]any{"routes": []any{map[string]string{"Method": "GET", "Path": managementRoot + "/status"}, map[string]string{"Method": "PUT", "Path": managementRoot + "/disabled-models"}}, "resources": []any{map[string]string{"Path": "/status", "Menu": "Cursor Local", "Description": "Accounts, model controls and usage / 账户、模型与用量"}}}
}
func managementJSON(status int, value any) (any, error) {
	body, err := json.Marshal(value)
	return managementResponse{StatusCode: status, Headers: http.Header{"Content-Type": {"application/json; charset=utf-8"}, "X-Content-Type-Options": {"nosniff"}, "Cache-Control": {"no-store"}}, Body: body}, err
}
func (s *Service) management(raw []byte) (any, error) { return s.managementWithContext(s.root, raw) }
func (s *Service) managementWithContext(parent context.Context, raw []byte) (any, error) {
	var request struct {
		Method, Path string
		Body         []byte
	}
	if json.Unmarshal(raw, &request) != nil {
		return nil, reject(400, "invalid management request")
	}
	if request.Method == "GET" && request.Path == resourceRoot+"/status" {
		return managementResponse{StatusCode: 200, Body: managementPage, Headers: http.Header{"Content-Type": {"text/html; charset=utf-8"}, "Cache-Control": {"no-store"}, "X-Content-Type-Options": {"nosniff"}, "Referrer-Policy": {"no-referrer"}, "Content-Security-Policy": {"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"}}}, nil
	}
	// Host routes invoke this only after CPA's management-key authentication.
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	switch {
	case request.Method == "GET" && request.Path == "/v0/management"+managementRoot+"/status":
		return s.managementStatus(ctx)
	case request.Method == "PUT" && request.Path == "/v0/management"+managementRoot+"/disabled-models":
		return s.saveDisabled(ctx, request.Body)
	default:
		return managementJSON(404, map[string]string{"error": "not found"})
	}
}
func (s *Service) accountFiles() ([]managedFile, error) {
	if s.host == nil {
		return nil, reject(503, "host callbacks unavailable")
	}
	raw, err := s.host("host.auth.list", struct{}{})
	if err != nil {
		return nil, err
	}
	var data struct{ Files []managedFile }
	if json.Unmarshal(raw, &data) != nil {
		return nil, reject(502, "invalid host account list")
	}
	out := make([]managedFile, 0)
	seen := make(map[string]bool)
	for _, f := range data.Files {
		if f.RuntimeOnly || (f.Provider != ID && f.Type != ID) || (f.Source != "" && f.Source != "file") {
			continue
		}
		physical := f.Path
		if physical == "" {
			physical = f.Name
		}
		if physical == "" || seen[physical] {
			continue
		}
		seen[physical] = true
		if len(out) >= 4096 {
			return nil, reject(502, "persisted account list exceeds its bound")
		}
		out = append(out, f)
	}
	return out, nil
}
func (s *Service) storedAccount(index string) (storedAuth, credentials, error) {
	raw, err := s.host("host.auth.get", map[string]string{"auth_index": index})
	if err != nil {
		return storedAuth{}, credentials{}, err
	}
	var auth storedAuth
	if json.Unmarshal(raw, &auth) != nil {
		return auth, credentials{}, reject(502, "invalid stored account")
	}
	credential, err := parseCredentials(auth.JSON)
	return auth, credential, err
}
func (s *Service) managementStatus(ctx context.Context) (any, error) {
	files, err := s.accountFiles()
	if err != nil {
		return managementJSON(502, map[string]string{"error": "cannot list persisted accounts"})
	}
	accounts := make([]map[string]any, 0)
	records := make([]accountRecord, 0, len(files))
	for _, f := range files {
		_, c, err := s.storedAccount(f.Index)
		records = append(records, accountRecord{File: f, Credential: c, Error: err})
	}
	for _, group := range groupAccounts(records) {
		if ctx.Err() != nil {
			return managementJSON(504, map[string]string{"error": "account status timed out"})
		}
		first := group.Records[0]
		f, c, credentialErr := first.File, first.Credential, first.Error
		usage := usageRecord{}
		metrics := cacheCounters{}
		var successAttempts, failedAttempts int64
		s.mu.Lock()
		for _, record := range group.Records {
			if value := s.usage[record.File.ID]; value != nil {
				usage.Executions += value.Executions
				usage.Success += value.Success
				usage.Failed += value.Failed
				usage.PromptTokens += value.PromptTokens
				usage.CompletionTokens += value.CompletionTokens
			}
			if value := s.cacheUsage[record.File.ID]; value != nil {
				metrics.Hits += value.Hits
				metrics.Misses += value.Misses
				metrics.Invalidations += value.Invalidations
				metrics.Fallbacks += value.Fallbacks
				metrics.Commits += value.Commits
			}
			successAttempts += record.File.Success
			failedAttempts += record.File.Failed
		}
		s.mu.Unlock()
		entry := map[string]any{"auth_index": f.Index, "name": f.Name, "label": f.Label, "status": f.Status, "local_usage": usage, "checkpoint_metrics": metrics, "host_runtime": map[string]any{"scope": "cli_proxy_process", "success_attempts": successAttempts, "failed_attempts": failedAttempts}, "subscription_quota": quotaResponse{Summary: []quotaMetric{}}, "models": []any{}}
		if credentialErr != nil {
			entry["status"] = "credentials unavailable"
		} else {
			entry["subscription_quota"] = s.subscriptionQuota(ctx, c)
			discover := c
			discover.DisabledModels = nil
			credentialRaw, _ := json.Marshal(discover)
			envelope, _ := json.Marshal(map[string]any{"StorageJSON": credentialRaw})
			result, modelErr := s.models(ctx, envelope)
			if modelErr != nil {
				entry["status"] = "model discovery unavailable"
			} else {
				models := result.(map[string]any)["Models"].([]any)
				for _, model := range models {
					m := model.(map[string]any)
					id := strings.TrimPrefix(m["ID"].(string), "cursor/")
					m["disabled"] = slices.Contains(c.DisabledModels, id) || slices.Contains(c.DisabledModels, "cursor/"+id)
				}
				entry["models"] = models
			}
		}
		accounts = append(accounts, entry)
	}
	s.mu.Lock()
	metrics := s.cacheStats
	entries, waiting := len(s.checkpoints), s.waiting
	s.mu.Unlock()
	return managementJSON(200, map[string]any{"provider": ID, "version": Version, "generated_at": time.Now().UTC(), "accounts": accounts, "checkpoint_metrics": metrics, "cache_entries": entries, "session_turns": waiting, "context_policy": map[string]any{"client_context_limit": 1000000, "reserve": 16384, "method": "conservative_utf8_bytes_v1"}})
}
func (s *Service) saveDisabled(ctx context.Context, raw []byte) (any, error) {
	var update struct {
		Index   string   `json:"auth_index"`
		Models  []string `json:"disabled_models"`
		Confirm bool     `json:"confirm_disable_all"`
	}
	if len(raw) > 1<<20 || json.Unmarshal(raw, &update) != nil || update.Index == "" {
		return managementJSON(400, map[string]string{"error": "invalid model update"})
	}
	files, err := s.accountFiles()
	if err != nil {
		return managementJSON(502, map[string]string{"error": "cannot list accounts"})
	}
	persisted := false
	for _, f := range files {
		persisted = persisted || f.Index == update.Index
	}
	if !persisted {
		return managementJSON(404, map[string]string{"error": "persisted Cursor account not found"})
	}
	stored, c, err := s.storedAccount(update.Index)
	if err != nil {
		return managementJSON(502, map[string]string{"error": "cannot read account"})
	}
	c.DisabledModels = nil
	credentialRaw, _ := json.Marshal(c)
	envelope, _ := json.Marshal(map[string]any{"StorageJSON": credentialRaw})
	result, err := s.models(ctx, envelope)
	if err != nil {
		return managementJSON(502, map[string]string{"error": "cannot discover models"})
	}
	known := make(map[string]bool)
	for _, item := range result.(map[string]any)["Models"].([]any) {
		known[strings.TrimPrefix(item.(map[string]any)["ID"].(string), "cursor/")] = true
	}
	disabled := make([]string, 0)
	for _, id := range update.Models {
		id = strings.TrimPrefix(strings.TrimSpace(id), "cursor/")
		if !known[id] {
			return managementJSON(400, map[string]string{"error": "unknown model"})
		}
		if !slices.Contains(disabled, id) {
			disabled = append(disabled, id)
		}
	}
	if len(disabled) == len(known) && !update.Confirm {
		return managementJSON(400, map[string]string{"error": "disabling all models requires confirmation"})
	}
	slices.Sort(disabled)
	var fields map[string]json.RawMessage
	if json.Unmarshal(stored.JSON, &fields) != nil {
		return managementJSON(502, map[string]string{"error": "invalid account data"})
	}
	fields["disabled_models"], _ = json.Marshal(disabled)
	updated, _ := json.Marshal(fields)
	if _, err := s.host("host.auth.save", map[string]any{"name": stored.Name, "json": json.RawMessage(updated)}); err != nil {
		return managementJSON(502, map[string]string{"error": "cannot save account"})
	}
	return managementJSON(200, map[string]any{"auth_index": update.Index, "disabled_models": disabled})
}
