package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func quotaFixture(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	if r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Connect-Protocol-Version") != "1" {
		t.Error("invalid dashboard RPC transport")
	}
	w.Header().Set("Content-Type", "application/json")
	switch strings.TrimPrefix(r.URL.Path, "/aiserver.v1.DashboardService/") {
	case "GetPlanInfo":
		_, _ = w.Write([]byte(`{"planInfo":{"planName":"Pro","price":"$20/mo","billingCycleEnd":"1793689075000"},"token":"must-not-leak"}`))
	case "GetCurrentPeriodUsage":
		_, _ = w.Write([]byte(`{"billingCycleStart":"1791010675000","billingCycleEnd":"1793689075000","planUsage":{"autoPercentUsed":0.12222222222222222,"apiPercentUsed":6.933333333333333,"includedSpend":133,"remaining":1867,"limit":2000}}`))
	case "GetHardLimit":
		_, _ = w.Write([]byte(`{"noUsageBasedAllowed":true}`))
	case "GetSandUsageStatus":
		_, _ = w.Write([]byte(`{"usagePercent":0,"onDemandSettings":{"visible":true,"enabled":false,"dashboardUrl":"https://private.invalid/token"}}`))
	default:
		t.Error("unverified endpoint queried")
		w.WriteHeader(404)
	}
}
func TestLiveDashboardQuotaContract(t *testing.T) {
	s := testService(t, func(w http.ResponseWriter, r *http.Request) { quotaFixture(t, w, r) })
	c := fixtureCredentials()
	raw := marshal(t, map[string]any{"auth_index": "one", "auth_id": ID + "-one.json", "provider": ID, "storage_json": marshal(t, c)})
	data, ok := s.Call("quota.fetch", raw)
	var envelope struct{ Result quotaResponse }
	if !ok || json.Unmarshal(data, &envelope) != nil {
		t.Fatal("native quota call failed")
	}
	result := envelope.Result
	if result.Subscription == nil || result.Subscription.Plan != "Pro" || len(result.Groups) != 3 {
		t.Fatal("provider plan or buckets missing")
	}
	metrics := map[string]float64{}
	for _, m := range result.Summary {
		metrics[m.Key] = m.Value
	}
	if metrics["included_remaining"] != 18.67 || metrics["included_used"] != 1.33 || metrics["included_limit"] != 20 || metrics["on_demand_enabled"] != 0 || metrics["plan_monthly_price"] != 20 {
		t.Fatal("currency/status normalization incorrect")
	}
	if result.Groups[0].Buckets[0].RemainingFraction < 0.99 || result.Groups[1].Buckets[0].RemainingFraction > 0.94 || result.Groups[2].Buckets[0].RemainingFraction != 1 {
		t.Fatal("percentage scale or legitimate zero lost")
	}
	if result.Groups[0].Buckets[0].ResetTime != "2026-11-03T06:57:55Z" || result.Groups[2].Buckets[0].ResetTime != "" {
		t.Fatal("reset date changed or missing reset invented")
	}
	if strings.Contains(string(data), "must-not-leak") || strings.Contains(string(data), "private.invalid") || strings.Contains(string(data), c.AccessToken) {
		t.Fatal("upstream private metadata escaped quota response")
	}
	if _, err := s.fetchQuota(context.Background(), []byte(`{}`)); err == nil {
		t.Fatal("invalid credential accepted")
	}
}
func TestQuotaCacheConcurrencyAndCredentialIsolation(t *testing.T) {
	var calls atomic.Int32
	s := testService(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); quotaFixture(t, w, r) })
	var tasks sync.WaitGroup
	for range 20 {
		tasks.Add(1)
		go func() {
			defer tasks.Done()
			result := s.subscriptionQuota(context.Background(), fixtureCredentials())
			if len(result.Groups) != 3 {
				t.Error("concurrent snapshot missing quota")
			}
		}()
	}
	tasks.Wait()
	if calls.Load() != 4 {
		t.Fatal("duplicate dashboard requests for a cached account")
	}
	second := fixtureCredentials()
	second.AccountID = "different"
	_ = s.subscriptionQuota(context.Background(), second)
	if calls.Load() != 8 {
		t.Fatal("quota cache crossed account identity")
	}
	second.AccessToken = "rotated-token"
	_ = s.subscriptionQuota(context.Background(), second)
	if calls.Load() != 12 {
		t.Fatal("quota cache survived token rotation")
	}
}
func TestQuotaFailuresNeverBecomeZeroUsage(t *testing.T) {
	for _, payload := range []string{`null`, `{"planUsage":{"autoPercentUsed":-1,"apiPercentUsed":-9},"billingCycleEnd":"invalid"}`, `{"usagePercent":null}`, strings.Repeat("x", (1<<20)+1)} {
		t.Run(payload[:min(len(payload), 50)], func(t *testing.T) {
			s := testService(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				// Deliberately malformed static JSON fixture; no user input or HTML rendering.
				// nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter
				_, _ = w.Write([]byte(payload))
			})
			result := s.subscriptionQuota(context.Background(), fixtureCredentials())
			if result.Subscription != nil || len(result.Groups) != 0 || len(result.Summary) != 0 {
				t.Fatal("unavailable data converted to a fabricated quota")
			}
		})
	}
	s := testService(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "GetPlanInfo") {
			quotaFixture(t, w, r)
			return
		}
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`secret upstream error`))
	})
	result := s.subscriptionQuota(context.Background(), fixtureCredentials())
	if result.Subscription == nil || len(result.Groups) != 0 {
		t.Fatal("partial failure lost verified plan or invented usage")
	}
}
func TestQuotaCancellationAndUnsupportedMethod(t *testing.T) {
	s := testService(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("cancelled/unsupported request reached upstream")
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := s.subscriptionQuota(ctx, fixtureCredentials())
	if result.Subscription != nil || len(result.Groups) != 0 {
		t.Fatal("cancelled request returned provider data")
	}
	if s.quotaRPC(context.Background(), "test-token", "SetHardLimit", &struct{}{}) {
		t.Fatal("write endpoint accepted")
	}
}
