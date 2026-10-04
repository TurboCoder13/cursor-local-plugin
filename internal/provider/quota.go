package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// These fields follow CPA's normalized quota contract. Provider billing data
// and local process estimates use distinct keys and are never combined.
type quotaMetric struct {
	Key      string  `json:"key"`
	Label    string  `json:"label"`
	Value    float64 `json:"value"`
	Format   string  `json:"format"`
	Currency string  `json:"currency,omitempty"`
}
type quotaBucket struct {
	Window            string  `json:"window"`
	RemainingFraction float64 `json:"remainingFraction"`
	ResetTime         string  `json:"resetTime,omitempty"`
	Description       string  `json:"description,omitempty"`
}
type quotaGroup struct {
	DisplayName string        `json:"displayName"`
	Buckets     []quotaBucket `json:"buckets"`
}
type quotaSubscription struct {
	Plan string `json:"plan"`
}
type quotaResponse struct {
	Subscription *quotaSubscription `json:"subscription,omitempty"`
	Summary      []quotaMetric      `json:"summary"`
	Groups       []quotaGroup       `json:"groups,omitempty"`
}
type quotaSnapshot struct {
	Done  chan struct{}
	Until time.Time
	Data  quotaResponse
}

const quotaCacheTTL = time.Minute
const quotaTimeout = 12 * time.Second

func (s *Service) fetchQuota(ctx context.Context, raw []byte) (any, error) {
	var request struct {
		AuthIndex   string `json:"auth_index"`
		AuthID      string `json:"auth_id"`
		Provider    string `json:"provider"`
		StorageJSON []byte `json:"storage_json"`
	}
	if json.Unmarshal(raw, &request) != nil || request.Provider != ID || request.AuthIndex == "" || !strings.HasPrefix(request.AuthID, ID+"-") || len(request.AuthID) > 1024 || len(request.AuthIndex) > 1024 {
		return nil, reject(400, "a Cursor credential is required for quota")
	}
	credential, err := parseCredentials(request.StorageJSON)
	if err != nil {
		return nil, err
	}
	result := s.subscriptionQuota(ctx, credential)
	// Do not append to a cached slice; another caller may read it concurrently.
	result.Summary = append(append([]quotaMetric(nil), result.Summary...), s.localQuota(request.AuthID)...)
	return result, nil
}

func (s *Service) localQuota(id string) []quotaMetric {
	s.mu.Lock()
	usage := usageRecord{}
	if value := s.usage[id]; value != nil {
		usage = *value
	}
	s.mu.Unlock()
	return []quotaMetric{
		{Key: "local_executions", Label: "Local executions since proxy restart", Value: float64(usage.Executions), Format: "number"},
		{Key: "local_success", Label: "Completed requests since proxy restart", Value: float64(usage.Success), Format: "number"},
		{Key: "local_failed", Label: "Failed requests since proxy restart", Value: float64(usage.Failed), Format: "number"},
		{Key: "estimated_input_tokens", Label: "Estimated input tokens since proxy restart", Value: float64(usage.PromptTokens), Format: "number"},
		{Key: "estimated_output_tokens", Label: "Estimated output tokens since proxy restart", Value: float64(usage.CompletionTokens), Format: "number"},
	}
}

func (s *Service) subscriptionQuota(ctx context.Context, credential credentials) quotaResponse {
	// Cache by identity AND token so refresh/account changes cannot reuse a prior
	// account's response. Pending entries are never evicted or duplicated.
	key := sha256.Sum256([]byte(credential.AccountID + "\x00" + credential.AccessToken))
	s.mu.Lock()
	if s.quotas == nil {
		s.quotas = make(map[[32]byte]*quotaSnapshot)
	}
	if entry := s.quotas[key]; entry != nil && (entry.Done != nil || time.Now().Before(entry.Until)) {
		done := entry.Done
		if done == nil {
			data := entry.Data
			s.mu.Unlock()
			return data
		}
		s.mu.Unlock()
		select {
		case <-done:
			s.mu.Lock()
			data := entry.Data
			s.mu.Unlock()
			return data
		case <-ctx.Done():
			return quotaResponse{Summary: []quotaMetric{}}
		}
	}
	delete(s.quotas, key)
	if len(s.quotas) >= 64 {
		var oldestKey [32]byte
		var oldest *quotaSnapshot
		for k, entry := range s.quotas {
			if entry.Done == nil && (oldest == nil || entry.Until.Before(oldest.Until)) {
				oldestKey, oldest = k, entry
			}
		}
		if oldest == nil {
			s.mu.Unlock()
			return quotaResponse{Summary: []quotaMetric{}}
		}
		delete(s.quotas, oldestKey)
	}
	entry := &quotaSnapshot{Done: make(chan struct{})}
	s.quotas[key] = entry
	s.mu.Unlock()
	data := s.loadSubscriptionQuota(ctx, credential.AccessToken)
	s.mu.Lock()
	entry.Data, entry.Until = data, time.Now().Add(quotaCacheTTL)
	close(entry.Done)
	entry.Done = nil
	s.mu.Unlock()
	return data
}

// Only the four verified read-only RPCs may receive the saved Cursor token.
// Unknown JSON fields, including URLs and identity metadata, are discarded.
func (s *Service) quotaRPC(ctx context.Context, token, method string, result any) bool {
	switch method {
	case "GetPlanInfo", "GetCurrentPeriodUsage", "GetHardLimit", "GetSandUsageStatus":
	default:
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+"/aiserver.v1.DashboardService/"+method, bytes.NewReader([]byte(`{}`)))
	if err != nil {
		return false
	}
	headers(req, token, "application/json")
	response, err := s.client.Do(req)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return false
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 || len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return false
	}
	return json.Unmarshal(raw, result) == nil
}

type cursorPlanInfo struct {
	PlanInfo *struct {
		PlanName        string      `json:"planName"`
		Price           string      `json:"price"`
		BillingCycleEnd json.Number `json:"billingCycleEnd"`
	} `json:"planInfo"`
}
type cursorPeriodUsage struct {
	BillingCycleStart json.Number `json:"billingCycleStart"`
	BillingCycleEnd   json.Number `json:"billingCycleEnd"`
	PlanUsage         *struct {
		AutoPercentUsed *float64 `json:"autoPercentUsed"`
		APIPercentUsed  *float64 `json:"apiPercentUsed"`
		IncludedSpend   *float64 `json:"includedSpend"`
		Remaining       *float64 `json:"remaining"`
		Limit           *float64 `json:"limit"`
	} `json:"planUsage"`
	SpendLimitUsage *struct {
		TotalSpend *float64 `json:"totalSpend"`
	} `json:"spendLimitUsage"`
}
type cursorHardLimit struct {
	NoUsageBasedAllowed bool `json:"noUsageBasedAllowed"`
}
type cursorBotUsage struct {
	UsagePercent     *float64 `json:"usagePercent"`
	NextReset        string   `json:"nextResetTimestampUtc"`
	OnDemandSettings *struct {
		Enabled bool `json:"enabled"`
	} `json:"onDemandSettings"`
}

var monthlyPricePattern = regexp.MustCompile(`^\$([0-9]+(?:\.[0-9]{1,2})?)/mo$`)

func timestampMS(number json.Number) int64 {
	value, err := strconv.ParseInt(string(number), 10, 64)
	if err != nil || value < 946684800000 || value > 4102444800000 {
		return 0
	}
	return value
}
func quotaCurrency(key, label string, value *float64) []quotaMetric {
	if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 || *value > 1e12 {
		return nil
	}
	return []quotaMetric{{Key: key, Label: label, Value: *value / 100, Format: "currency", Currency: "USD"}}
}
func quotaWindow(id, label string, used *float64, reset string) []quotaGroup {
	if used == nil || math.IsNaN(*used) || math.IsInf(*used, 0) || *used < 0 || *used > 1e6 {
		return nil
	}
	return []quotaGroup{{DisplayName: label, Buckets: []quotaBucket{{Window: id, RemainingFraction: 1 - math.Min(*used/100, 1), ResetTime: reset, Description: "Usage reported by Cursor"}}}}
}
func (s *Service) loadSubscriptionQuota(parent context.Context, token string) quotaResponse {
	ctx, cancel := context.WithTimeout(parent, quotaTimeout)
	defer cancel()
	var plan cursorPlanInfo
	var usage cursorPeriodUsage
	var hard cursorHardLimit
	var bot cursorBotUsage
	var ok [4]bool
	var tasks sync.WaitGroup
	tasks.Add(4)
	go func() { defer tasks.Done(); ok[0] = s.quotaRPC(ctx, token, "GetPlanInfo", &plan) }()
	go func() { defer tasks.Done(); ok[1] = s.quotaRPC(ctx, token, "GetCurrentPeriodUsage", &usage) }()
	go func() { defer tasks.Done(); ok[2] = s.quotaRPC(ctx, token, "GetHardLimit", &hard) }()
	go func() { defer tasks.Done(); ok[3] = s.quotaRPC(ctx, token, "GetSandUsageStatus", &bot) }()
	tasks.Wait()
	result := quotaResponse{Summary: []quotaMetric{}}
	var end int64
	if ok[0] && plan.PlanInfo != nil {
		if name := strings.TrimSpace(plan.PlanInfo.PlanName); name != "" && len(name) <= 128 {
			result.Subscription = &quotaSubscription{Plan: name}
		}
		end = timestampMS(plan.PlanInfo.BillingCycleEnd)
		if match := monthlyPricePattern.FindStringSubmatch(plan.PlanInfo.Price); len(match) == 2 {
			value, err := strconv.ParseFloat(match[1], 64)
			if err == nil && value <= 1e9 {
				result.Summary = append(result.Summary, quotaMetric{Key: "plan_monthly_price", Label: "Monthly plan price", Value: value, Format: "currency", Currency: "USD"})
			}
		}
	}
	if ok[1] {
		if usageEnd := timestampMS(usage.BillingCycleEnd); usageEnd != 0 {
			end = usageEnd
		}
		if start := timestampMS(usage.BillingCycleStart); start != 0 && start < end {
			result.Summary = append(result.Summary, quotaMetric{Key: "billing_cycle_start_ms", Label: "Billing cycle start", Value: float64(start), Format: "number"})
		}
		reset := ""
		if end != 0 {
			reset = time.UnixMilli(end).UTC().Format(time.RFC3339)
		}
		if usage.PlanUsage != nil {
			p := usage.PlanUsage
			result.Groups = append(result.Groups, quotaWindow("cursor_models", "Cursor Models", p.AutoPercentUsed, reset)...)
			result.Groups = append(result.Groups, quotaWindow("other_models", "Other Models", p.APIPercentUsed, reset)...)
			result.Summary = append(result.Summary, quotaCurrency("included_used", "Included usage spent", p.IncludedSpend)...)
			result.Summary = append(result.Summary, quotaCurrency("included_remaining", "Included usage remaining", p.Remaining)...)
			result.Summary = append(result.Summary, quotaCurrency("included_limit", "Included usage allowance", p.Limit)...)
		}
		if usage.SpendLimitUsage != nil {
			result.Summary = append(result.Summary, quotaCurrency("on_demand_spent", "On-demand spending", usage.SpendLimitUsage.TotalSpend)...)
		}
	}
	if end != 0 {
		result.Summary = append(result.Summary, quotaMetric{Key: "billing_cycle_end_ms", Label: "Usage resets", Value: float64(end), Format: "number"})
	}
	if ok[3] {
		reset := ""
		if value, err := time.Parse(time.RFC3339, bot.NextReset); err == nil && value.Year() >= 2000 && value.Year() <= 2100 {
			reset = value.UTC().Format(time.RFC3339)
		}
		result.Groups = append(result.Groups, quotaWindow("grok_bot", "Grok Bot · weekly", bot.UsagePercent, reset)...)
	}
	// Preserve unknown as unknown. A verified prohibition or an explicit
	// on-demand-settings object establishes the boolean; allowance alone does not.
	if (ok[2] && hard.NoUsageBasedAllowed) || (ok[3] && bot.OnDemandSettings != nil) {
		enabled := float64(0)
		if !(ok[2] && hard.NoUsageBasedAllowed) && bot.OnDemandSettings.Enabled {
			enabled = 1
		}
		result.Summary = append(result.Summary, quotaMetric{Key: "on_demand_enabled", Label: "On-demand spending enabled", Value: enabled, Format: "number"})
	}
	// GetHardLimit does not document monetary units; never invent a budget.
	return result
}
