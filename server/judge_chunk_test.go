package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestJudgeAskChunking — 48件超の質問が複数コールに分割され、全問の判定がマージされる。
// clef の「64問超は422」を踏まえた分割の回帰テスト。
func TestJudgeAskChunking(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		var req struct {
			Questions map[string]map[string]any `json:"questions"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &req)
		if len(req.Questions) > 64 {
			w.WriteHeader(422)
			_, _ = io.WriteString(w, `{"errors":[{"message":"too many questions"}]}`)
			return
		}
		answers := map[string]any{}
		for qid := range req.Questions {
			answers[qid] = map[string]any{"choice": "関連", "confidence": 0.9}
		}
		resp, _ := json.Marshal(map[string]any{"model": "fake", "answers": answers})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(resp)
	}))
	defer srv.Close()

	jc := NewJudgeClient(JudgeConfig{Kind: "sysone", URL: srv.URL, Timeout: 5 * time.Second})
	qs := map[string]map[string]any{}
	for i := 0; i < 100; i++ {
		qs[fmt.Sprintf("q%03d", i)] = map[string]any{"instructions": "x"}
	}
	res, meta, err := jc.Ask(context.Background(), "state", qs)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 100 {
		t.Fatalf("merged %d judgments, want 100", len(res))
	}
	if n := atomic.LoadInt32(&calls); n != 3 {
		t.Fatalf("calls = %d, want 3", n)
	}
	if !strings.Contains(meta, "chunked") {
		t.Fatalf("meta = %q", meta)
	}
}
