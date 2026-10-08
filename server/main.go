// honchol — a lightweight, Honcho-compatible memory server
//
// A single-binary, SQLite-backed subset of the Honcho v3 HTTP API.
// Designed as a drop-in base_url target for honcho-ai SDK clients
// (agent memory plugins, migration scripts, etc.).
//
// Commands:
//
//	serve   [-addr :8788] [-db path]                  start the HTTP server
//	doctor  [-db path] [-judge]                       store health/counts (optionally probe judge)
//	derive  [-db path] [-session ID] [-requeue-dead]  run one derivation pass
//	import  -src export.db [-ws id] [-db path]        bulk import from an export db
//	fts-rebuild [-db path]                            rebuild FTS5 indexes
//	embed-backfill [-db path] [-batch N] [-limit N]   backfill conclusion embeddings (needs HONCHO_LITE_EMBED_*)
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// version is a var (not const) so release builds can override it:
//
//	go build -ldflags "-X main.version=0.3.1"
var version = "0.3.1"

var usage = `honchol — lightweight Honcho-compatible memory server ` + version + `

usage: honchol <command> [flags]

commands:
  serve   -addr 127.0.0.1:8788 -db honchol.db   start HTTP server (Honcho v3 subset)
  doctor  -db honchol.db [-judge]               store health check + counts (optionally probe judge)
  derive  -db honchol.db [-session ID] [-requeue-dead]
                                                run one derive pass; -requeue-dead resets dead-letters
  fts-rebuild -db honchol.db                    rebuild FTS5 indexes from content tables
  embed-backfill -db honchol.db [-batch 256] [-limit N]
                                                embed missing conclusion vectors (needs HONCHO_LITE_EMBED_*)
  import  -src export.db [-ws id] [-db path]    bulk import from an export database
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		cmdServe(os.Args[2:])
	case "doctor":
		cmdDoctor(os.Args[2:])
	case "derive":
		cmdDerive(os.Args[2:])
	case "fts-rebuild":
		cmdFTSRebuild(os.Args[2:])
	case "import":
		cmdImport(os.Args[2:])
	case "embed-backfill":
		cmdEmbedBackfill(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8788", "listen address")
	dbPath := fs.String("db", "honchol.db", "sqlite database path")
	fs.Parse(args)

	st, err := OpenStore(*dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	srv := &apiServer{st: st}
	srv.ep = NewEmbeddingProvider(EmbeddingConfigFromEnv())
	srv.fuse = srv.ep.Enabled() && envOr("HONCHO_LITE_FUSE", "0") == "1"
	if srv.ep.Enabled() {
		log.Printf("embedding arm: provider=%s dim=%d fuse=%v", srv.ep.Kind(), srv.ep.Dim(), srv.fuse)
	}
	srv.tp = newTranslateProviderFromEnv()
	if srv.tp.Enabled() {
		log.Printf("translate: provider=%s -> %s", srv.tp.Kind(), srv.tp.cfg.url)
	}
	if cfg, err := JudgeConfigFromEnv(); err == nil {
		srv.jc = NewJudgeClient(*cfg)
		fb := ""
		if cfg.Fallback != nil {
			fb = " fallback=" + jMaskURL(cfg.Fallback.URL)
		}
		log.Printf("search judge: %s %s%s", cfg.Kind, jMaskURL(cfg.URL), fb)
	} else {
		log.Printf("search judge: not configured — raw search order (%v)", err)
	}
	if lc := LLMConfigFromEnv(); os.Getenv(lc.KeyEnv) != "" {
		srv.llm = NewLLMClient(lc)
		log.Printf("llm: %s @ %s (key env %s)", lc.Model, lc.BaseURL, lc.KeyEnv)
	} else {
		log.Printf("llm: disabled — key env %s is empty", lc.KeyEnv)
	}
	log.Printf("honchol %s serving on %s (db=%s)", version, *addr, *dbPath)
	if !isLoopbackAddr(*addr) {
		if os.Getenv("HONCHO_LITE_ALLOW_NON_LOOPBACK") != "1" {
			log.Fatalf("refusing to bind non-loopback address %s: no authentication available (set HONCHO_LITE_ALLOW_NON_LOOPBACK=1 to override)", *addr)
		}
		log.Printf("warning: %s is not a loopback address — this server has no authentication", *addr)
	}
	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      180 * time.Second, // chat は検索+LLMで最大 ~60s かかり得る
		IdleTimeout:       120 * time.Second,
	}
	// graceful shutdown（SREレビュー Should 対応）: SIGTERM/Interrupt で受付停止→既存接続を10s待つ
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		log.Printf("shutting down (signal)...")
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(sctx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}
}

// isLoopbackAddr — バインド先がループバックか。空ホスト（":8788" = 全インタフェース bind）は
// 非ループバックとして扱う（無認証サーバの全公開を防ぐ。レビュー指摘の修正）。
func isLoopbackAddr(addr string) bool {
	h := addr
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

// acquireFlock — 多重起動防止（非ブロッキング。取得できなければ ok=false）。
func acquireFlock(path string) (*os.File, bool, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, false, nil
	}
	return f, true, nil
}

func cmdDoctor(args []string) {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	dbPath := fs.String("db", "honchol.db", "sqlite database path")
	judge := fs.Bool("judge", false, "also ping the judge endpoint (HONCHO_LITE_JUDGE_* env)")
	fs.Parse(args)

	st, err := OpenStore(*dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	counts, err := st.Counts()
	if err != nil {
		log.Fatalf("counts: %v", err)
	}
	fmt.Printf("honchol doctor: db=%s ok\n", *dbPath)
	for _, k := range []string{"workspaces", "peers", "sessions", "session_peers", "messages", "conclusions", "cards", "vectors"} {
		fmt.Printf("  %-14s %d\n", k+":", counts[k])
	}
	if *judge {
		runJudgeCheck()
	}
	if v, ok, err := st.GetMeta("last_derive"); err == nil && ok {
		fmt.Printf("  %-14s %s\n", "last_derive:", v)
	}
	if tot, done, pend, dead, err := st.QueueCounts(""); err == nil {
		fmt.Printf("  %-14s total=%d completed=%d pending=%d dead_letter=%d\n", "queue:", tot, done, pend, dead)
	}
}

func cmdDerive(args []string) {
	fs := flag.NewFlagSet("derive", flag.ExitOnError)
	dbPath := fs.String("db", "honchol.db", "sqlite database path")
	session := fs.String("session", "", "only this session id")
	maxBatches := fs.Int("max-batches", 0, "safety limit (0 = env/default)")
	requeue := fs.Bool("requeue-dead", false, "reset dead-letter (processed=2) messages back to pending")
	fs.Parse(args)

	st, err := OpenStore(*dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	// 多重起動防止（SREレビュー Must① 対応）: 同一DBに derive は1プロセスのみ
	lock, ok, err := acquireFlock(*dbPath + ".derive.lock")
	if err != nil {
		log.Fatalf("derive: lock: %v", err)
	}
	if !ok {
		log.Printf("derive: another derive is running on %s — skip", *dbPath)
		return
	}
	defer func() {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
	}()

	if *requeue {
		n, err := st.RequeueDeadLetters("", *session)
		if err != nil {
			log.Fatalf("derive: requeue: %v", err)
		}
		log.Printf("derive: requeued %d dead-letter messages", n)
	}

	srv := &apiServer{st: st}
	if cfg, err := JudgeConfigFromEnv(); err == nil {
		srv.jc = NewJudgeClient(*cfg)
	} else {
		log.Printf("derive: judge not configured (%v) — D1/D2/D4 fail-open", err)
	}
	lc := LLMConfigFromEnv()
	if os.Getenv(lc.KeyEnv) == "" {
		log.Fatalf("derive: LLM key env %s is empty", lc.KeyEnv)
	}
	srv.llm = NewLLMClient(lc)

	opts := deriveOptsFromEnv()
	if *session != "" {
		opts.SessionFilter = *session
	}
	if *maxBatches > 0 {
		opts.MaxBatches = *maxBatches
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	start := time.Now()
	stats, err := srv.deriveRun(ctx, opts)
	if err != nil {
		log.Fatalf("derive: %v", err)
	}
	blob, _ := json.Marshal(stats)
	log.Printf("derive done in %s: %s", time.Since(start).Round(time.Millisecond), blob)
	rec, _ := json.Marshal(map[string]any{"at": nowISO(), "stats": stats})
	if err := st.SetMeta("last_derive", string(rec)); err != nil {
		log.Printf("derive: set meta: %v", err)
	}
	// v0.3a: 新規結論の埋め込み（provider=none の既定では no-op・fail-soft）
	if p := NewEmbeddingProvider(EmbeddingConfigFromEnv()); p.Enabled() {
		ectx, ecancel := context.WithTimeout(context.Background(), 10*time.Minute)
		edone, eerr := embedMissingConclusions(ectx, st, p, 128, 0, nil)
		ecancel()
		if eerr != nil {
			log.Printf("derive: embed-backfill soft error after %d: %v", edone, eerr)
		} else if edone > 0 {
			log.Printf("derive: embed-backfill +%d vectors", edone)
		}
	}
	fmt.Println(string(blob))
}

func cmdFTSRebuild(args []string) {
	fs := flag.NewFlagSet("fts-rebuild", flag.ExitOnError)
	dbPath := fs.String("db", "honchol.db", "sqlite database path")
	fs.Parse(args)

	st, err := OpenStore(*dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	start := time.Now()
	if err := st.RebuildFTS(); err != nil {
		log.Fatalf("fts-rebuild: %v", err)
	}
	log.Printf("fts-rebuild done in %s", time.Since(start).Round(time.Millisecond))
}
