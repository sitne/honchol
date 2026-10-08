package main

// 検索v2 / v0.3a: EmbeddingProvider（none|local|api）とベクトル backfill ヘルパー。
//
// - local: サイドカー（sidecar/model_sidecar.py・bekko ONNX）へ HTTP /embed
// - api  : OpenAI 互換 /embeddings へ HTTP
// - none : 無効（既定。完全ロールバック点）
//
// 設定は HONCHO_LITE_EMBED_*（deploy/honchol.env で指定）。

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// ---------- ベクトル BLOB コーデック（float32 LE・vectors テーブル保存用） ----------

func vecToBlob(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(f))
	}
	return b
}

func blobToVec(b []byte) ([]float32, error) {
	if len(b)%4 != 0 {
		return nil, fmt.Errorf("vec blob length %d is not a multiple of 4", len(b))
	}
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v, nil
}

// normalizeVec — L2 正規化（その場で書き換えて返す）。ゼロベクトルはそのまま。
func normalizeVec(v []float32) []float32 {
	var s float64
	for _, f := range v {
		s += float64(f) * float64(f)
	}
	if s == 0 {
		return v
	}
	inv := float32(1.0 / math.Sqrt(s))
	for i := range v {
		v[i] *= inv
	}
	return v
}

// ---------- 設定 ----------

type EmbeddingConfig struct {
	Kind      string // none | local | api
	URL       string // local: サイドカー /embed
	Dim       int    // 期待次元（0 = 無検査）
	APIBase   string // api: OpenAI 互換ベースURL
	APIModel  string
	APIKeyEnv string
	Timeout   time.Duration
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envIntOr(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// EmbeddingConfigFromEnv — HONCHO_LITE_EMBED_* から設定を読む（既定 none）。
func EmbeddingConfigFromEnv() EmbeddingConfig {
	return EmbeddingConfig{
		Kind:      strings.ToLower(envOr("HONCHO_LITE_EMBED_PROVIDER", "none")),
		URL:       envOr("HONCHO_LITE_EMBED_URL", "http://127.0.0.1:8793/embed"),
		Dim:       envIntOr("HONCHO_LITE_EMBED_DIM", 384),
		APIBase:   strings.TrimRight(envOr("HONCHO_LITE_EMBED_API_BASE", ""), "/"),
		APIModel:  envOr("HONCHO_LITE_EMBED_API_MODEL", "text-embedding-3-small"),
		APIKeyEnv: envOr("HONCHO_LITE_EMBED_API_KEY_ENV", "HONCHO_LITE_EMBED_API_KEY"),
		Timeout:   time.Duration(envIntOr("HONCHO_LITE_EMBED_TIMEOUT_MS", 5000)) * time.Millisecond,
	}
}

// ---------- provider ----------

type EmbeddingProvider struct {
	cfg    EmbeddingConfig
	hc     *http.Client
	apiKey string
}

func NewEmbeddingProvider(cfg EmbeddingConfig) *EmbeddingProvider {
	p := &EmbeddingProvider{cfg: cfg, hc: &http.Client{Timeout: cfg.Timeout}}
	if cfg.Kind == "api" && cfg.APIKeyEnv != "" {
		p.apiKey = os.Getenv(cfg.APIKeyEnv)
	}
	return p
}

func (p *EmbeddingProvider) Enabled() bool {
	return p != nil && p.cfg.Kind != "" && p.cfg.Kind != "none"
}
func (p *EmbeddingProvider) Kind() string { return p.cfg.Kind }
func (p *EmbeddingProvider) Dim() int     { return p.cfg.Dim }

// Embed — texts を埋め込み（L2 正規化済み・入力順）。
func (p *EmbeddingProvider) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if !p.Enabled() {
		return nil, fmt.Errorf("embedding provider is disabled")
	}
	if len(texts) == 0 {
		return nil, nil
	}
	switch p.cfg.Kind {
	case "local":
		return p.embedLocal(ctx, texts)
	case "api":
		return p.embedAPI(ctx, texts)
	default:
		return nil, fmt.Errorf("unknown embedding provider kind %q", p.cfg.Kind)
	}
}

type embedLocalReq struct {
	Texts []string `json:"texts"`
}

type embedLocalResp struct {
	Vectors [][]float32 `json:"vectors"`
	Dim     int         `json:"dim"`
	Model   string      `json:"model"`
}

func (p *EmbeddingProvider) embedLocal(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(embedLocalReq{Texts: texts})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed local: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embed local: http %d: %.200s", resp.StatusCode, raw)
	}
	var out embedLocalResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("embed local: decode: %w", err)
	}
	return p.finalize(texts, out.Vectors)
}

type embedAPIResp struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

func (p *EmbeddingProvider) embedAPI(ctx context.Context, texts []string) ([][]float32, error) {
	if p.cfg.APIBase == "" {
		return nil, fmt.Errorf("embed api: HONCHO_LITE_EMBED_API_BASE is empty")
	}
	body, err := json.Marshal(map[string]any{"model": p.cfg.APIModel, "input": texts})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.APIBase+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	resp, err := p.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed api: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embed api: http %d: %.200s", resp.StatusCode, raw)
	}
	var out embedAPIResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("embed api: decode: %w", err)
	}
	vecs := make([][]float32, 0, len(out.Data))
	for _, d := range out.Data {
		vecs = append(vecs, d.Embedding)
	}
	return p.finalize(texts, vecs)
}

// finalize — 件数・次元の検証 + 防御的 L2 正規化。
func (p *EmbeddingProvider) finalize(texts []string, vecs [][]float32) ([][]float32, error) {
	if len(vecs) != len(texts) {
		return nil, fmt.Errorf("embed: got %d vectors for %d texts", len(vecs), len(texts))
	}
	for i, v := range vecs {
		if p.cfg.Dim > 0 && len(v) != p.cfg.Dim {
			return nil, fmt.Errorf("embed: vector %d has dim %d, want %d", i, len(v), p.cfg.Dim)
		}
		vecs[i] = normalizeVec(v)
	}
	return vecs, nil
}

// ---------- backfill ヘルパー（CLI と derive 後フックが共用） ----------

// embedMissingConclusions — ベクトル未計算の結論を埋め込む。
// limit<=0 は無制限、batch は 1 リクエストあたり件数。バッチごとにコミット（中断・再開安全）。
func embedMissingConclusions(ctx context.Context, st *Store, p *EmbeddingProvider, batch, limit int, logf func(string, ...any)) (int, error) {
	if batch <= 0 {
		batch = 128
	}
	done := 0
	start := time.Now()
	for limit <= 0 || done < limit {
		n := batch
		if limit > 0 && done+n > limit {
			n = limit - done
		}
		rows, err := st.ConclusionsMissingVectors(n)
		if err != nil {
			return done, err
		}
		if len(rows) == 0 {
			break
		}
		texts := make([]string, len(rows))
		for i, r := range rows {
			texts[i] = r.Content
		}
		// 一過性障害（サイドカー再起動・瞬断など）に耐えるリトライ
		var vecs [][]float32
		var lerr error
		for attempt := 1; attempt <= 5; attempt++ {
			vecs, lerr = p.Embed(ctx, texts)
			if lerr == nil {
				break
			}
			if ctx.Err() != nil {
				return done, ctx.Err()
			}
			delay := time.Duration(attempt*attempt) * time.Second
			if logf != nil {
				logf("embed attempt %d/5 failed (%v) — retry in %s", attempt, lerr, delay)
			}
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return done, ctx.Err()
			}
		}
		if lerr != nil {
			return done, lerr
		}
		vrows := make([]VectorRow, len(rows))
		for i := range rows {
			vrows[i] = VectorRow{ID: rows[i].ID, Dim: len(vecs[i]), Vec: vecs[i]}
		}
		if err := st.UpsertVectors("conclusion", vrows); err != nil {
			return done, err
		}
		done += len(rows)
		if logf != nil {
			el := time.Since(start).Seconds()
			logf("embedded %d (%.1f/s)", done, float64(done)/math.Max(el, 1e-9))
		}
		if len(rows) < n {
			break
		}
		select {
		case <-ctx.Done():
			return done, ctx.Err()
		default:
		}
	}
	return done, nil
}
