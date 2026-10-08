package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestQueryExpansion — 長文クエリで直接候補ゼロ → LLM展開 → 再検索でヒットする（spec §3.3.1）。
func TestQueryExpansion(t *testing.T) {
	st := testStore(t)
	if err := st.AddDerivedConclusion("w", "agent", "alice", "aliceは較正の研究をしている", "explicit", nil, ""); err != nil {
		t.Fatal(err)
	}
	llmSrv := fakeLLMServer(t)
	defer llmSrv.Close()
	t.Setenv("FAKE_LLM_KEY", "x")
	srv := &apiServer{st: st, llm: NewLLMClient(LLMConfig{BaseURL: llmSrv.URL, Model: "m", KeyEnv: "FAKE_LLM_KEY", Timeout: 5 * time.Second})}
	rows := srv.rankedSearchConclusions(context.Background(), "w", "agent", "alice", "較正について何を知っている？", 10)
	if len(rows) == 0 {
		t.Fatal("expansion path did not find the conclusion")
	}
	if rows[0].Content != "aliceは較正の研究をしている" {
		t.Fatalf("rows = %+v", rows)
	}
}

// TestConclusionExpansionShortQuery — 短語クエリの直接候補ゼロでも展開で拾える（較正→calibration 経由）。
func TestConclusionExpansionShortQuery(t *testing.T) {
	st := testStore(t)
	// 「較正」の literal を含まないが、展開キーワードでヒットし得る結論
	if err := st.AddDerivedConclusion("w", "agent", "alice", "温度スケーリングのcalibrationは完了した", "explicit", nil, ""); err != nil {
		t.Fatal(err)
	}
	llmSrv := fakeLLMServer(t)
	defer llmSrv.Close()
	t.Setenv("FAKE_LLM_KEY", "x")
	srv := &apiServer{st: st, llm: NewLLMClient(LLMConfig{BaseURL: llmSrv.URL, Model: "m", KeyEnv: "FAKE_LLM_KEY", Timeout: 5 * time.Second})}
	rows := srv.rankedSearchConclusions(context.Background(), "w", "agent", "alice", "較正", 10)
	if len(rows) == 0 || !strings.Contains(rows[0].Content, "calibration") {
		t.Fatalf("short-query expansion failed: %+v", rows)
	}
}
