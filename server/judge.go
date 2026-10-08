// judge.go — 判断クライアント（clef / SystemOne 互換シム）
//
// 2系統のエンドポイントを吸収する:
//
//	kind=clef   : Cloudflare Workers AI。リクエスト {"model","state","questions"}、
//	              応答は {"success":true,"result":{"answers":{...},"model":...}} の封筒付き。
//	kind=sysone : local judgment shim — plain {"answers":{...}} response, no auth (label is historical).
//	               Example: a small local model served at http://127.0.0.1:8799/v1/systemone
//
// 環境変数（秘密は「変数名」だけを指定する流儀）:
//
//	HONCHO_LITE_JUDGE_URL          必須。例: https://api.cloudflare.com/client/v4/accounts/<acct>/ai/run/@cf/cloudflare/clef-flash
//	HONCHO_LITE_JUDGE_KIND         既定 "clef"（"sysone" も可）
//	HONCHO_LITE_JUDGE_MODEL        既定 "clef-flash"（kind=clef のときのみ送信）
//	HONCHO_LITE_JUDGE_KEY_ENV      既定 "CLOUDFLARE_API_TOKEN"（kind=clef）。空文字で認証なし
//	HONCHO_LITE_JUDGE_TIMEOUT_MS   既定 20000
//	HONCHO_LITE_JUDGE_FALLBACK_URL 障害時のフォールバック先（sysone として扱う。例 http://127.0.0.1:8799/v1/systemone）
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---- config ----

type JudgeConfig struct {
	Kind     string // "clef" | "sysone"
	URL      string
	Model    string // clef のみ送信。空なら送らない
	KeyEnv   string // 環境変数の「名前」。値はここに保持しない
	Timeout  time.Duration
	Fallback *JudgeConfig
}

func JudgeConfigFromEnv() (*JudgeConfig, error) {
	url := os.Getenv("HONCHO_LITE_JUDGE_URL")
	if url == "" {
		return nil, fmt.Errorf("HONCHO_LITE_JUDGE_URL is not set")
	}
	kind := os.Getenv("HONCHO_LITE_JUDGE_KIND")
	if kind == "" {
		kind = "clef"
	}
	timeout := 20 * time.Second
	if ms := os.Getenv("HONCHO_LITE_JUDGE_TIMEOUT_MS"); ms != "" {
		if n, err := strconv.Atoi(ms); err == nil && n > 0 {
			timeout = time.Duration(n) * time.Millisecond
		}
	}
	model := os.Getenv("HONCHO_LITE_JUDGE_MODEL")
	if model == "" && kind == "clef" {
		model = "clef-flash"
	}
	keyEnv, ok := os.LookupEnv("HONCHO_LITE_JUDGE_KEY_ENV")
	if !ok {
		if kind == "clef" {
			keyEnv = "CLOUDFLARE_API_TOKEN"
		} else {
			keyEnv = ""
		}
	}
	cfg := &JudgeConfig{Kind: kind, URL: url, Model: model, KeyEnv: keyEnv, Timeout: timeout}
	if fb := os.Getenv("HONCHO_LITE_JUDGE_FALLBACK_URL"); fb != "" {
		fbT := timeout
		if ms := os.Getenv("HONCHO_LITE_JUDGE_FALLBACK_TIMEOUT_MS"); ms != "" {
			if n, err := strconv.Atoi(ms); err == nil && n > 0 {
				fbT = time.Duration(n) * time.Millisecond
			}
		}
		cfg.Fallback = &JudgeConfig{Kind: "sysone", URL: fb, Timeout: fbT}
	}
	return cfg, nil
}

// ---- client ----

type JudgeClient struct {
	cfg JudgeConfig
	key string
	hc  *http.Client
	fb  *JudgeClient
}

func NewJudgeClient(cfg JudgeConfig) *JudgeClient {
	var key string
	if cfg.KeyEnv != "" {
		key = os.Getenv(cfg.KeyEnv)
	}
	c := &JudgeClient{cfg: cfg, key: key, hc: &http.Client{Timeout: cfg.Timeout}}
	if cfg.Fallback != nil {
		c.fb = NewJudgeClient(*cfg.Fallback)
	}
	return c
}

// HasKey — キー環境変数が空でないか（doctor 表示用。値は出さない）
func (c *JudgeClient) HasKey() bool { return c.key != "" }

// ---- question builders ----

func NoulQ(instructions string) map[string]any {
	return map[string]any{"type": "noul", "instructions": instructions}
}

func ChoiceQ(instructions string, criteria map[string]string) map[string]any {
	return map[string]any{"type": "choice", "instructions": instructions, "criteria": criteria}
}

// ---- results ----

type Judgment struct {
	Noul          *float64           `json:"noul,omitempty"`
	Choice        string             `json:"choice,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Confidence    *float64           `json:"confidence,omitempty"`
	Score         *float64           `json:"score,omitempty"`
	Raw           map[string]any     `json:"-"`
}

// askChunkSize — judge 1コールあたりの質問数上限。
// clef (CF Workers AI) は 64 問超で 422 (validation) を返すため、余裕を見て 48 で分割する。
const askChunkSize = 48

// Ask — state + questions を送り、1問ずつの Judgment を返す。
// 48 問超は自動で複数コールに分割してマージする（チャンク内は primary→fallback）。
// meta は実際に応えたモデル名（フォールバック時は " (fallback)"、分割時は "chunked(...)"）。
func (c *JudgeClient) Ask(ctx context.Context, state string, questions map[string]map[string]any) (map[string]Judgment, string, error) {
	if len(questions) <= askChunkSize {
		return c.askWithFallback(ctx, state, questions)
	}
	keys := make([]string, 0, len(questions))
	for k := range questions {
		keys = append(keys, k)
	}
	sort.Strings(keys) // 順序を安定化（ログ・テストの再現性）
	merged := make(map[string]Judgment, len(questions))
	metas := map[string]bool{}
	nChunks := (len(keys) + askChunkSize - 1) / askChunkSize
	for start := 0; start < len(keys); start += askChunkSize {
		end := start + askChunkSize
		if end > len(keys) {
			end = len(keys)
		}
		chunk := make(map[string]map[string]any, end-start)
		for _, k := range keys[start:end] {
			chunk[k] = questions[k]
		}
		res, meta, err := c.askWithFallback(ctx, state, chunk)
		if err != nil {
			return nil, "", fmt.Errorf("judge chunk [%d:%d]: %w", start, end, err)
		}
		for k, v := range res {
			merged[k] = v
		}
		metas[meta] = true
	}
	metaList := make([]string, 0, len(metas))
	for m := range metas {
		metaList = append(metaList, m)
	}
	sort.Strings(metaList)
	return merged, fmt.Sprintf("chunked(%s x%d)", strings.Join(metaList, ","), nChunks), nil
}

// askWithFallback — 単一コール + primary→fallback。
func (c *JudgeClient) askWithFallback(ctx context.Context, state string, questions map[string]map[string]any) (map[string]Judgment, string, error) {
	res, meta, err := c.askOne(ctx, state, questions)
	if err == nil {
		return res, meta, nil
	}
	if c.fb != nil {
		log.Printf("judge: primary(%s %s) failed: %v — fallback -> %s", c.cfg.Kind, jMaskURL(c.cfg.URL), err, jMaskURL(c.fb.cfg.URL))
		res2, meta2, err2 := c.fb.askOne(ctx, state, questions)
		if err2 == nil {
			return res2, meta2 + " (fallback)", nil
		}
		return nil, "", fmt.Errorf("judge: primary: %v / fallback: %w", err, err2)
	}
	return nil, "", err
}

func (c *JudgeClient) askOne(ctx context.Context, state string, questions map[string]map[string]any) (map[string]Judgment, string, error) {
	body := map[string]any{"state": state, "questions": questions}
	if c.cfg.Model != "" {
		body["model"] = c.cfg.Model
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL, bytes.NewReader(buf))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "honchol/"+version)
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, "", fmt.Errorf("read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("http %d: %.300s", resp.StatusCode, string(raw))
	}
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, "", fmt.Errorf("bad json: %.200s", string(raw))
	}
	answersRaw, modelName, err := extractAnswers(top)
	if err != nil {
		return nil, "", err
	}
	out := make(map[string]Judgment, len(answersRaw))
	for qid, a := range answersRaw {
		out[qid] = parseJudgment(a)
	}
	if modelName == "" {
		modelName = c.cfg.Model
	}
	if modelName == "" {
		modelName = c.cfg.Kind
	}
	return out, modelName, nil
}

// extractAnswers — clef 封筒（result.answers）と素の sysone（answers）の両対応
func extractAnswers(top map[string]any) (map[string]map[string]any, string, error) {
	model := func(m map[string]any) string {
		if s, ok := m["model"].(string); ok {
			return s
		}
		return ""
	}
	if r, ok := top["result"].(map[string]any); ok {
		if a, ok := r["answers"].(map[string]any); ok {
			return toAnswerMap(a), model(r), nil
		}
	}
	if a, ok := top["answers"].(map[string]any); ok {
		return toAnswerMap(a), model(top), nil
	}
	if errs, ok := top["errors"]; ok {
		return nil, "", fmt.Errorf("judge errors: %.300v", errs)
	}
	if e, ok := top["error"]; ok {
		return nil, "", fmt.Errorf("judge error: %.300v", e)
	}
	j, _ := json.Marshal(top)
	return nil, "", fmt.Errorf("no answers in judge response: %.300s", string(j))
}

func toAnswerMap(a map[string]any) map[string]map[string]any {
	out := make(map[string]map[string]any, len(a))
	for k, v := range a {
		if m, ok := v.(map[string]any); ok {
			out[k] = m
		} else {
			out[k] = map[string]any{"raw": v}
		}
	}
	return out
}

func parseJudgment(m map[string]any) Judgment {
	j := Judgment{Raw: m}
	if v, ok := m["noul"]; ok {
		f := asFloat(v)
		j.Noul = &f
	}
	if v, ok := m["choice"]; ok {
		if s, ok := v.(string); ok {
			j.Choice = s
		}
	}
	if v, ok := m["probabilities"].(map[string]any); ok {
		j.Probabilities = make(map[string]float64, len(v))
		for k, p := range v {
			j.Probabilities[k] = asFloat(p)
		}
	}
	if v, ok := m["confidence"]; ok {
		f := asFloat(v)
		j.Confidence = &f
	}
	if v, ok := m["score"]; ok {
		f := asFloat(v)
		j.Score = &f
	}
	return j
}

func asFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case json.Number:
		f, _ := x.Float64()
		return f
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	}
	return 0
}

// jMaskURL — ログ用にアカウントID等を伏せる（/accounts/<id>/ → /accounts/…/）
var reAcct = regexp.MustCompile(`(/accounts/)([^/]+)(/)`)

func jMaskURL(u string) string {
	return reAcct.ReplaceAllString(u, "${1}…${3}")
}

// ---- v0.3d: mbja judge (kind=mbja) ----

// AskMbja — 検索judge: mbja（ModernBERT-Ja+GLiClass・サイドカー）に query×texts の関連度を問い合わせる。
// HTTP契約: POST {"query": "...", "texts": ["..."]} → {"scores": [float...], "model": "..."}
// scores[i] = P(relevant | query, texts[i])。件数不一致はエラー。
func (c *JudgeClient) AskMbja(ctx context.Context, query string, texts []string) ([]float64, string, error) {
	// 古典経路と同一基準の切詰め（ペイロード肥大防止・セキュリティレビュー対応）
	ts := make([]string, len(texts))
	for i, t := range texts {
		ts[i] = truncRunes(cleanForPrompt(t), 500)
	}
	body := map[string]any{"query": truncRunes(cleanForPrompt(query), 300), "texts": ts}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL, bytes.NewReader(buf))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "honchol/"+version)
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, "", fmt.Errorf("read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("http %d: %.300s", resp.StatusCode, string(raw))
	}
	var out struct {
		Scores []float64 `json:"scores"`
		Model  string    `json:"model"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, "", fmt.Errorf("bad json: %.200s", string(raw))
	}
	if len(out.Scores) != len(texts) {
		return nil, "", fmt.Errorf("mbja: scores %d != texts %d", len(out.Scores), len(texts))
	}
	for i, s := range out.Scores {
		if s != s || s > 1e9 || s < -1e9 { // NaN / ±Inf / 異常値（math 非依存の検査）
			return nil, "", fmt.Errorf("mbja: non-finite score[%d]", i)
		}
	}
	name := out.Model
	if name == "" {
		name = "mbja"
	}
	return out.Scores, name, nil
}

// envFloat — 環境変数の float（未設定・不正は既定値）。
func envFloat(name string, def float64) float64 {
	if s := os.Getenv(name); s != "" {
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f
		}
	}
	return def
}

// runJudgeCheck — doctor -judge 用の疎通確認（P2 judge クライアントの実地検証）
func runJudgeCheck() {
	cfg, err := JudgeConfigFromEnv()
	if err != nil {
		fmt.Printf("judge: not configured: %v\n", err)
		os.Exit(1)
	}
	c := NewJudgeClient(*cfg)
	keyState := "missing"
	if c.HasKey() {
		keyState = "set"
	}
	fmt.Printf("judge: kind=%s model=%s key_env=%s(%s) url=%s\n", cfg.Kind, cfg.Model, cfg.KeyEnv, keyState, jMaskURL(cfg.URL))
	if cfg.Fallback != nil {
		fmt.Printf("judge: fallback -> %s\n", jMaskURL(cfg.Fallback.URL))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	t0 := time.Now()
	res, meta, err := c.Ask(ctx, "The sky is blue on a clear day. A ball is round.",
		map[string]map[string]any{
			"sky":   NoulQ("The sky is blue on a clear day."),
			"shape": ChoiceQ("What shape is a ball?", map[string]string{"round": "having a circular shape", "square": "having four equal sides"}),
		})
	dt := time.Since(t0)
	if err != nil {
		fmt.Printf("judge: FAIL %dms: %v\n", dt.Milliseconds(), err)
		os.Exit(1)
	}
	if n := res["sky"].Noul; n != nil {
		fmt.Printf("judge: ok %dms model=%s sky.noul=%.3f\n", dt.Milliseconds(), meta, *n)
	} else {
		fmt.Printf("judge: ok %dms model=%s (noul not parsed)\n", dt.Milliseconds(), meta)
	}
	if ch := res["shape"]; ch.Choice != "" {
		fmt.Printf("judge: shape.choice=%s\n", ch.Choice)
	}
}
