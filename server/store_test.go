package main

import "testing"

// TestDeleteSessionCascade — セッション削除で summaries とセッション由来の結論も
// 回収される（公開前レビュー指摘の回帰テスト。再作成時に旧 summary が復活しない）。
func TestDeleteSessionCascade(t *testing.T) {
	st := testStore(t)
	ws, sid := "w", "s1"
	if _, err := st.EnsureWorkspace(ws); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsurePeer(ws, "alice", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureSession(ws, sid, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.AddDerivedConclusion(ws, "agent", "alice", "aliceは較正の研究をしている", "explicit", []string{"m1"}, sid); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSummary(ws, sid, "summary text", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteSession(ws, sid); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.GetSummary(ws, sid); err != nil || ok {
		t.Fatalf("summary survived delete: ok=%v err=%v", ok, err)
	}
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM conclusions WHERE workspace_id=? AND session_id=?`, ws, sid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("session conclusions survived delete: %d", n)
	}
}
