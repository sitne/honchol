package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeLLMServer — 抽出/要約/カードの3種プロンプトに応答する偽 LLM。
// system プロンプトの語で判別: 抽出器 / 要約器 / 保守器。
func fakeLLMServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &req)
		sys := ""
		if len(req.Messages) > 0 {
			sys = req.Messages[0].Content
		}
		var content string
		switch {
		case strings.Contains(sys, "抽出器"):
			content = `{"observations":[` +
				`{"text":"aliceは較正の研究をしている","kind":"explicit","sources":[1]},` +
				`{"text":"温度スケーリング実験は中止になった","kind":"inductive","sources":[3]},` +
				`{"text":"aliceは較正データの取扱いに注意深い","kind":"explicit","sources":[4]}]}`
		case strings.Contains(sys, "要約器"):
			content = "テスト会話の要約です。較正研究と実験中止の経緯が含まれる。"
		case strings.Contains(sys, "保守器"):
			content = `{"card":["テストカード項目"]}`
		case strings.Contains(sys, "対話層"):
			content = "記憶によると、較正研究の話です。"
		case strings.Contains(sys, "キーワード"):
			content = `{"keywords":["較正","calibration"]}`
		default:
			content = "{}"
		}
		resp, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": content}}},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(resp)
	}))
}

// fakeJudgeServer — D1/D2/D4 の choice に応答する偽 judge（sysone 封筒）。
func fakeJudgeServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			State     string                    `json:"state"`
			Questions map[string]map[string]any `json:"questions"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &req)
		ans := map[string]any{}
		for qid, q := range req.Questions {
			instr, _ := q["instructions"].(string)
			var choice string
			switch {
			case strings.HasPrefix(qid, "d1_"):
				choice = "別"
				if strings.Contains(instr, "較正の研究をしている") {
					choice = "重複"
				}
			case strings.HasPrefix(qid, "d2_"):
				choice = "整合"
				if strings.Contains(instr, "中止") {
					choice = "矛盾"
				}
			case strings.HasPrefix(qid, "d4_"):
				choice = "一時的"
				if strings.Contains(instr, "較正") {
					choice = "安定した情報"
				}
			}
			ans[qid] = map[string]any{"choice": choice, "confidence": 0.9}
		}
		resp, _ := json.Marshal(map[string]any{"model": "fake-judge", "answers": ans})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(resp)
	}))
}

func TestDerivePipeline(t *testing.T) {
	st := testStore(t)
	ws, sid := "w", "s1"

	add := func(pid, content string) string {
		m, err := st.AddMessage(ws, sid, pid, content, "", "{}")
		if err != nil {
			t.Fatal(err)
		}
		return m.ID
	}
	m1 := add("alice", "私は較正の研究をしている")
	_ = m1
	add("agent", "了解です")
	add("alice", "温度スケーリング実験は中止になった")
	add("alice", "aliceは較正データの取扱いに注意深い")

	if err := st.AddDerivedConclusion(ws, "agent", "alice", "aliceは較正の研究をしている", "explicit", nil, sid); err != nil {
		t.Fatal(err)
	}
	if err := st.AddDerivedConclusion(ws, "agent", "alice", "温度スケーリング実験は来週実施する", "explicit", nil, sid); err != nil {
		t.Fatal(err)
	}

	llmSrv := fakeLLMServer(t)
	defer llmSrv.Close()
	jSrv := fakeJudgeServer(t)
	defer jSrv.Close()

	t.Setenv("FAKE_LLM_KEY", "x")
	srv := &apiServer{
		st:  st,
		jc:  NewJudgeClient(JudgeConfig{Kind: "sysone", URL: jSrv.URL, Timeout: 5 * time.Second}),
		llm: NewLLMClient(LLMConfig{BaseURL: llmSrv.URL, Model: "fake-model", KeyEnv: "FAKE_LLM_KEY", Timeout: 5 * time.Second}),
	}

	opts := DeriveOpts{BatchPerSession: 40, SessionsPerRun: 20, MaxBatches: 10, QuietMin: 0, MinNewForSummary: 2, MaxTries: 3, MaxCandidates: 8}
	stats, err := srv.deriveRun(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Messages != 4 || stats.Candidates != 3 {
		t.Fatalf("stats = %+v", stats)
	}
	if stats.Inserted != 2 || stats.Merged != 1 || stats.Contradict != 1 || stats.Cards != 1 || stats.Summaries != 1 {
		t.Fatalf("stats = %+v", stats)
	}

	rows, total, err := st.ListConclusions(ws, "agent", "alice", 1, 100, false)
	if err != nil {
		t.Fatal(err)
	}
	if total != 4 {
		t.Fatalf("conclusions total = %d (rows=%d)", total, len(rows))
	}
	var c0, contra string
	for _, r := range rows {
		switch {
		case r.Level == "contradiction":
			contra = r.ID
		case r.Content == "aliceは較正の研究をしている":
			c0 = r.ID
		}
	}
	if contra == "" || c0 == "" {
		t.Fatalf("contra=%q c0=%q rows=%+v", contra, c0, rows)
	}
	srcs, err := st.GetConclusionSources(c0)
	if err != nil {
		t.Fatal(err)
	}
	merged := false
	for _, s := range srcs {
		if s == m1 {
			merged = true
		}
	}
	if !merged {
		t.Fatalf("C0 sources = %v (want contains %s)", srcs, m1)
	}

	total2, completed, pending, dead, err := st.QueueCounts(ws)
	if err != nil {
		t.Fatal(err)
	}
	if total2 != 4 || completed != 4 || pending != 0 || dead != 0 {
		t.Fatalf("queue = total %d completed %d pending %d dead %d", total2, completed, pending, dead)
	}

	sc, ok, err := st.GetSummary(ws, sid)
	if err != nil || !ok || !strings.Contains(sc, "要約") {
		t.Fatalf("summary ok=%v err=%v content=%q", ok, err, sc)
	}

	card, ok2, err := st.GetCard(ws, "agent", "alice")
	if err != nil || !ok2 || len(card) != 1 || card[0] != "テストカード項目" {
		t.Fatalf("card = %v ok=%v err=%v", card, ok2, err)
	}

	// 2回目: 未処理なし → ノーオペ
	stats2, err := srv.deriveRun(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if stats2.Batches != 0 || stats2.Messages != 0 || stats2.Inserted != 0 {
		t.Fatalf("second run stats = %+v", stats2)
	}
}
