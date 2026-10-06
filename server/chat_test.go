package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestChatEndpoint — hChat の 200（LLM合成）/ 501（stream）/ 503（LLM未設定）を検証。
func TestChatEndpoint(t *testing.T) {
	st := testStore(t)
	ws := "w"
	if err := st.SetCard(ws, "agent", "alice", []string{"sitneは較正研究をしている"}); err != nil {
		t.Fatal(err)
	}
	llmSrv := fakeLLMServer(t)
	defer llmSrv.Close()
	t.Setenv("FAKE_LLM_KEY", "x")
	srv := &apiServer{st: st, llm: NewLLMClient(LLMConfig{BaseURL: llmSrv.URL, Model: "fake-model", KeyEnv: "FAKE_LLM_KEY", Timeout: 5 * time.Second})}
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	post := func(base, body string) (int, string) {
		resp, err := http.Post(base+"/v3/workspaces/"+ws+"/peers/agent/chat", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	code, body := post(ts.URL, `{"query":"較正の研究について知っていることを教えて","stream":false,"target":"alice"}`)
	if code != http.StatusOK || !strings.Contains(body, "較正研究") {
		t.Fatalf("chat = %d %s", code, body)
	}

	code, _ = post(ts.URL, `{"query":"x","stream":true,"target":"alice"}`)
	if code != http.StatusNotImplemented {
		t.Fatalf("stream chat = %d", code)
	}

	srv2 := &apiServer{st: st}
	ts2 := httptest.NewServer(srv2.routes())
	defer ts2.Close()
	code, _ = post(ts2.URL, `{"query":"較正について","stream":false,"target":"alice"}`)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("no-llm chat = %d", code)
	}
}
