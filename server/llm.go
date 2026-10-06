// llm.go — 抽出・要約・chat 用のクラウドLLMクライアント（OpenAI互換 chat/completions）
//
// 環境変数（秘密は「変数名」だけを指定する流儀）:
//
//	HONCHO_LITE_LLM_BASE_URL   既定 https://opencode.ai/zen/go/v1
//	HONCHO_LITE_LLM_MODEL      既定 deepseek-v4.1-flash
//	HONCHO_LITE_LLM_KEY_ENV    既定 OPENCODE_GO_API_KEY。空文字で認証なし
//	HONCHO_LITE_LLM_TIMEOUT_MS 既定 90000
//
// 用途: derive の抽出/要約、検索クエリ展開（P2 続）、chat/dialectic（P2c）。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type LLMConfig struct {
	BaseURL string
	Model   string
	KeyEnv  string // 環境変数の「名前」。値はここに保持しない
	Timeout time.Duration
	Session string // x-opencode-session ヘッダ値（opencode-go が要求する安定セッションID）
}

func LLMConfigFromEnv() LLMConfig {
	base := strings.TrimRight(os.Getenv("HONCHO_LITE_LLM_BASE_URL"), "/")
	if base == "" {
		base = "https://opencode.ai/zen/go/v1"
	}
	model := os.Getenv("HONCHO_LITE_LLM_MODEL")
	if model == "" {
		model = "deepseek-v4.1-flash"
	}
	keyEnv, ok := os.LookupEnv("HONCHO_LITE_LLM_KEY_ENV")
	if !ok {
		keyEnv = "OPENCODE_GO_API_KEY"
	}
	timeout := 90 * time.Second
	if ms := os.Getenv("HONCHO_LITE_LLM_TIMEOUT_MS"); ms != "" {
		if n, err := strconv.Atoi(ms); err == nil && n > 0 {
			timeout = time.Duration(n) * time.Millisecond
		}
	}
	session := os.Getenv("HONCHO_LITE_LLM_SESSION")
	if session == "" {
		session = "honcho-lite"
	}
	return LLMConfig{BaseURL: base, Model: model, KeyEnv: keyEnv, Timeout: timeout, Session: session}
}

type LLMClient struct {
	cfg LLMConfig
	key string
	hc  *http.Client
}

func NewLLMClient(cfg LLMConfig) *LLMClient {
	var key string
	if cfg.KeyEnv != "" {
		key = os.Getenv(cfg.KeyEnv)
	}
	return &LLMClient{cfg: cfg, key: key, hc: &http.Client{Timeout: cfg.Timeout}}
}

// HasKey — キー環境変数が空でないか（値は出さない）
func (c *LLMClient) HasKey() bool { return c.key != "" }

// Chat — system+user の1往復。content 文字列を返す。
func (c *LLMClient) Chat(ctx context.Context, system, user string, temperature float64) (string, error) {
	body := map[string]any{
		"model": c.cfg.Model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		"temperature": temperature,
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/chat/completions", bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "honchol/"+version)
	if c.cfg.Session != "" {
		req.Header.Set("x-opencode-session", c.cfg.Session)
	}
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("llm http: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", fmt.Errorf("llm read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("llm http %d: %.300s", resp.StatusCode, string(raw))
	}
	var top struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error any `json:"error"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return "", fmt.Errorf("llm bad json: %.200s", string(raw))
	}
	if len(top.Choices) == 0 {
		return "", fmt.Errorf("llm no choices: %.200s", string(raw))
	}
	return top.Choices[0].Message.Content, nil
}

// ChatJSON — Chatして最初の JSON 値（object/array）を寛容に取り出す。
// コードフェンスや前後の地の文は無視する。
func (c *LLMClient) ChatJSON(ctx context.Context, system, user string, temperature float64) (any, error) {
	s, err := c.Chat(ctx, system, user, temperature)
	if err != nil {
		return nil, err
	}
	return extractJSONValue(s)
}

func extractJSONValue(s string) (any, error) {
	i := strings.IndexAny(s, "{[")
	if i < 0 {
		return nil, fmt.Errorf("no json value in response: %.200s", s)
	}
	dec := json.NewDecoder(strings.NewReader(s[i:]))
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("json decode: %v (%.200s)", err, s[i:])
	}
	return v, nil
}
