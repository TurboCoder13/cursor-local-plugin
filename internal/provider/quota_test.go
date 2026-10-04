package provider

import (
	"encoding/json"
	"testing"
)

func TestQuotaContractAndAccountIsolation(t *testing.T) {
	s := NewService(func(string, any) (json.RawMessage, error) {
		t.Fatal("local quota must not read credentials or contact upstream")
		return nil, nil
	})
	defer s.Shutdown()
	first, second := ID+"-first.json", ID+"-second.json"
	s.recordExecution(first, 25, 10)
	s.recordExecution(second, 999, 888)
	_, _ = s.lifecycle("request.complete", marshal(t, map[string]any{"RequestID": "quota-success", "Outcome": "success", "Metadata": map[string]any{"selected_auth_id": first}}))
	reply, ok := encode(map[string]any{"summary": s.localQuota(first)}, nil)
	var envelope struct {
		Result struct {
			Summary      []quotaMetric
			Groups       json.RawMessage
			Subscription json.RawMessage
		}
	}
	if !ok || json.Unmarshal(reply, &envelope) != nil {
		t.Fatal("quota call failed")
	}
	got := map[string]float64{}
	for _, metric := range envelope.Result.Summary {
		got[metric.Key] = metric.Value
	}
	if len(got) != 5 || got["local_executions"] != 1 || got["local_success"] != 1 || got["estimated_input_tokens"] != 25 || got["estimated_output_tokens"] != 10 || envelope.Result.Groups != nil || envelope.Result.Subscription != nil {
		t.Fatal("local counters mixed accounts or fabricated subscription quota")
	}
	for _, raw := range []string{`{}`, `null`, `{"provider":"codex","auth_id":"cursor-local-first.json","auth_index":"one"}`, `{"provider":"cursor-local","auth_id":"codex-first.json","auth_index":"one"}`} {
		if _, ok := s.Call("quota.fetch", []byte(raw)); ok {
			t.Fatal("invalid quota credential accepted")
		}
	}
	if _, ok := s.Call("quota.reset", []byte(`{}`)); ok {
		t.Fatal("subscription reset was advertised")
	}
	described, ok := s.Call("quota.describe", []byte(`{}`))
	var description struct {
		Result struct {
			Providers []string `json:"supported_providers"`
			Reset     bool     `json:"supports_reset"`
		}
	}
	if !ok || json.Unmarshal(described, &description) != nil || len(description.Result.Providers) != 1 || description.Result.Providers[0] != ID || description.Result.Reset {
		t.Fatal("invalid quota description")
	}
}
