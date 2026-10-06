package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// ---------- canonical formats ----------

const timeFormat = "2006-01-02T15:04:05.000000+00:00"

func nowISO() string { return time.Now().UTC().Format(timeFormat) }

func normTime(s string) string {
	if s == "" {
		return nowISO()
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, timeFormat} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().Format(timeFormat)
		}
	}
	return nowISO()
}

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("id-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func countTokens(s string) int {
	n := len([]rune(s))/4 + 1
	if n < 1 {
		n = 1
	}
	return n
}

func normJSON(raw string) string {
	t := strings.TrimSpace(raw)
	if t == "" || t == "null" || !json.Valid([]byte(t)) {
		return "{}"
	}
	return t
}

// ---------- store ----------

const schemaSQL = `
CREATE TABLE IF NOT EXISTS workspaces (
  id TEXT PRIMARY KEY,
  created_at TEXT NOT NULL,
  metadata TEXT NOT NULL DEFAULT '{}'
);
CREATE TABLE IF NOT EXISTS peers (
  workspace_id TEXT NOT NULL,
  id TEXT NOT NULL,
  created_at TEXT NOT NULL,
  metadata TEXT NOT NULL DEFAULT '{}',
  configuration TEXT NOT NULL DEFAULT '{}',
  PRIMARY KEY (workspace_id, id)
);
CREATE TABLE IF NOT EXISTS sessions (
  workspace_id TEXT NOT NULL,
  id TEXT NOT NULL,
  created_at TEXT NOT NULL,
  is_active INTEGER NOT NULL DEFAULT 1,
  metadata TEXT NOT NULL DEFAULT '{}',
  configuration TEXT NOT NULL DEFAULT '{}',
  PRIMARY KEY (workspace_id, id)
);
CREATE TABLE IF NOT EXISTS session_peers (
  workspace_id TEXT NOT NULL,
  session_id TEXT NOT NULL,
  peer_id TEXT NOT NULL,
  observe_me INTEGER,
  observe_others INTEGER,
  PRIMARY KEY (workspace_id, session_id, peer_id)
);
CREATE TABLE IF NOT EXISTS messages (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL,
  session_id TEXT NOT NULL,
  peer_id TEXT NOT NULL,
  created_at TEXT NOT NULL,
  content TEXT NOT NULL,
  token_count INTEGER NOT NULL DEFAULT 0,
  metadata TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(workspace_id, session_id, created_at);
CREATE INDEX IF NOT EXISTS idx_messages_peer ON messages(workspace_id, peer_id, created_at);
CREATE TABLE IF NOT EXISTS conclusions (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL,
  observer_id TEXT NOT NULL,
  observed_id TEXT NOT NULL,
  session_id TEXT,
  content TEXT NOT NULL,
  level TEXT NOT NULL DEFAULT 'explicit',
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_conclusions_pair ON conclusions(workspace_id, observer_id, observed_id, created_at);
CREATE TABLE IF NOT EXISTS cards (
  workspace_id TEXT NOT NULL,
  observer_id TEXT NOT NULL,
  target_id TEXT NOT NULL,
  card TEXT NOT NULL DEFAULT '[]',
  updated_at TEXT NOT NULL,
  PRIMARY KEY (workspace_id, observer_id, target_id)
);
CREATE TABLE IF NOT EXISTS summaries (
  workspace_id TEXT NOT NULL,
  session_id TEXT NOT NULL,
  content TEXT NOT NULL,
  upto_rowid INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (workspace_id, session_id)
);
CREATE TABLE IF NOT EXISTS meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
`

type Store struct {
	db         *sql.DB
	ftsOK      bool // FTS5 インデックスが利用可能
	ftsTrigram bool // trigram トークナイザ（CJK 部分一致対応）
}

func OpenStore(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, err
	}
	st := &Store{db: db}
	if err := st.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	if err := st.initFTS(); err != nil {
		log.Printf("fts: disabled (%v) — LIKE-only search", err)
	} else {
		st.ftsOK = true
	}
	return st, nil
}

func (s *Store) Close() error { return s.db.Close() }

// ---------- rows ----------

type wsRow struct {
	ID        string
	CreatedAt string
}

type peerRow struct {
	ID            string
	WS            string
	CreatedAt     string
	Metadata      string
	Configuration string
}

type sessionRow struct {
	ID            string
	WS            string
	CreatedAt     string
	IsActive      bool
	Metadata      string
	Configuration string
}

type messageRow struct {
	ID         string
	WS         string
	SessionID  string
	PeerID     string
	CreatedAt  string
	Content    string
	TokenCount int
	Metadata   string
}

type conclusionRow struct {
	ID         string
	WS         string
	ObserverID string
	ObservedID string
	CreatedAt  string
	Content    string
	Level      string
	SessionID  sql.NullString
}

// ---------- workspaces ----------

func (s *Store) EnsureWorkspace(id string) (wsRow, error) {
	_, err := s.db.Exec(`INSERT INTO workspaces(id, created_at) VALUES(?, ?) ON CONFLICT(id) DO NOTHING`, id, nowISO())
	if err != nil {
		return wsRow{}, err
	}
	var r wsRow
	err = s.db.QueryRow(`SELECT id, created_at FROM workspaces WHERE id=?`, id).Scan(&r.ID, &r.CreatedAt)
	return r, err
}

// ---------- peers ----------

func (s *Store) EnsurePeer(ws, id string, metadata string) (peerRow, error) {
	_, err := s.db.Exec(`INSERT INTO peers(workspace_id, id, created_at, metadata) VALUES(?, ?, ?, ?) ON CONFLICT(workspace_id, id) DO NOTHING`,
		ws, id, nowISO(), normJSON(metadata))
	if err != nil {
		return peerRow{}, err
	}
	return s.GetPeer(ws, id)
}

func (s *Store) GetPeer(ws, id string) (peerRow, error) {
	var r peerRow
	err := s.db.QueryRow(`SELECT workspace_id, id, created_at, metadata, configuration FROM peers WHERE workspace_id=? AND id=?`, ws, id).
		Scan(&r.WS, &r.ID, &r.CreatedAt, &r.Metadata, &r.Configuration)
	return r, err
}

func (s *Store) UpdatePeer(ws, id, metadata, configuration string) (peerRow, error) {
	if _, err := s.EnsurePeer(ws, id, "{}"); err != nil {
		return peerRow{}, err
	}
	if strings.TrimSpace(metadata) != "" {
		if _, err := s.db.Exec(`UPDATE peers SET metadata=? WHERE workspace_id=? AND id=?`, normJSON(metadata), ws, id); err != nil {
			return peerRow{}, err
		}
	}
	if strings.TrimSpace(configuration) != "" {
		if _, err := s.db.Exec(`UPDATE peers SET configuration=? WHERE workspace_id=? AND id=?`, normJSON(configuration), ws, id); err != nil {
			return peerRow{}, err
		}
	}
	return s.GetPeer(ws, id)
}

// ---------- sessions ----------

func (s *Store) EnsureSession(ws, id string, metadata string) (sessionRow, error) {
	_, err := s.db.Exec(`INSERT INTO sessions(workspace_id, id, created_at, metadata) VALUES(?, ?, ?, ?) ON CONFLICT(workspace_id, id) DO NOTHING`,
		ws, id, nowISO(), normJSON(metadata))
	if err != nil {
		return sessionRow{}, err
	}
	return s.GetSession(ws, id)
}

func (s *Store) GetSession(ws, id string) (sessionRow, error) {
	var r sessionRow
	var active int
	err := s.db.QueryRow(`SELECT workspace_id, id, created_at, is_active, metadata, configuration FROM sessions WHERE workspace_id=? AND id=?`, ws, id).
		Scan(&r.WS, &r.ID, &r.CreatedAt, &active, &r.Metadata, &r.Configuration)
	r.IsActive = active != 0
	return r, err
}

func (s *Store) UpdateSession(ws, id, metadata, configuration string) (sessionRow, error) {
	if _, err := s.EnsureSession(ws, id, "{}"); err != nil {
		return sessionRow{}, err
	}
	if strings.TrimSpace(metadata) != "" {
		if _, err := s.db.Exec(`UPDATE sessions SET metadata=? WHERE workspace_id=? AND id=?`, normJSON(metadata), ws, id); err != nil {
			return sessionRow{}, err
		}
	}
	if strings.TrimSpace(configuration) != "" {
		if _, err := s.db.Exec(`UPDATE sessions SET configuration=? WHERE workspace_id=? AND id=?`, normJSON(configuration), ws, id); err != nil {
			return sessionRow{}, err
		}
	}
	return s.GetSession(ws, id)
}

func (s *Store) DeleteSession(ws, id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// セッション削除は派生データも回収する（レビュー指摘）: summaries は (ws, session_id) 主キー、
	// conclusions は session_id スコープのもののみ（他セッション由来の結論は残す）。
	for _, q := range []string{
		`DELETE FROM messages WHERE workspace_id=? AND session_id=?`,
		`DELETE FROM session_peers WHERE workspace_id=? AND session_id=?`,
		`DELETE FROM summaries WHERE workspace_id=? AND session_id=?`,
		`DELETE FROM conclusions WHERE workspace_id=? AND session_id=?`,
		`DELETE FROM sessions WHERE workspace_id=? AND id=?`,
	} {
		if _, err := tx.Exec(q, ws, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) AddSessionPeer(ws, sid, pid string, observeMe, observeOthers *bool) error {
	b := func(v *bool) any {
		if v == nil {
			return nil
		}
		if *v {
			return 1
		}
		return 0
	}
	_, err := s.db.Exec(`INSERT INTO session_peers(workspace_id, session_id, peer_id, observe_me, observe_others) VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(workspace_id, session_id, peer_id) DO UPDATE SET
		  observe_me=COALESCE(excluded.observe_me, session_peers.observe_me),
		  observe_others=COALESCE(excluded.observe_others, session_peers.observe_others)`,
		ws, sid, pid, b(observeMe), b(observeOthers))
	return err
}

func (s *Store) SessionPeerConfig(ws, sid, pid string) (*bool, *bool, error) {
	var om, oo sql.NullInt64
	err := s.db.QueryRow(`SELECT observe_me, observe_others FROM session_peers WHERE workspace_id=? AND session_id=? AND peer_id=?`, ws, sid, pid).
		Scan(&om, &oo)
	if err == sql.ErrNoRows {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	conv := func(v sql.NullInt64) *bool {
		if !v.Valid {
			return nil
		}
		b := v.Int64 != 0
		return &b
	}
	return conv(om), conv(oo), nil
}

func (s *Store) listSessionsQuery(where string, args []any, page, size int) ([]sessionRow, int64, error) {
	var total int64
	countQ := `SELECT COUNT(*) FROM sessions ` + where
	if err := s.db.QueryRow(countQ, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	q := `SELECT workspace_id, id, created_at, is_active, metadata, configuration FROM sessions ` + where +
		` ORDER BY created_at DESC LIMIT ? OFFSET ?`
	args = append(args, size, (page-1)*size)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []sessionRow
	for rows.Next() {
		var r sessionRow
		var active int
		if err := rows.Scan(&r.WS, &r.ID, &r.CreatedAt, &active, &r.Metadata, &r.Configuration); err != nil {
			return nil, 0, err
		}
		r.IsActive = active != 0
		out = append(out, r)
	}
	return out, total, rows.Err()
}

func (s *Store) ListSessionsForPeer(ws, pid string, page, size int) ([]sessionRow, int64, error) {
	where := `WHERE workspace_id=? AND id IN (SELECT session_id FROM session_peers WHERE workspace_id=? AND peer_id=?)`
	return s.listSessionsQuery(where, []any{ws, ws, pid}, page, size)
}

func (s *Store) ListSessions(ws string, page, size int) ([]sessionRow, int64, error) {
	return s.listSessionsQuery(`WHERE workspace_id=?`, []any{ws}, page, size)
}

// ---------- messages ----------

type messageCreate struct {
	Content       string          `json:"content"`
	PeerID        string          `json:"peer_id"`
	CreatedAt     *string         `json:"created_at"`
	Configuration json.RawMessage `json:"configuration"`
	Metadata      json.RawMessage `json:"metadata"`
}

func (s *Store) AddMessage(ws, sid, pid, content, createdAt, metadata string) (messageRow, error) {
	if _, err := s.EnsureSession(ws, sid, "{}"); err != nil {
		return messageRow{}, err
	}
	if _, err := s.EnsurePeer(ws, pid, "{}"); err != nil {
		return messageRow{}, err
	}
	if err := s.AddSessionPeer(ws, sid, pid, nil, nil); err != nil {
		return messageRow{}, err
	}
	r := messageRow{
		ID:         newID(),
		WS:         ws,
		SessionID:  sid,
		PeerID:     pid,
		CreatedAt:  normTime(createdAt),
		Content:    content,
		TokenCount: countTokens(content),
		Metadata:   normJSON(metadata),
	}
	_, err := s.db.Exec(`INSERT INTO messages(id, workspace_id, session_id, peer_id, created_at, content, token_count, metadata) VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.WS, r.SessionID, r.PeerID, r.CreatedAt, r.Content, r.TokenCount, r.Metadata)
	return r, err
}

func (s *Store) ListMessages(ws, sid string, page, size int, reverse bool, peerFilter string) ([]messageRow, int64, error) {
	where := `WHERE workspace_id=? AND session_id=?`
	args := []any{ws, sid}
	if peerFilter != "" {
		where += ` AND peer_id=?`
		args = append(args, peerFilter)
	}
	var total int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	order := "ASC"
	if reverse {
		order = "DESC"
	}
	q := `SELECT id, workspace_id, session_id, peer_id, created_at, content, token_count, metadata FROM messages ` + where +
		` ORDER BY created_at ` + order + `, rowid ` + order + ` LIMIT ? OFFSET ?`
	args = append(args, size, (page-1)*size)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []messageRow
	for rows.Next() {
		var r messageRow
		if err := rows.Scan(&r.ID, &r.WS, &r.SessionID, &r.PeerID, &r.CreatedAt, &r.Content, &r.TokenCount, &r.Metadata); err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}

// MessagesForContext returns chronological messages fitting the token budget (newest kept).
func (s *Store) MessagesForContext(ws, sid string, budget int) ([]messageRow, error) {
	rows, err := s.db.Query(`SELECT id, workspace_id, session_id, peer_id, created_at, content, token_count, metadata FROM messages
		WHERE workspace_id=? AND session_id=? ORDER BY created_at DESC, rowid DESC LIMIT 2000`, ws, sid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rev []messageRow
	used := 0
	for rows.Next() {
		var r messageRow
		if err := rows.Scan(&r.ID, &r.WS, &r.SessionID, &r.PeerID, &r.CreatedAt, &r.Content, &r.TokenCount, &r.Metadata); err != nil {
			return nil, err
		}
		if budget > 0 && used+r.TokenCount > budget && len(rev) > 0 {
			break
		}
		used += r.TokenCount
		rev = append(rev, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]messageRow, 0, len(rev))
	for i := len(rev) - 1; i >= 0; i-- {
		out = append(out, rev[i])
	}
	return out, nil
}

// SearchMessages: naive LIKE search over message content (fallback tier;
// the ranked two-stage search — FTS5 candidates + judge re-ranking — lives in search.go).
func (s *Store) SearchMessages(ws, sid, pid, query string, limit int) ([]messageRow, error) {
	where := `WHERE workspace_id=? AND content LIKE ? ESCAPE '\'`
	args := []any{ws, likePattern(query)}
	if sid != "" {
		where += ` AND session_id=?`
		args = append(args, sid)
	}
	if pid != "" {
		where += ` AND peer_id=?`
		args = append(args, pid)
	}
	args = append(args, limit)
	rows, err := s.db.Query(`SELECT id, workspace_id, session_id, peer_id, created_at, content, token_count, metadata FROM messages `+where+
		` ORDER BY created_at DESC, rowid DESC LIMIT ?`, args...)
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

func likePattern(q string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(q) + "%"
}

// ---------- conclusions ----------

func (s *Store) CreateConclusion(ws, observer, observed, content, level string, sessionID *string) (conclusionRow, error) {
	r := conclusionRow{
		ID:         newID(),
		WS:         ws,
		ObserverID: observer,
		ObservedID: observed,
		CreatedAt:  nowISO(),
		Content:    content,
		Level:      level,
	}
	if r.Level == "" {
		r.Level = "explicit"
	}
	if sessionID != nil {
		r.SessionID = sql.NullString{String: *sessionID, Valid: true}
	}
	_, err := s.db.Exec(`INSERT INTO conclusions(id, workspace_id, observer_id, observed_id, session_id, content, level, created_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.WS, r.ObserverID, r.ObservedID, r.SessionID, r.Content, r.Level, r.CreatedAt)
	return r, err
}

func (s *Store) ListConclusions(ws, observer, observed string, page, size int, reverse bool) ([]conclusionRow, int64, error) {
	where := `WHERE workspace_id=?`
	args := []any{ws}
	if observer != "" {
		where += ` AND observer_id=?`
		args = append(args, observer)
	}
	if observed != "" {
		where += ` AND observed_id=?`
		args = append(args, observed)
	}
	var total int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM conclusions `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	order := "DESC"
	if reverse {
		order = "ASC"
	}
	q := `SELECT id, workspace_id, observer_id, observed_id, session_id, content, level, created_at FROM conclusions ` + where +
		` ORDER BY created_at ` + order + `, rowid ` + order + ` LIMIT ? OFFSET ?`
	args = append(args, size, (page-1)*size)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []conclusionRow
	for rows.Next() {
		var r conclusionRow
		if err := rows.Scan(&r.ID, &r.WS, &r.ObserverID, &r.ObservedID, &r.SessionID, &r.Content, &r.Level, &r.CreatedAt); err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}

func (s *Store) QueryConclusions(ws, observer, observed, query string, topK int) ([]conclusionRow, error) {
	where := `WHERE workspace_id=?`
	args := []any{ws}
	if observer != "" {
		where += ` AND observer_id=?`
		args = append(args, observer)
	}
	if observed != "" {
		where += ` AND observed_id=?`
		args = append(args, observed)
	}
	if query != "" {
		where += ` AND content LIKE ? ESCAPE '\'`
		args = append(args, likePattern(query))
	}
	args = append(args, topK)
	rows, err := s.db.Query(`SELECT id, workspace_id, observer_id, observed_id, session_id, content, level, created_at FROM conclusions `+where+
		` ORDER BY created_at DESC, rowid DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []conclusionRow
	for rows.Next() {
		var r conclusionRow
		if err := rows.Scan(&r.ID, &r.WS, &r.ObserverID, &r.ObservedID, &r.SessionID, &r.Content, &r.Level, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) DeleteConclusion(ws, id string) error {
	_, err := s.db.Exec(`DELETE FROM conclusions WHERE workspace_id=? AND id=?`, ws, id)
	return err
}

// RecentConclusions feeds the naive P1 representations.
func (s *Store) RecentConclusions(ws, observer, observed string, limit int, like string) ([]conclusionRow, error) {
	where := `WHERE workspace_id=? AND observer_id=? AND observed_id=?`
	args := []any{ws, observer, observed}
	if like != "" {
		where += ` AND content LIKE ? ESCAPE '\'`
		args = append(args, likePattern(like))
	}
	args = append(args, limit)
	rows, err := s.db.Query(`SELECT id, workspace_id, observer_id, observed_id, session_id, content, level, created_at FROM conclusions `+where+
		` ORDER BY created_at DESC, rowid DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []conclusionRow
	for rows.Next() {
		var r conclusionRow
		if err := rows.Scan(&r.ID, &r.WS, &r.ObserverID, &r.ObservedID, &r.SessionID, &r.Content, &r.Level, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ObservedAny: conclusions about a target from any observer (omniscient view).
func (s *Store) ObservedAny(ws, observed string, limit int, like string) ([]conclusionRow, error) {
	where := `WHERE workspace_id=? AND observed_id=?`
	args := []any{ws, observed}
	if like != "" {
		where += ` AND content LIKE ? ESCAPE '\'`
		args = append(args, likePattern(like))
	}
	args = append(args, limit)
	rows, err := s.db.Query(`SELECT id, workspace_id, observer_id, observed_id, session_id, content, level, created_at FROM conclusions `+where+
		` ORDER BY created_at DESC, rowid DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []conclusionRow
	for rows.Next() {
		var r conclusionRow
		if err := rows.Scan(&r.ID, &r.WS, &r.ObserverID, &r.ObservedID, &r.SessionID, &r.Content, &r.Level, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------- cards ----------

func (s *Store) GetCard(ws, observer, target string) ([]string, bool, error) {
	var raw string
	err := s.db.QueryRow(`SELECT card FROM cards WHERE workspace_id=? AND observer_id=? AND target_id=?`, ws, observer, target).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var card []string
	if err := json.Unmarshal([]byte(raw), &card); err != nil {
		return nil, false, nil
	}
	return card, true, nil
}

func (s *Store) SetCard(ws, observer, target string, card []string) error {
	rawb, err := json.Marshal(card)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO cards(workspace_id, observer_id, target_id, card, updated_at) VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(workspace_id, observer_id, target_id) DO UPDATE SET card=excluded.card, updated_at=excluded.updated_at`,
		ws, observer, target, string(rawb), nowISO())
	return err
}

// ---------- misc ----------

func (s *Store) Counts() (map[string]int64, error) {
	out := map[string]int64{}
	for _, t := range []string{"workspaces", "peers", "sessions", "session_peers", "messages", "conclusions", "cards"} {
		var n int64
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + t).Scan(&n); err != nil {
			return nil, err
		}
		out[t] = n
	}
	return out, nil
}
