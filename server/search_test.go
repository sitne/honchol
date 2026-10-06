package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	st, err := OpenStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// TestSearchCandidatesFTS — 候補段: CJK/EN サブストリング・重複除去の確認。
func TestSearchCandidatesFTS(t *testing.T) {
	st := testStore(t)
	if !st.ftsOK {
		t.Fatal("fts not ok")
	}
	t.Logf("ftsTrigram=%v", st.ftsTrigram)
	must := func(content string) {
		if _, err := st.AddMessage("w", "s1", "p1", content, "", "{}"); err != nil {
			t.Fatal(err)
		}
	}
	must("昨日の較正テストは成功した")
	must("The calibration run succeeded")
	must("まったく別の話題です")

	// CJK サブストリング（trigram）
	got, err := st.SearchCandidates("w", "", "", "較正テスト", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.Contains(got[0].Content, "較正テスト") {
		t.Fatalf("CJK: got %d rows %+v", len(got), got)
	}
	// EN サブストリング
	got, err = st.SearchCandidates("w", "", "", "calibration", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.Contains(got[0].Content, "calibration") {
		t.Fatalf("EN: got %d rows %+v", len(got), got)
	}
	// 重複除去（FTS と LIKE の両方にヒットしても1件）
	if len(got) != 1 {
		t.Fatalf("dedupe: got %d", len(got))
	}
}

// TestShortQueryLIKE — 3ルーン未満のクエリは LIKE で拾える（FTS はスキップ）。
func TestShortQueryLIKE(t *testing.T) {
	st := testStore(t)
	if _, err := st.AddMessage("w", "s1", "p1", "較正の話", "", "{}"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddMessage("w", "s1", "p1", "その他の話", "", "{}"); err != nil {
		t.Fatal(err)
	}
	got, err := st.SearchCandidates("w", "", "", "較", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.Contains(got[0].Content, "較正") {
		t.Fatalf("short: got %d %+v", len(got), got)
	}
}

// fakeSysoneJudge — qid ごとに instruction 内のキーワードで判定を変える偽 judge。
// 「パスタ」→弱関連 / 「較正」→関連 / それ以外→無関係。
func fakeSysoneJudge(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Questions map[string]struct {
				Instructions string `json:"instructions"`
			} `json:"questions"`
		}
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &body); err != nil {
			w.WriteHeader(400)
			return
		}
		answers := map[string]any{}
		for qid, q := range body.Questions {
			choice := "無関係"
			switch {
			case strings.Contains(q.Instructions, "較正"):
				choice = "関連"
			case strings.Contains(q.Instructions, "パスタ"):
				choice = "弱関連"
			}
			answers[qid] = map[string]any{
				"choice":        choice,
				"probabilities": map[string]float64{choice: 0.9},
				"confidence":    0.9,
			}
		}
		resp := map[string]any{"model": "fake-judge", "answers": answers}
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

// TestRankedSearchJudgeRerank — 二段検索: judge の tier で並べ替え・無関係を落とす。
func TestRankedSearchJudgeRerank(t *testing.T) {
	st := testStore(t)
	must := func(content string) {
		if _, err := st.AddMessage("w", "s1", "p1", content, "", "{}"); err != nil {
			t.Fatal(err)
		}
	}
	must("テスト前にパスタを作った")
	must("テストの結果は較正テストで良好")
	must("テストとは無関係な雑談")

	jsrv := fakeSysoneJudge(t)
	defer jsrv.Close()
	a := &apiServer{st: st, jc: NewJudgeClient(JudgeConfig{Kind: "sysone", URL: jsrv.URL, Timeout: 5 * time.Second})}

	rows, mode := a.rankedSearchMessages(context.Background(), "w", "s1", "", "テスト", 10)
	if len(rows) != 2 {
		t.Fatalf("got %d rows (mode=%s): %+v", len(rows), mode, rows)
	}
	if !strings.Contains(rows[0].Content, "較正") {
		t.Fatalf("first = %q (want 較正)", rows[0].Content)
	}
	if !strings.Contains(rows[1].Content, "パスタ") {
		t.Fatalf("second = %q (want パスタ)", rows[1].Content)
	}
	if !strings.HasPrefix(mode, "judged:") {
		t.Fatalf("mode = %s", mode)
	}
}

// TestRankedSearchJudgeDown — judge 障害時は素の候補順にフォールバック（fail-open）。
func TestRankedSearchJudgeDown(t *testing.T) {
	st := testStore(t)
	for _, c := range []string{"テストA 較正の記録", "テストB 別件の記録"} {
		if _, err := st.AddMessage("w", "s1", "p1", c, "", "{}"); err != nil {
			t.Fatal(err)
		}
	}
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = io.WriteString(w, "down")
	}))
	defer down.Close()
	a := &apiServer{st: st, jc: NewJudgeClient(JudgeConfig{Kind: "sysone", URL: down.URL, Timeout: 2 * time.Second})}

	rows, mode := a.rankedSearchMessages(context.Background(), "w", "s1", "", "テスト", 10)
	if len(rows) != 2 {
		t.Fatalf("got %d rows (mode=%s)", len(rows), mode)
	}
	if !strings.Contains(mode, "fallback") {
		t.Fatalf("mode = %s", mode)
	}
}

// TestConclusionCandidates — 結論側の候補段（FTSトリガ経由）確認。
func TestConclusionCandidates(t *testing.T) {
	st := testStore(t)
	if _, err := st.CreateConclusion("w", "obs", "obd", "炉心の温度は1200度で安定していた", "explicit", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateConclusion("w", "obs", "obd", "今日はラーメンを食べた", "explicit", nil); err != nil {
		t.Fatal(err)
	}
	got, err := st.SearchConclusionCandidates("w", "obs", "obd", "炉心の温度", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.Contains(got[0].Content, "炉心") {
		t.Fatalf("got %d %+v", len(got), got)
	}
}
