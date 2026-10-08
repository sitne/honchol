package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestJudgeRelevanceMbja — スコア→tier 変換と閾値の検証。
func TestJudgeRelevanceMbja(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{"scores": []float64{0.92, 0.4, 0.03}, "model": "mbja-test"}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()
	c := NewJudgeClient(JudgeConfig{Kind: "mbja", URL: ts.URL, Timeout: 5 * time.Second})
	tiers, meta, err := judgeRelevance(context.Background(), c, "較正", []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("judgeRelevance: %v", err)
	}
	if meta != "mbja-test" {
		t.Fatalf("meta = %q", meta)
	}
	want := []int{tierRelated, tierWeak, tierUnrelated}
	for i := range want {
		if tiers[i] != want[i] {
			t.Fatalf("tiers[%d] = %d, want %d", i, tiers[i], want[i])
		}
	}
}

// TestJudgeRelevanceMbjaFallback — mbja 故障時は fallback の古典封筒（choice）へ。
func TestJudgeRelevanceMbjaFallback(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer bad.Close()
	fb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Questions map[string]map[string]any `json:"questions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		answers := map[string]any{}
		for k := range in.Questions {
			answers[k] = map[string]any{"choice": "関連"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"answers": answers, "model": "shim"})
	}))
	defer fb.Close()
	c := NewJudgeClient(JudgeConfig{Kind: "mbja", URL: bad.URL, Timeout: 2 * time.Second,
		Fallback: &JudgeConfig{Kind: "sysone", URL: fb.URL, Timeout: 5 * time.Second}})
	tiers, _, err := judgeRelevance(context.Background(), c, "較正", []string{"a", "b"})
	if err != nil {
		t.Fatalf("judgeRelevance fallback: %v", err)
	}
	if tiers[0] != tierRelated || tiers[1] != tierRelated {
		t.Fatalf("tiers = %v, want [2 2]", tiers)
	}
}
