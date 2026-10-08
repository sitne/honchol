package main

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestVecBlobRoundtrip(t *testing.T) {
	in := []float32{0, 1.5, -2.25, 3.14159, -0.0001}
	out, err := blobToVec(vecToBlob(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(in) {
		t.Fatalf("len %d != %d", len(out), len(in))
	}
	for i := range in {
		if in[i] != out[i] {
			t.Fatalf("at %d: %v != %v", i, in[i], out[i])
		}
	}
	if _, err := blobToVec([]byte{1, 2, 3}); err == nil {
		t.Fatal("expected error for non-multiple-of-4 blob")
	}
}

func TestEmbeddingConfigDefaults(t *testing.T) {
	for _, k := range []string{"HONCHO_LITE_EMBED_PROVIDER", "HONCHO_LITE_EMBED_URL", "HONCHO_LITE_EMBED_DIM", "HONCHO_LITE_EMBED_TIMEOUT_MS"} {
		t.Setenv(k, "")
	}
	cfg := EmbeddingConfigFromEnv()
	if cfg.Kind != "none" || cfg.Dim != 384 || cfg.URL == "" || cfg.Timeout <= 0 {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if NewEmbeddingProvider(cfg).Enabled() {
		t.Fatal("none provider should not be enabled")
	}
}

func fakeEmbedServer(t *testing.T, dim int, badDim bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Texts []string `json:"texts"`
		}
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &req); err != nil {
			w.WriteHeader(400)
			return
		}
		d := dim
		if badDim {
			d = dim + 1
		}
		vecs := make([][]float32, len(req.Texts))
		for i := range req.Texts {
			v := make([]float32, d)
			for j := range v {
				v[j] = float32(j + 1)
			}
			vecs[i] = v
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"vectors": vecs, "dim": d, "model": "fake"})
	}))
}

func TestEmbedLocalProvider(t *testing.T) {
	srv := fakeEmbedServer(t, 4, false)
	defer srv.Close()
	p := NewEmbeddingProvider(EmbeddingConfig{Kind: "local", URL: srv.URL, Dim: 4, Timeout: 5 * time.Second})
	vecs, err := p.Embed(context.Background(), []string{"a", "bb"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 2 || len(vecs[0]) != 4 {
		t.Fatalf("shape: %d x %d", len(vecs), len(vecs[0]))
	}
	var n float64
	for _, f := range vecs[0] {
		n += float64(f) * float64(f)
	}
	if math.Abs(math.Sqrt(n)-1) > 1e-5 {
		t.Fatalf("not normalized: |v|=%v", math.Sqrt(n))
	}

	bad := NewEmbeddingProvider(EmbeddingConfig{Kind: "local", URL: srv.URL, Dim: 8, Timeout: 5 * time.Second})
	if _, err := bad.Embed(context.Background(), []string{"a"}); err == nil {
		t.Fatal("expected dim mismatch error")
	}
}

func TestStoreVectorsAndBackfill(t *testing.T) {
	st := testStore(t)
	if _, err := st.CreateConclusion("w", "obs", "obd", "較正テストの記録", "explicit", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateConclusion("w", "obs", "obd", "別件のメモ", "explicit", nil); err != nil {
		t.Fatal(err)
	}
	miss, err := st.ConclusionsMissingVectors(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(miss) != 2 {
		t.Fatalf("missing = %d, want 2", len(miss))
	}

	srv := fakeEmbedServer(t, 4, false)
	defer srv.Close()
	p := NewEmbeddingProvider(EmbeddingConfig{Kind: "local", URL: srv.URL, Dim: 4, Timeout: 5 * time.Second})
	done, err := embedMissingConclusions(context.Background(), st, p, 1, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if done != 2 {
		t.Fatalf("done = %d, want 2", done)
	}
	n, err := st.VectorCount("conclusion")
	if err != nil || n != 2 {
		t.Fatalf("count = %d err=%v", n, err)
	}
	miss, err = st.ConclusionsMissingVectors(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(miss) != 0 {
		t.Fatalf("missing after backfill = %d", len(miss))
	}
	done, err = embedMissingConclusions(context.Background(), st, p, 4, 0, nil)
	if err != nil || done != 0 {
		t.Fatalf("second run: done=%d err=%v", done, err)
	}
}
