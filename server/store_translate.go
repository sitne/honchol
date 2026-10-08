package main

// v0.3c: translate_cache（クエリ翻訳のキャッシュ）アクセス。

import "database/sql"

// GetTranslateCache — src_hash に対する翻訳結果（あれば）。ok=false は未キャッシュ。
func (s *Store) GetTranslateCache(srcHash string) (string, bool, error) {
	var dst string
	err := s.db.QueryRow(`SELECT dst FROM translate_cache WHERE src_hash=?`, srcHash).Scan(&dst)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return dst, true, nil
}

// PutTranslateCache — 翻訳結果の upsert（冪等）。
func (s *Store) PutTranslateCache(srcHash, src, dst, provider string) error {
	_, err := s.db.Exec(
		`INSERT INTO translate_cache(src_hash, src, dst, provider, created_at) VALUES(?, ?, ?, ?, ?)
		 ON CONFLICT(src_hash) DO UPDATE SET dst=excluded.dst, provider=excluded.provider, created_at=excluded.created_at`,
		srcHash, src, dst, provider, nowISO(),
	)
	return err
}
