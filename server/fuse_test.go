package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRRFFuse(t *testing.T) {
	got := rrfFuse([][]string{{"a", "b", "c"}, {"b", "a"}}, 60, 3)
	want := []string{"a", "b", "c"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("got %v want %v", got, want)
	}
	got = rrfFuse([][]string{{"x", "y"}, nil}, 60, 5)
	if len(got) != 2 || got[0] != "x" || got[1] != "y" {
		t.Fatalf("single arm: %v", got)
	}
	got = rrfFuse([][]string{{"x", "y", "z"}}, 60, 2)
	if len(got) != 2 {
		t.Fatalf("topN: %v", got)
	}
}

func fakeJudgeAllRelated(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Questions map[string]struct{} `json:"questions"`
		}
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &body); err != nil {
			w.WriteHeader(400)
			return
		}
		answers := map[string]any{}
		for qid := range body.Questions {
			answers[qid] = map[string]any{
				"choice":        "関連",
				"probabilities": map[string]float64{"関連": 0.9},
				"confidence":    0.9,
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "fake", "answers": answers})
	}))
}

func TestRankedSearchConclusionsFused(t *testing.T) {
	st := testStore(t)
	rA, err := st.CreateConclusion("w", "obs", "obd", "calibration routine deployed for the sensor", "explicit", nil)
	if err != nil {
		t.Fatal(err)
	}
	rB, err := st.CreateConclusion("w", "obs", "obd", "totally unrelated note about pasta", "explicit", nil)
	if err != nil {
		t.Fatal(err)
	}
	rD, err := st.CreateConclusion("w", "obs", "obd", "較正の手順メモ", "explicit", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertVectors("conclusion", []VectorRow{
		{ID: rA.ID, Dim: 4, Vec: []float32{1, 0, 0, 0}},
		{ID: rB.ID, Dim: 4, Vec: []float32{0, 0, 0, 1}},
		{ID: rD.ID, Dim: 4, Vec: []float32{0.7, 0.7, 0, 0}},
	}); err != nil {
		t.Fatal(err)
	}

	emb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Texts []string `json:"texts"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &req)
		vecs := make([][]float32, len(req.Texts))
		for i := range req.Texts {
			vecs[i] = []float32{1, 0, 0, 0}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"vectors": vecs, "dim": 4, "model": "fake"})
	}))
	defer emb.Close()
	jsrv := fakeJudgeAllRelated(t)
	defer jsrv.Close()

	a := &apiServer{
		st:   st,
		jc:   NewJudgeClient(JudgeConfig{Kind: "sysone", URL: jsrv.URL, Timeout: 5 * time.Second}),
		ep:   NewEmbeddingProvider(EmbeddingConfig{Kind: "local", URL: emb.URL, Dim: 4, Timeout: 5 * time.Second}),
		fuse: true,
	}
	out := a.rankedSearchConclusions(context.Background(), "w", "obs", "obd", "較正", 10)
	if len(out) == 0 {
		t.Fatal("no rows")
	}
	found := map[string]bool{}
	for _, r := range out {
		found[r.ID] = true
	}
	if !found[rA.ID] || !found[rD.ID] {
		t.Fatalf("expected both arms present: got %d rows (A=%v D=%v)", len(out), found[rA.ID], found[rD.ID])
	}
	if out[0].ID != rD.ID {
		t.Fatalf("expected 較正 (fts+emb) first, got %q", out[0].Content)
	}

	// emb アーム障害時は FTS 単独で応答（fail-soft）
	aBad := &apiServer{
		st:   st,
		jc:   a.jc,
		ep:   NewEmbeddingProvider(EmbeddingConfig{Kind: "local", URL: "http://127.0.0.1:1/embed", Dim: 4, Timeout: 500 * time.Millisecond}),
		fuse: true,
	}
	out2 := aBad.rankedSearchConclusions(context.Background(), "w", "obs", "obd", "較正", 10)
	if len(out2) == 0 {
		t.Fatal("fail-soft: expected fts-only rows")
	}
}
