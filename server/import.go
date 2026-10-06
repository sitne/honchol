// import.go — P3: Honcho エクスポート（honcho_export/memory.db）→ honchol ストア取込
//
// 方針:
//   - id は原物を保持し INSERT OR IGNORE（冪等・再実行は差分のみ）
//   - メッセージは created_at 昇順で投入（rowid = 時系列順を保証）
//   - 取込メッセージは processed=1（自動 derive を通さない。既存結論が同梱されるため）
//   - 結論の observer は agent（honchol の流儀）、level は inductive（機械導出物の既定）。
//     エクスポート側には session_id / level / 出典の来歴が存在しないため補完不能（既知の制約・README 参照）
//   - 時刻は既知形式のみ受理。判別不能は now() へフォールバックし計数（time_fallbacks）＋警告ログ
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"strings"
	"time"
)

type ImportStats struct {
	Peers         int `json:"peers"`
	Sessions      int `json:"sessions"`
	SessionPeers  int `json:"session_peers"`
	Messages      int `json:"messages"`
	Conclusions   int `json:"conclusions"`
	TimeFallbacks int `json:"time_fallbacks"`
}

// normImportTime — エクスポート側の時刻文字列（"2006-01-02 15:04:05.999999+00:00" 等）を timeFormat へ正規化。
// 既知形式に一致しない場合は normTime（now() フォールバック）に委譲し ok=false を返す。
func normImportTime(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nowISO(), false
	}
	if t, err := time.Parse("2006-01-02 15:04:05.999999-07:00", s); err == nil {
		return t.UTC().Format(timeFormat), true
	}
	for _, l := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05.999999+00:00"} {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC().Format(timeFormat), true
		}
	}
	return normTime(s), false
}

// importFrom — src を読み取り専用でストリームし、dst に単一トランザクションで取り込む。
func importFrom(dst *Store, src *sql.DB, ws string) (*ImportStats, error) {
	stats := &ImportStats{}
	now := nowISO()

	tx, err := dst.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	prepare := func(q string) (*sql.Stmt, error) { return tx.Prepare(q) }

	stmtWs, err := prepare(`INSERT OR IGNORE INTO workspaces(id, created_at) VALUES(?, ?)`)
	if err != nil {
		return nil, err
	}
	stmtPeer, err := prepare(`INSERT OR IGNORE INTO peers(workspace_id, id, created_at) VALUES(?, ?, ?)`)
	if err != nil {
		return nil, err
	}
	stmtSess, err := prepare(`INSERT OR IGNORE INTO sessions(workspace_id, id, created_at) VALUES(?, ?, ?)`)
	if err != nil {
		return nil, err
	}
	stmtMsg, err := prepare(`INSERT OR IGNORE INTO messages(id, workspace_id, session_id, peer_id, created_at, content, token_count, metadata, processed) VALUES(?, ?, ?, ?, ?, ?, ?, ?, 1)`)
	if err != nil {
		return nil, err
	}
	stmtSP, err := prepare(`INSERT OR IGNORE INTO session_peers(workspace_id, session_id, peer_id) VALUES(?, ?, ?)`)
	if err != nil {
		return nil, err
	}
	stmtCon, err := prepare(`INSERT OR IGNORE INTO conclusions(id, workspace_id, observer_id, observed_id, session_id, content, level, created_at, sources, updated_at) VALUES(?, ?, ?, ?, NULL, ?, 'inductive', ?, '[]', ?)`)
	if err != nil {
		return nil, err
	}

	// 時刻フォールバックの計数つきヘルパ
	noteTime := func(where, raw string, ok bool) {
		if ok {
			return
		}
		stats.TimeFallbacks++
		if stats.TimeFallbacks <= 5 {
			log.Printf("import: unknown time format (%s): %q — now() で代替", where, raw)
		}
	}

	// workspace
	if _, err := stmtWs.Exec(ws, now); err != nil {
		return nil, err
	}

	// peers
	{
		rows, err := src.Query(`SELECT id FROM peers ORDER BY id`)
		if err != nil {
			return nil, fmt.Errorf("src peers: %w", err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			res, err := stmtPeer.Exec(ws, id, now)
			if err != nil {
				rows.Close()
				return nil, err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				stats.Peers++
			}
		}
		rows.Close()
	}

	// sessions
	{
		rows, err := src.Query(`SELECT id, created_at FROM sessions ORDER BY created_at, id`)
		if err != nil {
			return nil, fmt.Errorf("src sessions: %w", err)
		}
		for rows.Next() {
			var id, created string
			if err := rows.Scan(&id, &created); err != nil {
				rows.Close()
				return nil, err
			}
			ts, ok := normImportTime(created)
			noteTime("sessions", created, ok)
			res, err := stmtSess.Exec(ws, id, ts)
			if err != nil {
				rows.Close()
				return nil, err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				stats.Sessions++
			}
		}
		rows.Close()
	}

	// messages（created_at 昇順 = rowid 時系列保証。session_peers ペアも収集）
	pairs := map[[2]string]bool{}
	{
		rows, err := src.Query(`SELECT id, session_id, peer_id, created_at, content, token_count, metadata FROM messages ORDER BY created_at, id`)
		if err != nil {
			return nil, fmt.Errorf("src messages: %w", err)
		}
		for rows.Next() {
			var id, sid, pid, created, content, metadata string
			var tokens int
			if err := rows.Scan(&id, &sid, &pid, &created, &content, &tokens, &metadata); err != nil {
				rows.Close()
				return nil, err
			}
			if metadata == "" {
				metadata = "{}"
			}
			ts, ok := normImportTime(created)
			noteTime("messages", created, ok)
			res, err := stmtMsg.Exec(id, ws, sid, pid, ts, content, tokens, metadata)
			if err != nil {
				rows.Close()
				return nil, fmt.Errorf("insert message %s: %w", id, err)
			}
			if n, _ := res.RowsAffected(); n > 0 {
				stats.Messages++
				pairs[[2]string{sid, pid}] = true
			}
		}
		rows.Close()
	}

	// session_peers（メッセージから導出）
	for p := range pairs {
		res, err := stmtSP.Exec(ws, p[0], p[1])
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			stats.SessionPeers++
		}
	}

	// conclusions
	{
		rows, err := src.Query(`SELECT id, peer_id, content, created_at FROM conclusions ORDER BY created_at, id`)
		if err != nil {
			return nil, fmt.Errorf("src conclusions: %w", err)
		}
		for rows.Next() {
			var id, pid, content, created string
			if err := rows.Scan(&id, &pid, &content, &created); err != nil {
				rows.Close()
				return nil, err
			}
			t, ok := normImportTime(created)
			noteTime("conclusions", created, ok)
			res, err := stmtCon.Exec(id, ws, deriveObserver, pid, content, t, t)
			if err != nil {
				rows.Close()
				return nil, fmt.Errorf("insert conclusion %s: %w", id, err)
			}
			if n, _ := res.RowsAffected(); n > 0 {
				stats.Conclusions++
			}
		}
		rows.Close()
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return stats, nil
}

// cmdImport — main.go から呼ばれる CLI エントリ。
func cmdImport(args []string) {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	dbPath := fs.String("db", "honchol.db", "destination sqlite database")
	srcPath := fs.String("src", "", "source export sqlite (read-only, required)")
	ws := fs.String("ws", "default", "destination workspace id")
	fs.Parse(args)

	dst, err := OpenStore(*dbPath)
	if err != nil {
		log.Fatalf("open dest: %v", err)
	}
	defer dst.Close()

	src, err := sql.Open("sqlite", "file:"+*srcPath+"?mode=ro")
	if err != nil {
		log.Fatalf("open src: %v", err)
	}
	defer src.Close()

	start := time.Now()
	stats, err := importFrom(dst, src, *ws)
	if err != nil {
		log.Fatalf("import: %v", err)
	}
	blob, _ := json.Marshal(stats)
	log.Printf("import done in %s: %s", time.Since(start).Round(time.Millisecond), blob)
	fmt.Println(string(blob))
}
