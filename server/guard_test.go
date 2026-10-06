package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestGuardHostOrigin — ローカル防壁: Host 偽装（DNSリバインディング想定）と
// クロスサイト Origin を 403 で拒否し、loopback は通す。
func TestGuardHostOrigin(t *testing.T) {
	st := testStore(t)
	srv := &apiServer{st: st}
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v3/workspaces/w/queue/status")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("normal request status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v3/workspaces/w/queue/status", nil)
	req.Host = "evil.example.com"
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp2.StatusCode != http.StatusForbidden {
		t.Fatalf("host spoof status = %d, want 403", resp2.StatusCode)
	}
	resp2.Body.Close()

	req3, _ := http.NewRequest(http.MethodPost, ts.URL+"/v3/workspaces", strings.NewReader(`{"id":"w"}`))
	req3.Header.Set("Origin", "https://evil.example.com")
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	if resp3.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site origin status = %d, want 403", resp3.StatusCode)
	}
	resp3.Body.Close()

	req4, _ := http.NewRequest(http.MethodPost, ts.URL+"/v3/workspaces", strings.NewReader(`{"id":"w2"}`))
	req4.Header.Set("Origin", "http://127.0.0.1:8788")
	resp4, err := http.DefaultClient.Do(req4)
	if err != nil {
		t.Fatal(err)
	}
	if resp4.StatusCode != http.StatusOK {
		t.Fatalf("loopback origin status = %d, want 200", resp4.StatusCode)
	}
	resp4.Body.Close()
}

// TestGuardBodyLimit — 4MiB 超のボディを拒否する（メモリ/DB/課金LLMの焼却防止）。
func TestGuardBodyLimit(t *testing.T) {
	st := testStore(t)
	srv := &apiServer{st: st}
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	big := strings.Repeat("a", (4<<20)+4096)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v3/workspaces/w/sessions/s/messages",
		strings.NewReader(`{"messages":[{"peer_id":"p","content":"`+big+`"}]}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized body status = %d, want 400", resp.StatusCode)
	}
}

// TestIsLoopbackAddr — 空ホスト（":8788" = 全インタフェース bind）と 0.0.0.0/:: を
// 非ループバックとして拒否扱いにする（公開前レビュー指摘の回帰テスト）。
func TestIsLoopbackAddr(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:8788": true,
		"localhost:8788": true,
		"[::1]:8788":     true,
		"8788":           false, // ホストなしの数字だけは無効 → 非ループバック扱い
		":8788":          false,
		"0.0.0.0:8788":   false,
		"[::]:8788":      false,
		"192.0.2.1:8788": false,
	}
	for addr, want := range cases {
		if got := isLoopbackAddr(addr); got != want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", addr, got, want)
		}
	}
}
