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

func TestClefEnvelopeAndAuth(t *testing.T) {
	var gotAuth, gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		gotModel, _ = body["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"success":true,"result":{"answers":{"q1":{"noul":0.9},"q2":{"choice":"a","probabilities":{"a":0.8,"b":0.2},"confidence":0.8}},"model":"clef-flash"},"errors":[]}`)
	}))
	defer srv.Close()

	t.Setenv("TEST_JUDGE_KEY_CLEF", "sekret")
	c := NewJudgeClient(JudgeConfig{Kind: "clef", URL: srv.URL, Model: "clef-flash", KeyEnv: "TEST_JUDGE_KEY_CLEF", Timeout: 5 * time.Second})
	res, meta, err := c.Ask(context.Background(), "st", map[string]map[string]any{
		"q1": NoulQ("x"), "q2": ChoiceQ("y", map[string]string{"a": "A", "b": "B"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer sekret" {
		t.Fatalf("auth header = %q", gotAuth)
	}
	if gotModel != "clef-flash" {
		t.Fatalf("model field = %q", gotModel)
	}
	if meta != "clef-flash" {
		t.Fatalf("meta = %q", meta)
	}
	if res["q1"].Noul == nil || *res["q1"].Noul != 0.9 {
		t.Fatalf("q1 = %+v", res["q1"])
	}
	if res["q2"].Choice != "a" || res["q2"].Probabilities["b"] != 0.2 {
		t.Fatalf("q2 = %+v", res["q2"])
	}
}

func TestSysoneEnvelopeNoAuth(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"model":"shim-x","answers":{"q":{"noul":0.5}},"usage":{}}`)
	}))
	defer srv.Close()
	c := NewJudgeClient(JudgeConfig{Kind: "sysone", URL: srv.URL, Timeout: 5 * time.Second})
	res, meta, err := c.Ask(context.Background(), "st", map[string]map[string]any{"q": NoulQ("x")})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "" {
		t.Fatalf("auth should be empty, got %q", gotAuth)
	}
	if meta != "shim-x" {
		t.Fatalf("meta = %q", meta)
	}
	if res["q"].Noul == nil || *res["q"].Noul != 0.5 {
		t.Fatalf("q = %+v", res["q"])
	}
}

func TestFallbackOnPrimaryError(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = io.WriteString(w, `{"errors":[{"message":"boom"}]}`)
	}))
	defer primary.Close()
	fb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"model":"shim-fb","answers":{"q":{"noul":0.7}}}`)
	}))
	defer fb.Close()

	cfg := JudgeConfig{Kind: "clef", URL: primary.URL, Model: "clef-flash", Timeout: 5 * time.Second,
		Fallback: &JudgeConfig{Kind: "sysone", URL: fb.URL, Timeout: 5 * time.Second}}
	c := NewJudgeClient(cfg)
	res, meta, err := c.Ask(context.Background(), "st", map[string]map[string]any{"q": NoulQ("x")})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(meta, "(fallback)") {
		t.Fatalf("meta = %q", meta)
	}
	if res["q"].Noul == nil || *res["q"].Noul != 0.7 {
		t.Fatalf("q = %+v", res["q"])
	}
}

func TestNoFallbackReturnsError(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = io.WriteString(w, `oops`)
	}))
	defer primary.Close()
	c := NewJudgeClient(JudgeConfig{Kind: "clef", URL: primary.URL, Timeout: 5 * time.Second})
	_, _, err := c.Ask(context.Background(), "st", map[string]map[string]any{"q": NoulQ("x")})
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("err = %v", err)
	}
}

func TestExtractAnswersErrorShapes(t *testing.T) {
	if _, _, err := extractAnswers(map[string]any{"error": "bad"}); err == nil {
		t.Fatal("want error for {error:...}")
	}
	if _, _, err := extractAnswers(map[string]any{"errors": []any{map[string]any{"message": "x"}}}); err == nil {
		t.Fatal("want error for {errors:[...]}")
	}
}

func TestJudgeConfigFromEnvBasics(t *testing.T) {
	t.Setenv("HONCHO_LITE_JUDGE_URL", "https://example.invalid/x")
	t.Setenv("HONCHO_LITE_JUDGE_FALLBACK_URL", "http://127.0.0.1:8799/v1/systemone")
	cfg, err := JudgeConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Timeout != 20*time.Second {
		t.Fatalf("timeout = %v", cfg.Timeout)
	}
	if cfg.Fallback == nil || cfg.Fallback.Kind != "sysone" {
		t.Fatalf("fallback = %+v", cfg.Fallback)
	}
}

func TestJMaskURL(t *testing.T) {
	in := "https://api.cloudflare.com/client/v4/accounts/abc123def/ai/run/@cf/cloudflare/clef-flash"
	want := "https://api.cloudflare.com/client/v4/accounts/…/ai/run/@cf/cloudflare/clef-flash"
	if got := jMaskURL(in); got != want {
		t.Fatalf("got %q", got)
	}
}
