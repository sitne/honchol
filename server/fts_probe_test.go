package main

import "testing"

// TestFTS5Availability — P2 検索（FTS5 化）の設計判断用プローブ。
// 利用不可でもスキップ（失敗扱いにしない）。
func TestFTS5Availability(t *testing.T) {
	st, err := OpenStore(t.TempDir() + "/fts.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.db.Exec(`CREATE VIRTUAL TABLE IF NOT EXISTS _fts_probe USING fts5(body)`); err != nil {
		t.Skipf("FTS5 not available: %v", err)
	}
	if _, err := st.db.Exec(`INSERT INTO _fts_probe(body) VALUES ('hello world')`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.db.QueryRow(`SELECT count(*) FROM _fts_probe WHERE _fts_probe MATCH 'hello'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("n = %d", n)
	}
	t.Log("FTS5 available: yes")
}
