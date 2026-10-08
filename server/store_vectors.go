package main

// 検索 v0.3a: vectors テーブル（埋め込みベクトル）アクセス。

import "fmt"

// VectorRow — item 1件分の埋め込み（L2 正規化済み float32）。
type VectorRow struct {
	ID  string
	Dim int
	Vec []float32
}

// UpsertVectors — バッチ upsert（トランザクション・冪等）。
func (s *Store) UpsertVectors(itemType string, rows []VectorRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO vectors(item_type, item_id, dim, vec, updated_at) VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(item_type, item_id) DO UPDATE SET dim=excluded.dim, vec=excluded.vec, updated_at=excluded.updated_at`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	ts := nowISO()
	for _, r := range rows {
		if _, err := stmt.Exec(itemType, r.ID, r.Dim, vecToBlob(r.Vec), ts); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// VectorCount — item_type ごとのベクトル数。
func (s *Store) VectorCount(itemType string) (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT COUNT(*) FROM vectors WHERE item_type=?`, itemType).Scan(&n)
	return n, err
}

// VectorsAgg — item_type の (件数, max rowid)。全件キャッシュの無効化キー用
// （件数だけでは「件数不変の再埋め込み」を検知できない — レビュー指摘）。
func (s *Store) VectorsAgg(itemType string) (int64, int64, error) {
	var n, mx int64
	err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(MAX(rowid),0) FROM vectors WHERE item_type=?`, itemType).Scan(&n, &mx)
	return n, mx, err
}

// ConclusionsMissingVectors — ベクトル未計算の結論（rowid 順・limit 件）。
func (s *Store) ConclusionsMissingVectors(limit int) ([]conclusionRow, error) {
	rows, err := s.db.Query(`SELECT `+conclCols+` FROM conclusions c
		WHERE NOT EXISTS (SELECT 1 FROM vectors v WHERE v.item_type='conclusion' AND v.item_id=c.id)
		ORDER BY c.rowid LIMIT ?`, limit)
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

// LoadVectors — item_type 全ベクトル（RRF の embedding アーム用）。
func (s *Store) LoadVectors(itemType string) ([]VectorRow, error) {
	rows, err := s.db.Query(`SELECT item_id, dim, vec FROM vectors WHERE item_type=?`, itemType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VectorRow
	for rows.Next() {
		var (
			id  string
			dim int
			blb []byte
		)
		if err := rows.Scan(&id, &dim, &blb); err != nil {
			return nil, err
		}
		v, err := blobToVec(blb)
		if err != nil {
			return nil, fmt.Errorf("vectors %s: %w", id, err)
		}
		out = append(out, VectorRow{ID: id, Dim: dim, Vec: v})
	}
	return out, rows.Err()
}
