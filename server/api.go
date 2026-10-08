package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type apiServer struct {
	st  *Store
	jc  *JudgeClient // 判断（検索再ランク・derive）— 未設定なら nil
	llm *LLMClient   // 生成（抽出・要約・chat）— 未設定なら nil

	ep   *EmbeddingProvider // v0.3b: embedding アーム（nil = 無効）
	fuse bool               // v0.3b: RRF 融合（FTS/embedding 2アーム）有効
	vc   vecCache           // v0.3b: vectors 全件キャッシュ
	tp   *translateProvider // v0.3c: クエリ翻訳段（nil = 無効）
}

func (a *apiServer) routes() http.Handler {
	mux := http.NewServeMux()

	// workspaces
	mux.HandleFunc("POST /v3/workspaces", a.hWorkspaceEnsure)

	// peers
	mux.HandleFunc("POST /v3/workspaces/{ws}/peers", a.hPeerCreate)
	mux.HandleFunc("GET /v3/workspaces/{ws}/peers/{peer}", a.hPeerGet)
	mux.HandleFunc("PUT /v3/workspaces/{ws}/peers/{peer}", a.hPeerUpdate)
	mux.HandleFunc("GET /v3/workspaces/{ws}/peers/{peer}/context", a.hPeerContext)
	mux.HandleFunc("POST /v3/workspaces/{ws}/peers/{peer}/search", a.hPeerSearch)
	mux.HandleFunc("POST /v3/workspaces/{ws}/peers/{peer}/sessions", a.hPeerSessions)
	mux.HandleFunc("GET /v3/workspaces/{ws}/peers/{peer}/representation", a.hPeerRepresentation)
	mux.HandleFunc("POST /v3/workspaces/{ws}/peers/{peer}/representation", a.hPeerRepresentation)
	mux.HandleFunc("GET /v3/workspaces/{ws}/peers/{peer}/card", a.hPeerCardGet)
	mux.HandleFunc("PUT /v3/workspaces/{ws}/peers/{peer}/card", a.hPeerCardPut)
	mux.HandleFunc("POST /v3/workspaces/{ws}/peers/{peer}/chat", a.hChat)

	// sessions
	mux.HandleFunc("POST /v3/workspaces/{ws}/sessions", a.hSessionCreate)
	mux.HandleFunc("POST /v3/workspaces/{ws}/sessions/list", a.hSessionsList)
	mux.HandleFunc("GET /v3/workspaces/{ws}/sessions/{sid}", a.hSessionGet)
	mux.HandleFunc("PUT /v3/workspaces/{ws}/sessions/{sid}", a.hSessionUpdate)
	mux.HandleFunc("DELETE /v3/workspaces/{ws}/sessions/{sid}", a.hSessionDelete)
	mux.HandleFunc("POST /v3/workspaces/{ws}/sessions/{sid}/messages", a.hMessagesAdd)
	mux.HandleFunc("POST /v3/workspaces/{ws}/sessions/{sid}/messages/list", a.hMessagesList)
	mux.HandleFunc("GET /v3/workspaces/{ws}/sessions/{sid}/context", a.hSessionContext)
	mux.HandleFunc("POST /v3/workspaces/{ws}/sessions/{sid}/search", a.hSessionSearch)
	mux.HandleFunc("POST /v3/workspaces/{ws}/sessions/{sid}/peers", a.hSessionPeersAdd)
	mux.HandleFunc("PUT /v3/workspaces/{ws}/sessions/{sid}/peers", a.hSessionPeersAdd)
	mux.HandleFunc("GET /v3/workspaces/{ws}/sessions/{sid}/peers/{peer}/config", a.hSessionPeerConfigGet)
	mux.HandleFunc("PUT /v3/workspaces/{ws}/sessions/{sid}/peers/{peer}/config", a.hSessionPeerConfigPut)

	// workspace search + conclusions + queue
	mux.HandleFunc("POST /v3/workspaces/{ws}/search", a.hWorkspaceSearch)
	mux.HandleFunc("POST /v3/workspaces/{ws}/conclusions", a.hConclusionsCreate)
	mux.HandleFunc("POST /v3/workspaces/{ws}/conclusions/list", a.hConclusionsList)
	mux.HandleFunc("POST /v3/workspaces/{ws}/conclusions/query", a.hConclusionsQuery)
	mux.HandleFunc("DELETE /v3/workspaces/{ws}/conclusions/{cid}", a.hConclusionDelete)
	mux.HandleFunc("GET /v3/workspaces/{ws}/queue/status", a.hQueueStatus)

	mux.HandleFunc("GET /health", a.hHealth)

	mux.HandleFunc("/", a.hNotFound)
	return a.guard(mux)
}

// ---------- helpers ----------

func (a *apiServer) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		log.Printf("json encode: %v", err)
	}
}

func (a *apiServer) errJSON(w http.ResponseWriter, status int, msg string) {
	a.writeJSON(w, status, map[string]string{"detail": msg})
}

func decodeBody(r *http.Request, v any) error {
	defer r.Body.Close()
	err := json.NewDecoder(r.Body).Decode(v)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func pageParams(r *http.Request) (page, size int, reverse bool) {
	q := r.URL.Query()
	page = atoiDefault(q.Get("page"), 1)
	if page < 1 {
		page = 1
	}
	if page > 10000 {
		page = 10000 // 深い OFFSET による全表走査を防ぐ上限（レビュー指摘）
	}
	size = atoiDefault(q.Get("size"), 50)
	if size < 1 {
		size = 50
	}
	if size > 1000 {
		size = 1000
	}
	reverse = q.Get("reverse") == "true"
	return
}

func pagesOf(total int64, size int) int {
	if total <= 0 {
		return 1
	}
	return int((total + int64(size) - 1) / int64(size))
}

func (a *apiServer) hNotFound(w http.ResponseWriter, r *http.Request) {
	a.errJSON(w, http.StatusNotFound, "honcho-lite: no such route: "+r.Method+" "+r.URL.Path)
}

// ---------- JSON shapes (mirror honcho v3 responses) ----------

type wsOut struct {
	ID            string          `json:"id"`
	Metadata      json.RawMessage `json:"metadata"`
	Configuration json.RawMessage `json:"configuration"`
	CreatedAt     string          `json:"created_at"`
}

type peerOut struct {
	ID            string          `json:"id"`
	WorkspaceID   string          `json:"workspace_id"`
	CreatedAt     string          `json:"created_at"`
	Metadata      json.RawMessage `json:"metadata"`
	Configuration json.RawMessage `json:"configuration"`
}

type sessionOut struct {
	ID            string          `json:"id"`
	IsActive      bool            `json:"is_active"`
	WorkspaceID   string          `json:"workspace_id"`
	Metadata      json.RawMessage `json:"metadata"`
	Configuration json.RawMessage `json:"configuration"`
	CreatedAt     string          `json:"created_at"`
}

type messageOut struct {
	ID          string          `json:"id"`
	Content     string          `json:"content"`
	PeerID      string          `json:"peer_id"`
	SessionID   string          `json:"session_id"`
	Metadata    json.RawMessage `json:"metadata"`
	CreatedAt   string          `json:"created_at"`
	WorkspaceID string          `json:"workspace_id"`
	TokenCount  int             `json:"token_count"`
}

type conclusionOut struct {
	ID         string  `json:"id"`
	Content    string  `json:"content"`
	ObserverID string  `json:"observer_id"`
	ObservedID string  `json:"observed_id"`
	SessionID  *string `json:"session_id"`
	Level      string  `json:"level"`
	CreatedAt  string  `json:"created_at"`
}

type pageOut struct {
	Items any   `json:"items"`
	Page  int   `json:"page"`
	Size  int   `json:"size"`
	Total int64 `json:"total"`
	Pages int   `json:"pages"`
}

func peerJSON(r peerRow) peerOut {
	return peerOut{ID: r.ID, WorkspaceID: r.WS, CreatedAt: r.CreatedAt,
		Metadata: json.RawMessage(r.Metadata), Configuration: json.RawMessage(r.Configuration)}
}

func sessionJSON(r sessionRow) sessionOut {
	return sessionOut{ID: r.ID, IsActive: r.IsActive, WorkspaceID: r.WS, CreatedAt: r.CreatedAt,
		Metadata: json.RawMessage(r.Metadata), Configuration: json.RawMessage(r.Configuration)}
}

func messageJSON(r messageRow) messageOut {
	return messageOut{ID: r.ID, Content: r.Content, PeerID: r.PeerID, SessionID: r.SessionID,
		Metadata: json.RawMessage(r.Metadata), CreatedAt: r.CreatedAt, WorkspaceID: r.WS, TokenCount: r.TokenCount}
}

func conclusionJSON(r conclusionRow) conclusionOut {
	var sid *string
	if r.SessionID.Valid {
		s := r.SessionID.String
		sid = &s
	}
	return conclusionOut{ID: r.ID, Content: r.Content, ObserverID: r.ObserverID, ObservedID: r.ObservedID,
		SessionID: sid, Level: r.Level, CreatedAt: r.CreatedAt}
}

func messagesJSON(rows []messageRow) []messageOut {
	out := make([]messageOut, 0, len(rows))
	for _, r := range rows {
		out = append(out, messageJSON(r))
	}
	return out
}

func conclusionsJSON(rows []conclusionRow) []conclusionOut {
	out := make([]conclusionOut, 0, len(rows))
	for _, r := range rows {
		out = append(out, conclusionJSON(r))
	}
	return out
}

// ---------- workspaces ----------

func (a *apiServer) hWorkspaceEnsure(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if err := decodeBody(r, &body); err != nil || body.ID == "" {
		a.errJSON(w, http.StatusBadRequest, "id required")
		return
	}
	ws, err := a.st.EnsureWorkspace(body.ID)
	if err != nil {
		a.errInternal(w, err)
		return
	}
	a.writeJSON(w, http.StatusOK, wsOut{ID: ws.ID, Metadata: json.RawMessage("{}"), Configuration: json.RawMessage("{}"), CreatedAt: ws.CreatedAt})
}

// ---------- peers ----------

func (a *apiServer) hPeerCreate(w http.ResponseWriter, r *http.Request) {
	ws := r.PathValue("ws")
	var body struct {
		ID            string          `json:"id"`
		Metadata      json.RawMessage `json:"metadata"`
		Configuration json.RawMessage `json:"configuration"`
	}
	if err := decodeBody(r, &body); err != nil || body.ID == "" {
		a.errJSON(w, http.StatusBadRequest, "id required")
		return
	}
	if _, err := a.st.EnsureWorkspace(ws); err != nil {
		a.errInternal(w, err)
		return
	}
	p, err := a.st.EnsurePeer(ws, body.ID, string(body.Metadata))
	if err != nil {
		a.errInternal(w, err)
		return
	}
	if len(body.Configuration) > 0 {
		if p, err = a.st.UpdatePeer(ws, body.ID, "", string(body.Configuration)); err != nil {
			a.errInternal(w, err)
			return
		}
	}
	a.writeJSON(w, http.StatusOK, peerJSON(p))
}

func (a *apiServer) hPeerGet(w http.ResponseWriter, r *http.Request) {
	p, err := a.st.GetPeer(r.PathValue("ws"), r.PathValue("peer"))
	if err != nil {
		a.errJSON(w, http.StatusNotFound, "peer not found")
		return
	}
	a.writeJSON(w, http.StatusOK, peerJSON(p))
}

func (a *apiServer) hPeerUpdate(w http.ResponseWriter, r *http.Request) {
	ws, peer := r.PathValue("ws"), r.PathValue("peer")
	var body struct {
		Metadata      json.RawMessage `json:"metadata"`
		Configuration json.RawMessage `json:"configuration"`
	}
	if err := decodeBody(r, &body); err != nil {
		a.errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	p, err := a.st.UpdatePeer(ws, peer, string(body.Metadata), string(body.Configuration))
	if err != nil {
		a.errInternal(w, err)
		return
	}
	a.writeJSON(w, http.StatusOK, peerJSON(p))
}

// representationText — 観察者視点の記憶テキスト（箇条書き）。
// searchQuery 付きは二段検索（候補 → judge 関連判定）で選抜する。
func (a *apiServer) representationText(ctx context.Context, ws, observer, target, searchQuery string) *string {
	var rows []conclusionRow
	if searchQuery != "" {
		rows = a.rankedSearchConclusions(ctx, ws, observer, target, searchQuery, 25)
	} else {
		var err error
		if observer != "" {
			rows, err = a.st.RecentConclusions(ws, observer, target, 25, "")
		} else {
			rows, err = a.st.ObservedAny(ws, target, 25, "")
		}
		if err != nil {
			return nil
		}
	}
	if len(rows) == 0 {
		return nil
	}
	var b []byte
	for _, r := range rows {
		b = append(b, []byte("- "+truncRunes(cleanForPrompt(r.Content), 500)+"\n")...)
	}
	s := string(b)
	return &s
}

func (a *apiServer) hPeerContext(w http.ResponseWriter, r *http.Request) {
	ws, peer := r.PathValue("ws"), r.PathValue("peer")
	q := r.URL.Query()
	target := q.Get("target")
	if target == "" {
		target = peer
	}
	rep := a.representationText(r.Context(), ws, peer, target, q.Get("search_query"))
	var card any
	if c, ok, err := a.st.GetCard(ws, peer, target); err == nil && ok && len(c) > 0 {
		card = c
	}
	a.writeJSON(w, http.StatusOK, map[string]any{
		"peer_id":        peer,
		"target_id":      target,
		"representation": rep,
		"peer_card":      card,
	})
}

func (a *apiServer) hPeerSearch(w http.ResponseWriter, r *http.Request) {
	ws, peer := r.PathValue("ws"), r.PathValue("peer")
	var body struct {
		Query   string          `json:"query"`
		Filters json.RawMessage `json:"filters"`
		Limit   int             `json:"limit"`
	}
	if err := decodeBody(r, &body); err != nil || body.Query == "" {
		a.errJSON(w, http.StatusBadRequest, "query required")
		return
	}
	if len([]rune(body.Query)) > 8000 {
		a.errJSON(w, http.StatusBadRequest, "query too long (max 8000 chars)")
		return
	}
	limit := body.Limit
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	rows, _ := a.rankedSearchMessages(r.Context(), ws, "", peer, body.Query, limit)
	a.writeJSON(w, http.StatusOK, messagesJSON(rows))
}

func (a *apiServer) hPeerSessions(w http.ResponseWriter, r *http.Request) {
	page, size, _ := pageParams(r)
	rows, total, err := a.st.ListSessionsForPeer(r.PathValue("ws"), r.PathValue("peer"), page, size)
	if err != nil {
		a.errInternal(w, err)
		return
	}
	items := make([]sessionOut, 0, len(rows))
	for _, s := range rows {
		items = append(items, sessionJSON(s))
	}
	a.writeJSON(w, http.StatusOK, pageOut{Items: items, Page: page, Size: size, Total: total, Pages: pagesOf(total, size)})
}

func (a *apiServer) hPeerRepresentation(w http.ResponseWriter, r *http.Request) {
	ws, peer := r.PathValue("ws"), r.PathValue("peer")
	target, searchQuery := "", ""
	if r.Method == http.MethodPost {
		var body struct {
			Target      string `json:"target"`
			SearchQuery string `json:"search_query"`
		}
		_ = decodeBody(r, &body)
		target, searchQuery = body.Target, body.SearchQuery
	} else {
		q := r.URL.Query()
		target, searchQuery = q.Get("target"), q.Get("search_query")
	}
	if target == "" {
		target = peer
	}
	rep := a.representationText(r.Context(), ws, peer, target, searchQuery)
	text := ""
	if rep != nil {
		text = *rep
	}
	a.writeJSON(w, http.StatusOK, map[string]string{"representation": text})
}

func (a *apiServer) hPeerCardGet(w http.ResponseWriter, r *http.Request) {
	ws, peer := r.PathValue("ws"), r.PathValue("peer")
	target := r.URL.Query().Get("target")
	if target == "" {
		target = peer
	}
	var card any
	if c, ok, err := a.st.GetCard(ws, peer, target); err == nil && ok {
		card = c
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"peer_card": card})
}

func (a *apiServer) hPeerCardPut(w http.ResponseWriter, r *http.Request) {
	ws, peer := r.PathValue("ws"), r.PathValue("peer")
	target := r.URL.Query().Get("target")
	if target == "" {
		target = peer
	}
	var body struct {
		PeerCard []string `json:"peer_card"`
	}
	if err := decodeBody(r, &body); err != nil {
		a.errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.st.SetCard(ws, peer, target, body.PeerCard); err != nil {
		a.errInternal(w, err)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"peer_card": body.PeerCard})
}

// hChat — 対話層（dialectic）: 記憶を根拠にクエリへ回答する（クラウドLLM合成）。
// SDK 形状: POST {query, stream:false, target?, session_id?, reasoning_level?} -> {content}
func (a *apiServer) hChat(w http.ResponseWriter, r *http.Request) {
	ws, peer := r.PathValue("ws"), r.PathValue("peer")
	var body struct {
		Query          string `json:"query"`
		Stream         bool   `json:"stream"`
		Target         string `json:"target"`
		SessionID      string `json:"session_id"`
		ReasoningLevel string `json:"reasoning_level"`
	}
	if err := decodeBody(r, &body); err != nil || strings.TrimSpace(body.Query) == "" {
		a.errJSON(w, http.StatusBadRequest, "query required")
		return
	}
	if len([]rune(body.Query)) > 8000 {
		a.errJSON(w, http.StatusBadRequest, "query too long (max 8000 chars)")
		return
	}
	if body.Stream {
		a.errJSON(w, http.StatusNotImplemented, "chat: streaming not supported (use stream=false)")
		return
	}
	if a.llm == nil || !a.llm.HasKey() {
		a.errJSON(w, http.StatusServiceUnavailable, "chat: LLM not configured")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	chStart := time.Now()

	// 参照面: target 指定時は「peer が target について知っていること」、無指定は peer の全体表現
	ob, tg := deriveObserver, peer
	if body.Target != "" {
		ob, tg = peer, body.Target
	}
	var sections []string
	if c, ok, err := a.st.GetCard(ws, ob, tg); err == nil && ok && len(c) > 0 {
		sections = append(sections, "## カード（プロフィール）\n- "+strings.Join(c, "\n- "))
	}
	// 結論・発言の取得は独立 — 並列化（性能レビュー Should: 逐次だと 60s 予算を超え得る）
	retStart := time.Now()
	var rep *string
	var hits []messageRow
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); rep = a.representationText(ctx, ws, ob, tg, body.Query) }()
	go func() { defer wg.Done(); hits, _ = a.rankedSearchMessages(ctx, ws, body.SessionID, tg, body.Query, 8) }()
	wg.Wait()
	retrieval := time.Since(retStart)
	if rep != nil {
		sections = append(sections, "## 関連する結論\n"+*rep)
	}
	if len(hits) > 0 {
		var b strings.Builder
		b.WriteString("## 関連する発言\n")
		for _, h := range hits {
			fmt.Fprintf(&b, "- %s: %s\n", h.PeerID, truncRunes(cleanForPrompt(h.Content), 300))
		}
		sections = append(sections, b.String())
	}
	if len(sections) == 0 {
		a.writeJSON(w, http.StatusOK, map[string]any{"content": "この話題に関する記憶はまだありません。"})
		return
	}
	sys := "あなたは長期記憶システムの対話層です。与えられた記憶のみを根拠に、質問へ日本語で簡潔に答えてください。記憶に無いことは「記憶にない」と明示し、推測する場合は推測と書くこと。"
	usr := strings.Join(sections, "\n\n") + "\n\n## 質問\n" + body.Query
	answer, err := a.llm.Chat(ctx, sys, usr, 0.3)
	if err != nil {
		log.Printf("chat: llm failed: %v", err)
		a.errJSON(w, http.StatusBadGateway, "chat: upstream error")
		return
	}
	answer = strings.TrimSpace(answer)
	log.Printf("chat: ws=%s peer=%s retrieval=%.2fs total=%.2fs", ws, peer, retrieval.Seconds(), time.Since(chStart).Seconds())
	if answer == "" {
		a.writeJSON(w, http.StatusOK, map[string]any{"content": nil})
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"content": answer})
}

// ---------- sessions ----------

func (a *apiServer) hSessionCreate(w http.ResponseWriter, r *http.Request) {
	ws := r.PathValue("ws")
	var body struct {
		ID            string          `json:"id"`
		Metadata      json.RawMessage `json:"metadata"`
		Configuration json.RawMessage `json:"configuration"`
		Peers         map[string]struct {
			ObserveMe     *bool `json:"observe_me"`
			ObserveOthers *bool `json:"observe_others"`
		} `json:"peers"`
	}
	if err := decodeBody(r, &body); err != nil || body.ID == "" {
		a.errJSON(w, http.StatusBadRequest, "id required")
		return
	}
	if _, err := a.st.EnsureWorkspace(ws); err != nil {
		a.errInternal(w, err)
		return
	}
	s, err := a.st.EnsureSession(ws, body.ID, string(body.Metadata))
	if err != nil {
		a.errInternal(w, err)
		return
	}
	for pid, cfg := range body.Peers {
		if _, err := a.st.EnsurePeer(ws, pid, "{}"); err != nil {
			a.errInternal(w, err)
			return
		}
		if err := a.st.AddSessionPeer(ws, body.ID, pid, cfg.ObserveMe, cfg.ObserveOthers); err != nil {
			a.errInternal(w, err)
			return
		}
	}
	a.writeJSON(w, http.StatusOK, sessionJSON(s))
}

func (a *apiServer) hSessionsList(w http.ResponseWriter, r *http.Request) {
	page, size, _ := pageParams(r)
	rows, total, err := a.st.ListSessions(r.PathValue("ws"), page, size)
	if err != nil {
		a.errInternal(w, err)
		return
	}
	items := make([]sessionOut, 0, len(rows))
	for _, s := range rows {
		items = append(items, sessionJSON(s))
	}
	a.writeJSON(w, http.StatusOK, pageOut{Items: items, Page: page, Size: size, Total: total, Pages: pagesOf(total, size)})
}

func (a *apiServer) hSessionGet(w http.ResponseWriter, r *http.Request) {
	s, err := a.st.GetSession(r.PathValue("ws"), r.PathValue("sid"))
	if err != nil {
		a.errJSON(w, http.StatusNotFound, "session not found")
		return
	}
	a.writeJSON(w, http.StatusOK, sessionJSON(s))
}

func (a *apiServer) hSessionUpdate(w http.ResponseWriter, r *http.Request) {
	ws, sid := r.PathValue("ws"), r.PathValue("sid")
	var body struct {
		Metadata      json.RawMessage `json:"metadata"`
		Configuration json.RawMessage `json:"configuration"`
	}
	if err := decodeBody(r, &body); err != nil {
		a.errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	s, err := a.st.UpdateSession(ws, sid, string(body.Metadata), string(body.Configuration))
	if err != nil {
		a.errInternal(w, err)
		return
	}
	a.writeJSON(w, http.StatusOK, sessionJSON(s))
}

func (a *apiServer) hSessionDelete(w http.ResponseWriter, r *http.Request) {
	if err := a.st.DeleteSession(r.PathValue("ws"), r.PathValue("sid")); err != nil {
		a.errInternal(w, err)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{})
}

// ---------- messages ----------

func (a *apiServer) hMessagesAdd(w http.ResponseWriter, r *http.Request) {
	ws, sid := r.PathValue("ws"), r.PathValue("sid")
	var body struct {
		Messages []messageCreate `json:"messages"`
	}
	if err := decodeBody(r, &body); err != nil {
		a.errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(body.Messages) == 0 {
		a.errJSON(w, http.StatusBadRequest, "messages required")
		return
	}
	if len(body.Messages) > 500 {
		a.errJSON(w, http.StatusBadRequest, "too many messages (max 500 per request)")
		return
	}
	out := make([]messageOut, 0, len(body.Messages))
	for _, m := range body.Messages {
		if len([]rune(m.Content)) > 200_000 {
			a.errJSON(w, http.StatusBadRequest, "message too long (max 200000 chars)")
			return
		}
		createdAt := ""
		if m.CreatedAt != nil {
			createdAt = *m.CreatedAt
		}
		row, err := a.st.AddMessage(ws, sid, m.PeerID, m.Content, createdAt, string(m.Metadata))
		if err != nil {
			a.errInternal(w, err)
			return
		}
		out = append(out, messageJSON(row))
	}
	a.writeJSON(w, http.StatusOK, out)
}

func (a *apiServer) hMessagesList(w http.ResponseWriter, r *http.Request) {
	ws, sid := r.PathValue("ws"), r.PathValue("sid")
	page, size, reverse := pageParams(r)
	peerFilter := ""
	var body struct {
		Filters map[string]any `json:"filters"`
	}
	if err := decodeBody(r, &body); err == nil && body.Filters != nil {
		if v, ok := body.Filters["peer_id"].(string); ok {
			peerFilter = v
		}
	}
	rows, total, err := a.st.ListMessages(ws, sid, page, size, reverse, peerFilter)
	if err != nil {
		a.errInternal(w, err)
		return
	}
	a.writeJSON(w, http.StatusOK, pageOut{Items: messagesJSON(rows), Page: page, Size: size, Total: total, Pages: pagesOf(total, size)})
}

func (a *apiServer) hSessionContext(w http.ResponseWriter, r *http.Request) {
	ws, sid := r.PathValue("ws"), r.PathValue("sid")
	q := r.URL.Query()
	budget := atoiDefault(q.Get("tokens"), 8000)
	if budget <= 0 {
		budget = 8000
	}
	if budget > 100000 {
		budget = 100000 // 上限なしでは単一応答が数百MBになり得る（レビュー指摘）
	}
	rows, err := a.st.MessagesForContext(ws, sid, budget)
	if err != nil {
		a.errInternal(w, err)
		return
	}
	var rep any
	target := q.Get("peer_target")
	if target != "" {
		perspective := q.Get("peer_perspective")
		if rp := a.representationText(r.Context(), ws, perspective, target, q.Get("search_query")); rp != nil {
			rep = *rp
		}
	}
	var card any
	if target != "" {
		perspective := q.Get("peer_perspective")
		if c, ok, err := a.st.GetCard(ws, perspective, target); err == nil && ok && len(c) > 0 {
			card = c
		}
	}
	var summary any
	if s, ok, err := a.st.GetSummary(ws, sid); err == nil && ok {
		summary = s
	}
	a.writeJSON(w, http.StatusOK, map[string]any{
		"id":                  sid,
		"messages":            messagesJSON(rows),
		"summary":             summary,
		"peer_representation": rep,
		"peer_card":           card,
	})
}

func (a *apiServer) hSessionSearch(w http.ResponseWriter, r *http.Request) {
	ws, sid := r.PathValue("ws"), r.PathValue("sid")
	var body struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := decodeBody(r, &body); err != nil || body.Query == "" {
		a.errJSON(w, http.StatusBadRequest, "query required")
		return
	}
	if len([]rune(body.Query)) > 8000 {
		a.errJSON(w, http.StatusBadRequest, "query too long (max 8000 chars)")
		return
	}
	limit := body.Limit
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	rows, _ := a.rankedSearchMessages(r.Context(), ws, sid, "", body.Query, limit)
	a.writeJSON(w, http.StatusOK, messagesJSON(rows))
}

func (a *apiServer) hSessionPeersAdd(w http.ResponseWriter, r *http.Request) {
	ws, sid := r.PathValue("ws"), r.PathValue("sid")
	var body struct {
		Peers map[string]struct {
			ObserveMe     *bool `json:"observe_me"`
			ObserveOthers *bool `json:"observe_others"`
		} `json:"peers"`
	}
	if err := decodeBody(r, &body); err != nil {
		a.errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	for pid, cfg := range body.Peers {
		if _, err := a.st.EnsurePeer(ws, pid, "{}"); err != nil {
			a.errInternal(w, err)
			return
		}
		if err := a.st.AddSessionPeer(ws, sid, pid, cfg.ObserveMe, cfg.ObserveOthers); err != nil {
			a.errInternal(w, err)
			return
		}
	}
	a.writeJSON(w, http.StatusOK, map[string]any{})
}

func (a *apiServer) hSessionPeerConfigGet(w http.ResponseWriter, r *http.Request) {
	om, oo, err := a.st.SessionPeerConfig(r.PathValue("ws"), r.PathValue("sid"), r.PathValue("peer"))
	if err != nil {
		a.errInternal(w, err)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"observe_me": om, "observe_others": oo})
}

func (a *apiServer) hSessionPeerConfigPut(w http.ResponseWriter, r *http.Request) {
	ws, sid, peer := r.PathValue("ws"), r.PathValue("sid"), r.PathValue("peer")
	var body struct {
		ObserveMe     *bool `json:"observe_me"`
		ObserveOthers *bool `json:"observe_others"`
	}
	if err := decodeBody(r, &body); err != nil {
		a.errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := a.st.EnsurePeer(ws, peer, "{}"); err != nil {
		a.errInternal(w, err)
		return
	}
	if err := a.st.AddSessionPeer(ws, sid, peer, body.ObserveMe, body.ObserveOthers); err != nil {
		a.errInternal(w, err)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"observe_me": body.ObserveMe, "observe_others": body.ObserveOthers})
}

// ---------- search / conclusions / queue ----------

func (a *apiServer) hWorkspaceSearch(w http.ResponseWriter, r *http.Request) {
	ws := r.PathValue("ws")
	var body struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := decodeBody(r, &body); err != nil || body.Query == "" {
		a.errJSON(w, http.StatusBadRequest, "query required")
		return
	}
	if len([]rune(body.Query)) > 8000 {
		a.errJSON(w, http.StatusBadRequest, "query too long (max 8000 chars)")
		return
	}
	limit := body.Limit
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	rows, _ := a.rankedSearchMessages(r.Context(), ws, "", "", body.Query, limit)
	a.writeJSON(w, http.StatusOK, messagesJSON(rows))
}

func (a *apiServer) hConclusionsCreate(w http.ResponseWriter, r *http.Request) {
	ws := r.PathValue("ws")
	var body struct {
		Conclusions []struct {
			Content    string  `json:"content"`
			ObserverID string  `json:"observer_id"`
			ObservedID string  `json:"observed_id"`
			SessionID  *string `json:"session_id"`
			Level      string  `json:"level"`
		} `json:"conclusions"`
	}
	if err := decodeBody(r, &body); err != nil || len(body.Conclusions) == 0 {
		a.errJSON(w, http.StatusBadRequest, "conclusions required")
		return
	}
	if len(body.Conclusions) > 500 {
		a.errJSON(w, http.StatusBadRequest, "too many conclusions (max 500 per request)")
		return
	}
	out := make([]conclusionOut, 0, len(body.Conclusions))
	for _, c := range body.Conclusions {
		if c.ObserverID == "" || c.ObservedID == "" {
			a.errJSON(w, http.StatusBadRequest, "observer_id and observed_id required")
			return
		}
		if len([]rune(c.Content)) > 20_000 {
			a.errJSON(w, http.StatusBadRequest, "conclusion too long (max 20000 chars)")
			return
		}
		row, err := a.st.CreateConclusion(ws, c.ObserverID, c.ObservedID, c.Content, c.Level, c.SessionID)
		if err != nil {
			a.errInternal(w, err)
			return
		}
		out = append(out, conclusionJSON(row))
	}
	a.writeJSON(w, http.StatusOK, out)
}

func (a *apiServer) hConclusionsList(w http.ResponseWriter, r *http.Request) {
	ws := r.PathValue("ws")
	page, size, reverse := pageParams(r)
	var body struct {
		Filters struct {
			ObserverID string `json:"observer_id"`
			ObservedID string `json:"observed_id"`
		} `json:"filters"`
	}
	_ = decodeBody(r, &body)
	rows, total, err := a.st.ListConclusions(ws, body.Filters.ObserverID, body.Filters.ObservedID, page, size, reverse)
	if err != nil {
		a.errInternal(w, err)
		return
	}
	a.writeJSON(w, http.StatusOK, pageOut{Items: conclusionsJSON(rows), Page: page, Size: size, Total: total, Pages: pagesOf(total, size)})
}

func (a *apiServer) hConclusionsQuery(w http.ResponseWriter, r *http.Request) {
	ws := r.PathValue("ws")
	var body struct {
		Query   string `json:"query"`
		TopK    int    `json:"top_k"`
		Filters struct {
			ObserverID string `json:"observer_id"`
			ObservedID string `json:"observed_id"`
		} `json:"filters"`
	}
	if err := decodeBody(r, &body); err != nil || body.Query == "" {
		a.errJSON(w, http.StatusBadRequest, "query required")
		return
	}
	if len([]rune(body.Query)) > 8000 {
		a.errJSON(w, http.StatusBadRequest, "query too long (max 8000 chars)")
		return
	}
	topK := body.TopK
	if topK <= 0 {
		topK = 10
	}
	if topK > 100 {
		topK = 100
	}
	// 二段検索（候補FTS/LIKE → 僅少時は展開補完 → judge再ランク）。fail-open。
	rows := a.rankedSearchConclusions(r.Context(), ws, body.Filters.ObserverID, body.Filters.ObservedID, body.Query, topK)
	a.writeJSON(w, http.StatusOK, conclusionsJSON(rows))
}

func (a *apiServer) hConclusionDelete(w http.ResponseWriter, r *http.Request) {
	if err := a.st.DeleteConclusion(r.PathValue("ws"), r.PathValue("cid")); err != nil {
		a.errInternal(w, err)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{})
}

func (a *apiServer) hQueueStatus(w http.ResponseWriter, r *http.Request) {
	ws := r.PathValue("ws")
	total, completed, pending, dead, err := a.st.QueueCounts(ws)
	if err != nil {
		a.errInternal(w, err)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]int{
		"total_work_units":       total,
		"completed_work_units":   completed,
		"in_progress_work_units": 0,
		"pending_work_units":     pending,
		"dead_letter_work_units": dead,
	})
}

// ---------- ローカル防壁（Host/Origin/ボディ上限） ----------

// hHealth — 運用監視（非SDK・curl用）: queue 深さ / 最終 derive / FTS 状態。
// dead_letter 滞留または last_derive の 6h 超陳腐化で status=degraded（SREレビュー Should 対応）。
func (a *apiServer) hHealth(w http.ResponseWriter, r *http.Request) {
	tot, done, pend, dead, err := a.st.QueueCounts("")
	if err != nil {
		a.errInternal(w, err)
		return
	}
	last, _, _ := a.st.GetMeta("last_derive")
	status := "ok"
	staleDerive := false
	if last != "" {
		var rec struct {
			At string `json:"at"`
		}
		if json.Unmarshal([]byte(last), &rec) == nil && rec.At != "" {
			if t, err := time.Parse(timeFormat, rec.At); err == nil && time.Since(t) > 6*time.Hour {
				staleDerive = true
			}
		}
	}
	if dead > 0 || staleDerive {
		status = "degraded"
	}
	a.writeJSON(w, http.StatusOK, map[string]any{
		"status":       status,
		"stale_derive": staleDerive,
		"version":      version,
		"fts_ok":       a.st.ftsOK,
		"fts_trigram":  a.st.ftsTrigram,
		"queue": map[string]int{
			"total":       tot,
			"completed":   done,
			"pending":     pend,
			"dead_letter": dead,
		},
		"last_derive": last,
	})
}

// errInternal — 5xx 用: 詳細はサーバログのみ、クライアントには定型文（内部情報の露出防止）。
func (a *apiServer) errInternal(w http.ResponseWriter, err error) {
	log.Printf("internal error: %v", err)
	a.errJSON(w, http.StatusInternalServerError, "internal error")
}

// guard — セキュリティレビュー Should 対応:
//   - Host 検証: DNS リバインディングで外部サイトから 127.0.0.1 に到達されるのを拒否
//   - Origin 検証: ブラウザ発クロスサイト POST（blind POST で課金LLMを叩く等）を拒否
//   - ボディ上限: 巨大リクエストでメモリ/DB/課金LLMを焼かない（既定 4MiB）
func (a *apiServer) guard(next http.Handler) http.Handler {
	maxBody := int64(envInt("HONCHO_LITE_MAX_BODY_BYTES", 4<<20))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hostAllowed(r.Host) {
			a.errJSON(w, http.StatusForbidden, "forbidden host")
			return
		}
		if o := r.Header.Get("Origin"); o != "" && !originAllowed(o) {
			a.errJSON(w, http.StatusForbidden, "forbidden origin")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		}
		next.ServeHTTP(w, r)
	})
}

// hostAllowed — ホスト名（ポート可）がループバック名か。
func hostAllowed(host string) bool {
	h := strings.TrimSpace(host)
	if strings.HasPrefix(h, "[") {
		if i := strings.Index(h, "]"); i >= 0 {
			h = h[1:i]
		}
	} else if i := strings.LastIndex(h, ":"); i >= 0 {
		h = h[:i]
	}
	switch h {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

// originAllowed — Origin のホスト部がループバック名か。
func originAllowed(origin string) bool {
	rest := origin
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	return hostAllowed(rest)
}
