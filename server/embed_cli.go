package main

// v0.3a: embed-backfill CLI — 既存結論の埋め込みベクトルを一括計算（再開可能）。

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func cmdEmbedBackfill(args []string) {
	fs := flag.NewFlagSet("embed-backfill", flag.ExitOnError)
	dbPath := fs.String("db", "honchol.db", "sqlite database path")
	batch := fs.Int("batch", 256, "texts per embedding request")
	limit := fs.Int("limit", 0, "max items to embed (0 = all)")
	timeoutS := fs.Int("timeout-s", 120, "per-request timeout seconds")
	fs.Parse(args)

	st, err := OpenStore(*dbPath)
	if err != nil {
		log.Fatalf("embed-backfill: open store: %v", err)
	}
	defer st.Close()

	lock, ok, err := acquireFlock(*dbPath + ".embed.lock")
	if err != nil {
		log.Fatalf("embed-backfill: lock: %v", err)
	}
	if !ok {
		log.Printf("embed-backfill: another backfill is running on %s — skip", *dbPath)
		return
	}
	defer func() {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
	}()

	cfg := EmbeddingConfigFromEnv()
	if !NewEmbeddingProvider(cfg).Enabled() {
		log.Fatalf("embed-backfill: HONCHO_LITE_EMBED_PROVIDER=%q — set `local` (sidecar) or `api` first", cfg.Kind)
	}
	cfg.Timeout = time.Duration(*timeoutS) * time.Second
	p := NewEmbeddingProvider(cfg)

	counts, err := st.Counts()
	if err != nil {
		log.Fatalf("embed-backfill: counts: %v", err)
	}
	have, err := st.VectorCount("conclusion")
	if err != nil {
		log.Fatalf("embed-backfill: vectors: %v", err)
	}
	log.Printf("embed-backfill: provider=%s dim=%d | conclusions=%d vectors=%d missing≈%d",
		cfg.Kind, cfg.Dim, counts["conclusions"], have, counts["conclusions"]-have)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	start := time.Now()
	done, err := embedMissingConclusions(ctx, st, p, *batch, *limit, func(f string, a ...any) {
		log.Printf("embed-backfill: "+f, a...)
	})
	if err != nil {
		log.Fatalf("embed-backfill: after %d: %v", done, err)
	}
	log.Printf("embed-backfill: done — %d embedded in %s", done, time.Since(start).Round(time.Millisecond))
}
