// fts.go — FTS5（trigram）候補検索（spec §3.3.1 候補段）
//
// messages / conclusions の外部コンテンツ FTS5 インデックスを張り、
// 部分一致（trigram はサブストリング一致・CJK可）候補を返す。
// trigram が使えない SQLite では unicode61 にフォールバックし、
// 候補は LIKE が補完する（検索自体は常に成立する）。
package main

import (
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

// initFTS — FTS5 テーブル＋同期トリガを用意し、件数不一致時のみ rebuild。
func (s *Store) initFTS() error {
	ddl := func(tok string) []string {
		return []string{
			fmt.Sprintf(`CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(content, content='messages', content_rowid='rowid', tokenize='%s')`, tok),
			`CREATE TRIGGER IF NOT EXISTS messages_fts_ai AFTER INSERT ON messages BEGIN
			   INSERT INTO messages_fts(rowid, content) VALUES (new.rowid, new.content);
			 END`,
			`CREATE TRIGGER IF NOT EXISTS messages_fts_ad AFTER DELETE ON messages BEGIN
			   INSERT INTO messages_fts(messages_fts, rowid, content) VALUES ('delete', old.rowid, old.content);
			 END`,
			`CREATE TRIGGER IF NOT EXISTS messages_fts_au AFTER UPDATE OF content ON messages BEGIN
			   INSERT INTO messages_fts(messages_fts, rowid, content) VALUES ('delete', old.rowid, old.content);
			   INSERT INTO messages_fts(rowid, content) VALUES (new.rowid, new.content);
			 END`,
			fmt.Sprintf(`CREATE VIRTUAL TABLE IF NOT EXISTS conclusions_fts USING fts5(content, content='conclusions', content_rowid='rowid', tokenize='%s')`, tok),
			`CREATE TRIGGER IF NOT EXISTS conclusions_fts_ai AFTER INSERT ON conclusions BEGIN
			   INSERT INTO conclusions_fts(rowid, content) VALUES (new.rowid, new.content);
			 END`,
			`CREATE TRIGGER IF NOT EXISTS conclusions_fts_ad AFTER DELETE ON conclusions BEGIN
			   INSERT INTO conclusions_fts(conclusions_fts, rowid, content) VALUES ('delete', old.rowid, old.content);
			 END`,
			`CREATE TRIGGER IF NOT EXISTS conclusions_fts_au AFTER UPDATE OF content ON conclusions BEGIN
			   INSERT INTO conclusions_fts(conclusions_fts, rowid, content) VALUES ('delete', old.rowid, old.content);
			   INSERT INTO conclusions_fts(rowid, content) VALUES (new.rowid, new.content);
			 END`,
		}
	}
	run := func(tok string) error {
		for _, q := range ddl(tok) {
			if _, err := s.db.Exec(q); err != nil {
				return err
			}
		}
		return nil
	}
	if err := run("trigram"); err != nil {
		log.Printf("fts: trigram unavailable (%v) — retry with unicode61 (CJK substring match falls back to LIKE)", err)
		// 部分生成物は派生インデックスなので破棄してからやり直す
		if _, e1 := s.db.Exec(`DROP TABLE IF EXISTS messages_fts`); e1 != nil {
			return e1
		}
		if _, e2 := s.db.Exec(`DROP TABLE IF EXISTS conclusions_fts`); e2 != nil {
			return e2
		}
		if err2 := run("unicode61"); err2 != nil {
			return err2
		}
		s.ftsTrigram = false
	} else {
		s.ftsTrigram = true
	}
	// 既存行のバックフィル（件数不一致時のみ）
	for _, t := range []struct{ fts, src string }{{"messages_fts", "messages"}, {"conclusions_fts", "conclusions"}} {
		var a, b int64
		if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM `+t.src+`), (SELECT COUNT(*) FROM `+t.fts+`)`).Scan(&a, &b); err != nil {
			return err
		}
		if a == b {
			continue
		}
		if os.Getenv("HONCHO_LITE_FTS_REBUILD") == "0" {
			log.Printf("fts: %s mismatch (src=%d fts=%d) — rebuild skipped; run `honchol fts-rebuild`", t.fts, a, b)
			continue
		}
		start := time.Now()
		if _, err := s.db.Exec(`INSERT INTO ` + t.fts + `(` + t.fts + `) VALUES('rebuild')`); err != nil {
			return err
		}
		log.Printf("fts: rebuilt %s (src=%d fts=%d, %.2fs)", t.fts, a, b, time.Since(start).Seconds())
	}
	return nil
}

// ftsMatchQuery — クエリを FTS5 MATCH 式にする。
// 空白区切りで 3 ルーン以上の語を引用フレーズ化し AND で連結。該当なしは ""（FTS スキップ）。
func ftsMatchQuery(q string) string {
	var parts []string
	for _, t := range strings.Fields(q) {
		if len([]rune(t)) >= 3 {
			parts = append(parts, `"`+strings.ReplaceAll(t, `"`, `""`)+`"`)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " AND ")
}

const msgCols = `m.id, m.workspace_id, m.session_id, m.peer_id, m.created_at, m.content, m.token_count, m.metadata`

// SearchCandidates — 候補段: FTS5（サブストリング）∪ LIKE（重複除去・FTS優先順）。limit は候補上限。
func (s *Store) SearchCandidates(ws, sid, pid, query string, limit int) ([]messageRow, error) {
	var out []messageRow
	seen := map[string]bool{}
	add := func(rows []messageRow) {
		for _, r := range rows {
			if !seen[r.ID] {
				seen[r.ID] = true
				out = append(out, r)
			}
		}
	}
	if s.ftsOK {
		if mq := ftsMatchQuery(query); mq != "" {
			where := `WHERE messages_fts MATCH ? AND m.workspace_id=?`
			args := []any{mq, ws}
			if sid != "" {
				where += ` AND m.session_id=?`
				args = append(args, sid)
			}
			if pid != "" {
				where += ` AND m.peer_id=?`
				args = append(args, pid)
			}
			args = append(args, limit)
			rows, err := s.db.Query(`SELECT `+msgCols+` FROM messages_fts JOIN messages m ON m.rowid = messages_fts.rowid `+where+` ORDER BY rank LIMIT ?`, args...)
			if err != nil {
				log.Printf("fts: query failed (%v) — LIKE only", err)
			} else {
				var hits []messageRow
				for rows.Next() {
					var r messageRow
					if err := rows.Scan(&r.ID, &r.WS, &r.SessionID, &r.PeerID, &r.CreatedAt, &r.Content, &r.TokenCount, &r.Metadata); err != nil {
						rows.Close()
						return nil, err
					}
					hits = append(hits, r)
				}
				rows.Close()
				if err := rows.Err(); err != nil {
					return nil, err
				}
				add(hits)
			}
		}
	}
	// LIKE 補完（2ルーン以下・フレーズ不一致・unicode61 フォールバック時を拾う）
	like, err := s.SearchMessages(ws, sid, pid, query, limit)
	if err != nil {
		return nil, err
	}
	add(like)
	if len(out) > limit {
		out = out[:limit]
	}
	if out == nil {
		out = []messageRow{}
	}
	return out, nil
}

const conclCols = `c.id, c.workspace_id, c.observer_id, c.observed_id, c.session_id, c.content, c.level, c.created_at`

// SearchConclusionCandidates — 結論（conclusions）の候補段。observer/observed は "" で任意。
func (s *Store) SearchConclusionCandidates(ws, observer, observed, query string, limit int) ([]conclusionRow, error) {
	var out []conclusionRow
	seen := map[string]bool{}
	add := func(rows []conclusionRow) {
		for _, r := range rows {
			if !seen[r.ID] {
				seen[r.ID] = true
				out = append(out, r)
			}
		}
	}
	if s.ftsOK {
		if mq := ftsMatchQuery(query); mq != "" {
			where := `WHERE conclusions_fts MATCH ? AND c.workspace_id=?`
			args := []any{mq, ws}
			if observer != "" {
				where += ` AND c.observer_id=?`
				args = append(args, observer)
			}
			if observed != "" {
				where += ` AND c.observed_id=?`
				args = append(args, observed)
			}
			args = append(args, limit)
			rows, err := s.db.Query(`SELECT `+conclCols+` FROM conclusions_fts JOIN conclusions c ON c.rowid = conclusions_fts.rowid `+where+` ORDER BY rank LIMIT ?`, args...)
			if err != nil {
				log.Printf("fts: conclusions query failed (%v) — LIKE only", err)
			} else {
				var hits []conclusionRow
				for rows.Next() {
					var r conclusionRow
					if err := rows.Scan(&r.ID, &r.WS, &r.ObserverID, &r.ObservedID, &r.SessionID, &r.Content, &r.Level, &r.CreatedAt); err != nil {
						rows.Close()
						return nil, err
					}
					hits = append(hits, r)
				}
				rows.Close()
				if err := rows.Err(); err != nil {
					return nil, err
				}
				add(hits)
			}
		}
	}
	like, err := s.QueryConclusions(ws, observer, observed, query, limit)
	if err != nil {
		return nil, err
	}
	add(like)
	if len(out) > limit {
		out = out[:limit]
	}
	if out == nil {
		out = []conclusionRow{}
	}
	return out, nil
}
