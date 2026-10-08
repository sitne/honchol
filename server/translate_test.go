package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func testTranslateServer(t *testing.T, calls *atomic.Int32, status int, translation string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"translations": []string{translation},
			"model":        "test",
		})
	}))
}

func newTranslateTestServer(t *testing.T, url string) *apiServer {
	t.Helper()
	st, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return &apiServer{
		st: st,
		tp: &translateProvider{
			cfg: translateConfig{kind: "local", url: url, timeout: time.Second, maxLen: 400},
			hc:  &http.Client{},
		},
	}
}

func TestTranslateQueryAndCache(t *testing.T) {
	var calls atomic.Int32
	ts := testTranslateServer(t, &calls, http.StatusOK, "calibration test")
	defer ts.Close()
	srv := newTranslateTestServer(t, ts.URL)
	ctx := context.Background()

	out, ok := srv.translateQuery(ctx, "較正テスト")
	if !ok || out != "calibration test" {
		t.Fatalf("translateQuery = (%q, %v), want (calibration test, true)", out, ok)
	}
	out, ok = srv.translateQuery(ctx, "較正テスト")
	if !ok || out != "calibration test" {
		t.Fatalf("cache hit = (%q, %v)", out, ok)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("sidecar calls = %d, want 1 (cache must be used)", got)
	}
}

func TestTranslateQuerySkipsNonCJK(t *testing.T) {
	var calls atomic.Int32
	ts := testTranslateServer(t, &calls, http.StatusOK, "x")
	defer ts.Close()
	srv := newTranslateTestServer(t, ts.URL)
	if _, ok := srv.translateQuery(context.Background(), "calibration"); ok {
		t.Fatal("non-CJK query must skip translation")
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("sidecar calls = %d, want 0", got)
	}
}

func TestTranslateQueryFailSoft(t *testing.T) {
	var calls atomic.Int32
	ts := testTranslateServer(t, &calls, http.StatusInternalServerError, "")
	defer ts.Close()
	srv := newTranslateTestServer(t, ts.URL)
	if _, ok := srv.translateQuery(context.Background(), "較正"); ok {
		t.Fatal("failure must be fail-soft (ok=false)")
	}
}

func TestTranslateQueryNoneProvider(t *testing.T) {
	srv := &apiServer{}
	if _, ok := srv.translateQuery(context.Background(), "較正"); ok {
		t.Fatal("nil provider must skip")
	}
}

func TestHasCJK(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"較正", true},
		{"ぺっと", true},
		{"ペット", true},
		{"calibration", false},
		{"Modal 123", false},
		{"C++の話", true},
	}
	for _, c := range cases {
		if got := hasCJK(c.in); got != c.want {
			t.Fatalf("hasCJK(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestCleanTranslation(t *testing.T) {
	cases := map[string]string{
		`  "Calibration"  `: "Calibration",
		`'pet'`:             "pet",
		`“埋め込み”`:            "埋め込み",
	}
	for in, want := range cases {
		if got := cleanTranslation(in); got != want {
			t.Fatalf("cleanTranslation(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTranslateCacheRoundtrip(t *testing.T) {
	st, err := OpenStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if err := st.PutTranslateCache("h1", "較正", "Calibration", "local"); err != nil {
		t.Fatalf("put: %v", err)
	}
	v, ok, err := st.GetTranslateCache("h1")
	if err != nil || !ok || v != "Calibration" {
		t.Fatalf("get = (%q, %v, %v)", v, ok, err)
	}
	if _, ok, _ := st.GetTranslateCache("nope"); ok {
		t.Fatal("missing key must return ok=false")
	}
}
