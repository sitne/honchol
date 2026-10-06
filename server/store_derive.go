// store_derive.go — derive（派生パイプライン）用の store 拡張 + P1 スキーマからの加算移行
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// ---------- スキーマ移行（加算のみ・冪等） ----------

// migrate — P1 スキーマに P2 の列を追加（既存行は既定値）。
func (s *Store) migrate() error {
	adds := []struct{ table, col, ddl string }{
		{"messages", "processed", "INTEGER NOT NULL DEFAULT 0"},
		{"messages", "tries", "INTEGER NOT NULL DEFAULT 0"},
		{"conclusions", "sources", "TEXT NOT NULL DEFAULT '[]'"},
		{"conclusions", "updated_at", "TEXT NOT NULL DEFAULT ''"},
	}
	for _, a := range adds {
		has, err := s.columnExists(a.table, a.col)
		if err != nil {
			return err
		}
		if has {
			continue
		}
		if _, err := s.db.Exec("ALTER TABLE " + a.table + " ADD COLUMN " + a.col + " " + a.ddl); err != nil {
			return fmt.Errorf("migrate %s.%s: %w", a.table, a.col, err)
		}
	}
	// 既定値の補正（updated_at 未設定の既存行がある場合のみ実行。全表 UPDATE を毎起動で走らせない）
	var dirty int
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM conclusions WHERE updated_at='' LIMIT 1)`).Scan(&dirty); err != nil {
		return err
	}
	if dirty == 1 {
		if _, err := s.db.Exec(`UPDATE conclusions SET updated_at = created_at WHERE updated_at = ''`); err != nil {
			return err
		}
	}
	// 未処理メッセージ参照の部分索引（derive のセッション走査を全表走査にしない）
	if _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_messages_unproc ON messages(workspace_id, session_id) WHERE processed=0`); err != nil {
		return err
	}
	return nil
}

func (s *Store) columnExists(table, col string) (bool, error) {
	rows, err := s.db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == col {
			return true, nil
		}
	}
	return false, rows.Err()
}

// ---------- derive 参照 ----------

type deriveSessionRef struct {
	WS        string
	SessionID string
	MinRowid  int64
}

// DeriveSessions — 未処理メッセージを持つセッション（古い順）。
func (s *Store) DeriveSessions(limit int) ([]deriveSessionRef, error) {
	rows, err := s.db.Query(`SELECT workspace_id, session_id, MIN(rowid) AS mr FROM messages WHERE processed=0 GROUP BY workspace_id, session_id ORDER BY mr LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []deriveSessionRef
	for rows.Next() {
		var r deriveSessionRef
		if err := rows.Scan(&r.WS, &r.SessionID, &r.MinRowid); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UnprocessedMessages — セッション内の未処理メッセージ（古い順・最大 limit）。
func (s *Store) UnprocessedMessages(ws, sid string, limit int) ([]messageRow, error) {
	q := `SELECT id, workspace_id, session_id, peer_id, created_at, content, token_count, metadata
	      FROM messages WHERE workspace_id=? AND session_id=? AND processed=0 ORDER BY rowid ASC LIMIT ?`
	rows, err := s.db.Query(q, ws, sid, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []messageRow
	for rows.Next() {
		var r messageRow
		if err := rows.Scan(&r.ID, &r.WS, &r.SessionID, &r.PeerID, &r.CreatedAt, &r.Content, &r.TokenCount, &r.Metadata); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ScheduleRetry — 失敗バッチ: tries++ し、上限到達で processed=2（dead-letter・データ保持）。
func (s *Store) ScheduleRetry(ids []string, maxTries int) error {
	if len(ids) == 0 {
		return nil
	}
	args := []any{maxTries}
	for _, id := range ids {
		args = append(args, id)
	}
	_, err := s.db.Exec(`UPDATE messages SET tries = tries + 1, processed = CASE WHEN tries + 1 >= ? THEN 2 ELSE 0 END WHERE id IN (`+inPlaceholders(len(ids))+`)`, args...)
	return err
}

func inPlaceholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// ---------- 結論（sources 付き） ----------

// AddDerivedConclusion — derive が書く結論（sources = message id 配列、出典は内部保持）。
func (s *Store) AddDerivedConclusion(ws, observer, observed, content, level string, sources []string, sessionID string) error {
	if sources == nil {
		sources = []string{}
	}
	src, err := json.Marshal(sources)
	if err != nil {
		return err
	}
	now := nowISO()
	var sidArg any
	if sessionID != "" {
		sidArg = sessionID
	}
	_, err = s.db.Exec(`INSERT INTO conclusions(id, workspace_id, observer_id, observed_id, session_id, content, level, created_at, sources, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		newID(), ws, observer, observed, sidArg, content, level, now, string(src), now)
	return err
}

// GetConclusionSources — 出典（message id）配列。
func (s *Store) GetConclusionSources(id string) ([]string, error) {
	var raw string
	if err := s.db.QueryRow(`SELECT COALESCE(sources, '[]') FROM conclusions WHERE id=?`, id).Scan(&raw); err != nil {
		return nil, err
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, nil
	}
	return out, nil
}

// ---------- derive 書込み（トランザクション） ----------

type derivedInsert struct {
	WS, Observer, Observed, Content, Level string
	Sources                                []string
	SessionID                              string
}

// CommitDeriveBatch — 結論 INSERT + sources マージ + processed=1 を単一トランザクションで適用する。
// SREレビュー Must① 対応: 非トランザクションだとクラッシュ時に「結論は入ったがメッセージは
// 未処理のまま」の部分書込みが残り、次回バッチの再処理で重複が蓄積するため。
func (s *Store) CommitDeriveBatch(ids []string, inserts []derivedInsert, merges map[string][]string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := nowISO()
	for id, add := range merges {
		var raw string
		if err := tx.QueryRow(`SELECT COALESCE(sources, '[]') FROM conclusions WHERE id=?`, id).Scan(&raw); err != nil {
			return fmt.Errorf("merge read %s: %w", id, err)
		}
		var cur []string
		_ = json.Unmarshal([]byte(raw), &cur)
		seen := map[string]bool{}
		out := make([]string, 0, len(cur)+len(add))
		for _, x := range append(cur, add...) {
			if !seen[x] {
				seen[x] = true
				out = append(out, x)
			}
		}
		merged, err := json.Marshal(out)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE conclusions SET sources=?, updated_at=? WHERE id=?`, string(merged), now, id); err != nil {
			return fmt.Errorf("merge write %s: %w", id, err)
		}
	}
	for _, ins := range inserts {
		sources := ins.Sources
		if sources == nil {
			sources = []string{}
		}
		src, err := json.Marshal(sources)
		if err != nil {
			return err
		}
		var sidArg any
		if ins.SessionID != "" {
			sidArg = ins.SessionID
		}
		if _, err := tx.Exec(`INSERT INTO conclusions(id, workspace_id, observer_id, observed_id, session_id, content, level, created_at, sources, updated_at)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			newID(), ins.WS, ins.Observer, ins.Observed, sidArg, ins.Content, ins.Level, now, string(src), now); err != nil {
			return fmt.Errorf("insert conclusion: %w", err)
		}
	}
	if len(ids) > 0 {
		args := make([]any, 0, len(ids))
		for _, id := range ids {
			args = append(args, id)
		}
		if _, err := tx.Exec(`UPDATE messages SET processed=1 WHERE id IN (`+inPlaceholders(len(ids))+`)`, args...); err != nil {
			return fmt.Errorf("mark processed: %w", err)
		}
	}
	return tx.Commit()
}

// RequeueDeadLetters — dead-letter（processed=2）を pending に戻す（ws/sid は "" で全対象）。
func (s *Store) RequeueDeadLetters(ws, sid string) (int64, error) {
	q := `UPDATE messages SET processed=0, tries=0 WHERE processed=2`
	var args []any
	if ws != "" {
		q += ` AND workspace_id=?`
		args = append(args, ws)
	}
	if sid != "" {
		q += ` AND session_id=?`
		args = append(args, sid)
	}
	res, err := s.db.Exec(q, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return n, err
}

// RebuildFTS — FTS インデックスを内容テーブルから再構築する（`honchol fts-rebuild` 用）。
func (s *Store) RebuildFTS() error {
	for _, tbl := range []string{"messages_fts", "conclusions_fts"} {
		if _, err := s.db.Exec(`INSERT INTO ` + tbl + `(` + tbl + `) VALUES('rebuild')`); err != nil {
			return fmt.Errorf("rebuild %s: %w", tbl, err)
		}
	}
	return nil
}

// ---------- 要約・メタ・キュー ----------

func (s *Store) SetSummary(ws, sid, content string, uptoRowid int64) error {
	_, err := s.db.Exec(`INSERT INTO summaries(workspace_id, session_id, content, upto_rowid, updated_at)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(workspace_id, session_id) DO UPDATE SET
		  content=excluded.content, upto_rowid=excluded.upto_rowid, updated_at=excluded.updated_at`,
		ws, sid, content, uptoRowid, nowISO())
	return err
}

func (s *Store) GetSummary(ws, sid string) (string, bool, error) {
	var c string
	err := s.db.QueryRow(`SELECT content FROM summaries WHERE workspace_id=? AND session_id=?`, ws, sid).Scan(&c)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return c, true, nil
}

// SummaryState — 要約以降の処理済みメッセージ数と、セッション最終発言時刻。
func (s *Store) SummaryState(ws, sid string) (since int, lastAt string, err error) {
	var upto int64
	if err = s.db.QueryRow(`SELECT COALESCE((SELECT upto_rowid FROM summaries WHERE workspace_id=? AND session_id=?), 0)`, ws, sid).Scan(&upto); err != nil {
		return
	}
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE workspace_id=? AND session_id=? AND processed=1 AND rowid > ?`, ws, sid, upto).Scan(&since); err != nil {
		return
	}
	err = s.db.QueryRow(`SELECT COALESCE(MAX(created_at), '') FROM messages WHERE workspace_id=? AND session_id=?`, ws, sid).Scan(&lastAt)
	return
}

// RecentMessagesForSummary — セッション末尾の limit 件（古い順で返す）。
func (s *Store) RecentMessagesForSummary(ws, sid string, limit int) ([]messageRow, error) {
	q := `SELECT id, workspace_id, session_id, peer_id, created_at, content, token_count, metadata
	      FROM messages WHERE workspace_id=? AND session_id=? ORDER BY rowid DESC LIMIT ?`
	rows, err := s.db.Query(q, ws, sid, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []messageRow
	for rows.Next() {
		var r messageRow
		if err := rows.Scan(&r.ID, &r.WS, &r.SessionID, &r.PeerID, &r.CreatedAt, &r.Content, &r.TokenCount, &r.Metadata); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func (s *Store) LastMessageRowid(ws, sid string) (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT COALESCE(MAX(rowid), 0) FROM messages WHERE workspace_id=? AND session_id=?`, ws, sid).Scan(&n)
	return n, err
}

// QueueCounts — derive 進捗。total=全メッセージ、completed=処理済(processed=1)、
// pending=未処理(0)、dead=dead-letter(2)。ws="" は全ワークスペース。
// SREレビュー Must② 対応: dead-letter を completed に合算しない（恒久失敗を沈黙させない）。
func (s *Store) QueueCounts(ws string) (total, completed, pending, dead int, err error) {
	q := `SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN processed = 1 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN processed = 0 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN processed = 2 THEN 1 ELSE 0 END), 0)
		FROM messages`
	var args []any
	if ws != "" {
		q += ` WHERE workspace_id=?`
		args = append(args, ws)
	}
	err = s.db.QueryRow(q, args...).Scan(&total, &completed, &pending, &dead)
	return
}

func (s *Store) SetMeta(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO meta(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func (s *Store) GetMeta(key string) (string, bool, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}
