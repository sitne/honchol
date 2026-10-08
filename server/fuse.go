package main

// v0.3b: FTS/embedding 独立2アーム + RRF 融合（conclusions 検索）。
// HONCHO_LITE_FUSE=1 かつ embed provider 有効時に rankedSearchConclusions から使われる。
// 片側アーム障害時は fail-soft（embedding 落ち → FTS のみ / FTS 落ち → v1 経路へ）。

import (
	"context"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type vecCache struct {
	mu       sync.Mutex
	rows     []VectorRow
	count    int64
	stamp    int64
	loadedAt time.Time
}

// cachedVectors — vectors 全件キャッシュ（件数+max rowid が変わったら再ロード。
// rowid も見るのは「件数不変の再埋め込み」を検知するため — レビュー指摘）。
func (a *apiServer) cachedVectors() ([]VectorRow, error) {
	n, stamp, err := a.st.VectorsAgg("conclusion")
	if err != nil {
		return nil, err
	}
	a.vc.mu.Lock()
	defer a.vc.mu.Unlock()
	if a.vc.rows != nil && a.vc.count == n && a.vc.stamp == stamp {
		return a.vc.rows, nil
	}
	rows, err := a.st.LoadVectors("conclusion")
	if err != nil {
		return nil, err
	}
	a.vc.rows = rows
	a.vc.count = n
	a.vc.stamp = stamp
	a.vc.loadedAt = time.Now()
	log.Printf("fuse: loaded %d conclusion vectors", len(rows))
	return rows, nil
}

// ftsArmIDs — FTS アーム: literal 系候補のランク付き id 列。
func (a *apiServer) ftsArmIDs(ws, observer, observed, query string, limit int) ([]string, error) {
	rows, err := a.st.SearchConclusionCandidates(ws, observer, observed, query, limit)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	return ids, nil
}

// embArmIDs — embedding アーム: コサイン上位 limit 件の id 列（正規化済み総当たり）。
func (a *apiServer) embArmIDs(ctx context.Context, query string, limit int) ([]string, error) {
	rows, err := a.cachedVectors()
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	qv, err := a.ep.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	if len(qv) == 0 {
		return nil, nil
	}
	q := qv[0]
	type sc struct {
		id string
		s  float32
	}
	scores := make([]sc, 0, len(rows))
	for _, r := range rows {
		if len(r.Vec) != len(q) {
			continue
		}
		var dot float32
		for i := range q {
			dot += q[i] * r.Vec[i]
		}
		scores = append(scores, sc{r.ID, dot})
	}
	sort.Slice(scores, func(i, j int) bool {
		if scores[i].s != scores[j].s {
			return scores[i].s > scores[j].s
		}
		return scores[i].id < scores[j].id
	})
	if limit > len(scores) {
		limit = len(scores)
	}
	out := make([]string, limit)
	for i := range out {
		out[i] = scores[i].id
	}
	return out, nil
}

// fetchConclusionsByIDs — id 列の conclusions を順序保持で取得（ws/observer/observed フィルタ付き）。
func (a *apiServer) fetchConclusionsByIDs(ws, observer, observed string, ids []string) ([]conclusionRow, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	where := `c.workspace_id=?`
	args := []any{ws}
	if observer != "" {
		where += ` AND c.observer_id=?`
		args = append(args, observer)
	}
	if observed != "" {
		where += ` AND c.observed_id=?`
		args = append(args, observed)
	}
	ph := make([]string, len(ids))
	for i, id := range ids {
		ph[i] = "?"
		args = append(args, id)
	}
	q := `SELECT ` + conclCols + ` FROM conclusions c WHERE ` + where + ` AND c.id IN (` + strings.Join(ph, ",") + `)`
	rows, err := a.st.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byID := map[string]conclusionRow{}
	for rows.Next() {
		var r conclusionRow
		if err := rows.Scan(&r.ID, &r.WS, &r.ObserverID, &r.ObservedID, &r.SessionID, &r.Content, &r.Level, &r.CreatedAt); err != nil {
			return nil, err
		}
		byID[r.ID] = r
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]conclusionRow, 0, len(ids))
	for _, id := range ids {
		if r, ok := byID[id]; ok {
			out = append(out, r)
		}
	}
	return out, nil
}

// rrfFuse — ランク列の RRF 融合（k=60）。同点は id 昇順（決定性）。
func rrfFuse(arms [][]string, k float64, topN int) []string {
	score := map[string]float64{}
	for _, arm := range arms {
		for i, id := range arm {
			score[id] += 1 / (k + float64(i+1))
		}
	}
	type pair struct {
		id string
		s  float64
	}
	list := make([]pair, 0, len(score))
	for id, s := range score {
		list = append(list, pair{id, s})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].s != list[j].s {
			return list[i].s > list[j].s
		}
		return list[i].id < list[j].id
	})
	if topN > len(list) {
		topN = len(list)
	}
	out := make([]string, topN)
	for i := 0; i < topN; i++ {
		out[i] = list[i].id
	}
	return out
}

func appendUniq(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	for _, s := range a {
		seen[s] = true
	}
	for _, s := range b {
		if !seen[s] {
			seen[s] = true
			a = append(a, s)
		}
	}
	return a
}

// rankedSearchConclusionsFused — v0.3b 経路: 2アーム → RRF → judge。
// ok=false は「v1 経路へ委ねる」（候補段のハード障害時のみ）。
func (a *apiServer) rankedSearchConclusionsFused(ctx context.Context, ws, observer, target, query string, limit int) ([]conclusionRow, bool) {
	armN := envInt("HONCHO_LITE_FUSE_ARM_N", 200)
	topN := envInt("HONCHO_LITE_FUSE_TOP_N", 30)

	// アーム1: FTS/LIKE。僅少なら展開で補完（v1 と同じ挙動・fail-soft）。
	ftsIDs, err := a.ftsArmIDs(ws, observer, target, query, armN)
	if err != nil {
		log.Printf("search(concl): fuse fts arm failed (%v)", err)
		return nil, false
	}
	if len(ftsIDs) < 2 && strings.TrimSpace(query) != "" {
		for _, kw := range a.expandQuery(ctx, query) {
			more, err := a.ftsArmIDs(ws, observer, target, kw, armN)
			if err != nil {
				continue
			}
			ftsIDs = appendUniq(ftsIDs, more)
		}
	}

	// v0.3c: 翻訳前段 — embedding アームは翻訳後クエリ（FTS/judge は原文を維持）。
	embQuery := query
	if tq, ok := a.translateQuery(ctx, query); ok {
		embQuery = tq
	}

	// アーム2: embedding（fail-soft: 落ちたら FTS 単独で融合 = 順序維持）。
	embIDs, err := a.embArmIDs(ctx, embQuery, armN)
	if err != nil {
		log.Printf("search(concl): fuse emb arm failed (%v) — fts only", err)
		embIDs = nil
	}

	fused := rrfFuse([][]string{ftsIDs, embIDs}, 60, topN)
	if len(fused) == 0 {
		return nil, true
	}
	rows, err := a.fetchConclusionsByIDs(ws, observer, target, fused)
	if err != nil {
		log.Printf("search(concl): fuse fetch failed (%v)", err)
		return nil, false
	}
	if len(rows) == 0 {
		return nil, true
	}
	if a.jc == nil || os.Getenv("HONCHO_LITE_SEARCH_JUDGE") == "0" {
		return clipConclusions(rows, limit), true
	}
	jctx, cancel := context.WithTimeout(ctx, judgeSearchTimeout())
	defer cancel()
	texts := make([]string, len(rows))
	for i := range rows {
		texts[i] = rows[i].Content
	}
	tiers, meta, err := judgeRelevance(jctx, a.jc, query, texts)
	if err != nil {
		log.Printf("search(concl): fuse judge failed (%v) — fused order", err)
		return clipConclusions(rows, limit), true
	}
	idx := make([]int, len(rows))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(x, y int) bool { return tiers[idx[x]] > tiers[idx[y]] })
	out := make([]conclusionRow, 0, limit)
	for _, i := range idx {
		if tiers[i] == tierUnrelated {
			continue
		}
		out = append(out, rows[i])
		if len(out) >= limit {
			break
		}
	}
	log.Printf("search(concl): fused fts=%d emb=%d -> top%d -> kept %d (meta=%s)", len(ftsIDs), len(embIDs), len(fused), len(out), meta)
	return out, true
}
