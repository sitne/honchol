package main

// v0.3c: 検索クエリの翻訳前段（JA→EN）。
// HONCHO_LITE_TRANSLATE_PROVIDER=none|local。local = サイドカー /translate（LFM2-350M-ENJP-MT）。
// 使い方: RRF 融合経路の embedding アームに翻訳後クエリを渡す（FTS は原文 literal を維持）。
// キャッシュ: translate_cache（src_hash = SHA-256）。fail-soft: 失敗時は原文で続行。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode"
)

type translateConfig struct {
	kind    string // none | local
	url     string
	timeout time.Duration
	maxLen  int // これより長いクエリは翻訳スキップ（runes）
}

type translateProvider struct {
	cfg translateConfig
	hc  *http.Client
}

func newTranslateProviderFromEnv() *translateProvider {
	kind := strings.ToLower(envOr("HONCHO_LITE_TRANSLATE_PROVIDER", "none"))
	if kind == "" {
		kind = "none"
	}
	t := &translateProvider{
		cfg: translateConfig{
			kind:    kind,
			url:     envOr("HONCHO_LITE_TRANSLATE_URL", "http://127.0.0.1:8793/translate"),
			timeout: time.Duration(envInt("HONCHO_LITE_TRANSLATE_TIMEOUT_MS", 2500)) * time.Millisecond,
			maxLen:  envInt("HONCHO_LITE_TRANSLATE_MAXLEN", 400),
		},
	}
	t.hc = &http.Client{Timeout: t.cfg.timeout + 2*time.Second} // ctx との二重の安全弁
	return t
}

// Enabled — 翻訳段が有効か（none なら false）。
func (t *translateProvider) Enabled() bool { return t != nil && t.cfg.kind != "none" }

// Kind — none | local。
func (t *translateProvider) Kind() string {
	if t == nil {
		return "none"
	}
	return t.cfg.kind
}

// hasCJK — 漢字・ひらがな・カタカナを含むか（翻訳対象判定）。
func hasCJK(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) {
			return true
		}
	}
	return false
}

func sha256hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func shorten(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func hasASCIIAlpha(s string) bool {
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			return true
		}
	}
	return false
}

// cleanTranslation — 出力の軽い整形（引用符・前後空白）。
func cleanTranslation(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, `"'“”‘’`)
	return strings.TrimSpace(s)
}

// translate — プロバイダ別の翻訳呼び出し。
func (t *translateProvider) translate(ctx context.Context, text string) (string, error) {
	switch t.cfg.kind {
	case "local":
		return t.translateLocal(ctx, text)
	default:
		return "", fmt.Errorf("translate provider %q は未対応", t.cfg.kind)
	}
}

// translateLocal — サイドカー（LFM2 ONNX）に1件翻訳を依頼。
func (t *translateProvider) translateLocal(ctx context.Context, text string) (string, error) {
	body, _ := json.Marshal(map[string]any{"texts": []string{text}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.cfg.url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("sidecar translate http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out struct {
		Translations []string `json:"translations"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if len(out.Translations) == 0 {
		return "", fmt.Errorf("空の translations")
	}
	return out.Translations[0], nil
}

// translateQuery — 検索クエリの JA→EN 翻訳（キャッシュ + fail-soft）。
// ok=false は「翻訳なし＝原文で続行」を意味する。
func (a *apiServer) translateQuery(ctx context.Context, q string) (string, bool) {
	tp := a.tp
	if tp == nil || !tp.Enabled() || a.st == nil {
		return "", false
	}
	q = strings.TrimSpace(q)
	if q == "" || !hasCJK(q) {
		return "", false // 既に英語などはスキップ
	}
	if len([]rune(q)) > tp.cfg.maxLen {
		return "", false
	}
	key := sha256hex(q)
	if v, ok, err := a.st.GetTranslateCache(key); err == nil && ok && v != "" {
		return v, true // キャッシュヒット（ログは出さない）
	}
	tctx, cancel := context.WithTimeout(ctx, tp.cfg.timeout)
	defer cancel()
	out, err := tp.translate(tctx, q)
	if err != nil {
		log.Printf("translate: failed (%v) — using original", err)
		return "", false
	}
	out = cleanTranslation(out)
	if out == "" || !hasASCIIAlpha(out) {
		log.Printf("translate: suspicious output %q — using original", shorten(out, 60))
		return "", false
	}
	if err := a.st.PutTranslateCache(key, q, out, tp.cfg.kind); err != nil {
		log.Printf("translate: cache put: %v", err)
	}
	log.Printf("translate: %q -> %q", shorten(q, 80), shorten(out, 80))
	return out, true
}
