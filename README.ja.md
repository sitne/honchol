# honchol

**日本語** | [English](README.md)

**軽量・セルフホストのHoncho互換メモリサーバ——Go単一バイナリ + SQLiteファイル1つ。**

honchol は [Honcho](https://github.com/plastic-labs/honcho) v3 HTTP API の実用サブセットを実装したサーバ。
既存クライアント（honcho-ai SDK、エージェントのメモリプラグイン）は `base_url` を1行変えるだけで接続できる。
Docker も Postgres も不要——静的バイナリ1つとデータベースファイル1つだけなので、
エージェントを動かす隣のCPUのみのマシンでも動く。任意のローカルPythonサイドカー（v0.3）を
併設すると、ハイブリッド検索（ONNX埋め込み + クエリ翻訳）も使える。

> 非公式プロジェクト——Plastic Labs とは無関係。
> 「Honcho」は、本サーバが API 互換性を提供している上流オープンソースプロジェクトを指す。

[![CI](https://github.com/sitne/honchol/actions/workflows/ci.yml/badge.svg)](https://github.com/sitne/honchol/actions/workflows/ci.yml)

## なぜ

上流の Honcho は Docker/Postgres 前提のデプロイ。個人がホームサーバや安価なVPSでエージェント1体を
動かすような小規模構成に、記憶のためだけのデータベースサーバは要らない。honchol はその隙間を埋める。

設計目標:

- **受信優先**: 受信メッセージは何より先に永続化する。派生処理は非同期で、リトライ可能。
- **ベクトルストア不要**: 検索は FTS 候補 + LLM judge が出発点。任意のローカル埋め込みアーム（v0.3）は RRF で融合——ANNインデックスも外部ベクトルDBも要らない。
- **すべて fail-open**: judge や LLM に到達できなくてもサーバは動き続ける（劣化はするが落ちない）。
- **可搬**: バイナリ1つ、ファイル1つ。バックアップ=ファイルをコピーするだけ。

## 特徴

- **Honcho v3 HTTP サブセット** — workspaces、peers（カード・representation・context）、sessions、
  messages、conclusions、search（workspace/session/peer）、chat、queue status。
  honcho-ai Python SDK からの実呼び出しで検証済み（`server/smoke_sdk.py` 参照）。
- **SQLite ストレージ** — WALモード、ピュアGoドライバ（`modernc.org/sqlite`）、cgo不要。
- **ハイブリッド検索** — 独立2アーム: FTS5(trigram) + LIKE 候補と、（v0.3・任意の）ローカルONNX埋め込みアーム。
  両アームは RRF（逆順位融合）で統合し、LLM judge が関連度で再ランク。候補が薄いときは長文クエリを
  キーワード展開し、日本語クエリは埋め込み前に JA→EN 翻訳できる。
- **任意のモデルサイドカー** — 小さなPythonサービス（`sidecar/`）が bekko 埋め込み + LFM2-350M JA→EN 翻訳を
  loopback HTTP で提供（CPUのみ・torch不要）。無くても literal ティアで動く（fail-open）。
- **非同期の派生パイプライン** — 未処理メッセージ → 事実抽出（クラウドLLM）→
  重複/矛盾/種別の判定 → sources 付き conclusions → カードとセッション要約。
  単一実行、dead-letter 付きリトライ。
- **交換可能な judge** — `clef`（Cloudflare Workers AI 無料枠・既定）、plain-answers プロトコルの
  判定シム（`kind=sysone`・通例は小さなローカルモデル）、または実験的なローカルONNXスコアラ（`kind=mbja`）。
  LLM は生成専用で、OpenAI互換エンドポイントなら何でも使える。
- **systemd デプロイ同梱** — serve + 15分毎の derive タイマー + 日次integrity check付き
  バックアップ（`deploy/`）。

## クイックスタート

```bash
git clone https://github.com/sitne/honchol && cd honchol/server
go build -o honchol .
./honchol serve -addr 127.0.0.1:8792 -db honchol.db
```

必要な設定（env変数。秘密は「変数名」で参照し、値は設定ファイルに書かない）:

```bash
export OPENCODE_GO_API_KEY=...            # LLMキー — HONCHO_LITE_LLM_* で任意のOpenAI互換先に向けられる
export CLOUDFLARE_API_TOKEN=... CLOUDFLARE_ACCOUNT_ID=...   # 任意: judge を有効化
```

実SDKでスモークテスト:

```bash
pip install honcho-ai
python smoke_sdk.py
```

systemd デプロイ（serve + deriveタイマー + バックアップ）は [`deploy/README.md`](deploy/README.md)、
設計の背景は [`docs/DESIGN.md`](docs/DESIGN.md) を参照。

ハイブリッド検索（任意）は `sidecar/` のPythonサイドカーが担う——CPUのみのONNX。
`HONCHO_LITE_EMBED_PROVIDER=local` + `HONCHO_LITE_FUSE=1` でサーバが接続する
（`deploy/honchol.env` に設定例あり）。

## 設定（環境変数）

| 変数 | 既定値 | 用途 |
|---|---|---|
| `HONCHO_LITE_LLM_BASE_URL` | `https://opencode.ai/zen/go/v1` | 抽出/合成用のOpenAI互換ベースURL |
| `HONCHO_LITE_LLM_MODEL` | `deepseek-v4.1-flash` | モデルID |
| `HONCHO_LITE_LLM_KEY_ENV` | `OPENCODE_GO_API_KEY` | APIキーを保持するenv変数の「名前」 |
| `HONCHO_LITE_LLM_SESSION` | `honcho-lite` | 一部ゲートウェイが要求する固定セッションヘッダ |
| `HONCHO_LITE_JUDGE_URL` | — | judgeエンドポイント（clef封筒 or sysone素形） |
| `HONCHO_LITE_JUDGE_KIND` | `clef` | `clef` \| `sysone` \| `mbja`（実験） |
| `HONCHO_LITE_JUDGE_MODEL` | `clef-flash` | `kind=clef` のときのみ送信 |
| `HONCHO_LITE_JUDGE_KEY_ENV` | `CLOUDFLARE_API_TOKEN` | キーenv名。空文字=認証なし |
| `HONCHO_LITE_JUDGE_FALLBACK_URL` | — | primary judge 失敗時に試行 |
| `HONCHO_LITE_SEARCH_JUDGE` / `_EXPAND` / `_CANDIDATE_CAP` | on / on / 40 | 二段検索の調整 |
| `HONCHO_LITE_EMBED_PROVIDER` | `none` | `none` \| `local`（サイドカー）\| `api` — 埋め込みアームを有効化 |
| `HONCHO_LITE_EMBED_URL` / `_DIM` / `_TIMEOUT_MS` | `http://127.0.0.1:8793/embed` / `384` / 5000 | `/embed` エンドポイント設定 |
| `HONCHO_LITE_EMBED_API_BASE` / `_API_MODEL` / `_API_KEY_ENV` | — / `text-embedding-3-small` / `HONCHO_LITE_EMBED_API_KEY` | provider=`api`: base URL / モデル / キーenv名 |
| `HONCHO_LITE_FUSE` | `0` | `1` = FTS + 埋め込み両アームの RRF 融合 |
| `HONCHO_LITE_TRANSLATE_PROVIDER` | `none` | `none` \| `local` — 埋め込み前の JA→EN クエリ翻訳 |
| `HONCHO_LITE_TRANSLATE_URL` / `_TIMEOUT_MS` | `http://127.0.0.1:8793/translate` / 2500 | 翻訳ステージの設定 |
| `HONCHO_LITE_QUERY_DEADLINE_MS` | 15000 | 検索1回の全体デッドライン |
| `HONCHO_LITE_DERIVE_*` | `server/README.md` 参照 | 派生パイプラインの調整 |
| `HONCHO_LITE_*_TIMEOUT_MS`（LLM / judge / search） | 90000 / 20000 / 15000 | 呼び出しタイムアウト |
| `HONCHO_LITE_MAX_BODY_BYTES` | 4194304 (4 MiB) | リクエストボディ上限 |
| `HONCHO_LITE_ALLOW_NON_LOOPBACK` | — | 非loopbackバインドに必須（認証なしのため） |

## 性能メモ

CPUのみのコンテナ（4コア・GPUなし）、DB ≈ メッセージ23k / 結論44k（~60MB）での実測:

- 結論クエリ: p50 ≈ 18 ms
- 検索（候補 + judge再ランク）: p50 ≈ 1 s
- chat 合成（クラウドLLM 1往復）: ≈ 7–12 s
- ハイブリッド検索では、融合前にローカル JA→EN 翻訳（CPUで温時 ≈0.2–0.4 s）と埋め込み参照
  （結論44k件で <50 ms）が加わる——A/B実測ではエンドツーエンドの遅延は literal ティアと同程度。

注意: 上流との比較はここでは定性的（同じデータで、移行元の上流デプロイがタイムアウトした
chat/dialectic を honchol は完了できた）。上流は意味検索、honchol は literal 起点のハイブリッド
（RRF融合・埋め込みは任意）で仕組みが違う——ベンチマークではなくデプロイメモとして扱うこと。

## スコープ / 非目標（v0.3.1）

- 埋め込み層は任意・ローカル（ONNXサイドカー）。マネージドなベクトルDBもANNインデックスも無し——
  個人規模では結論集合への総当たりコサインで十分。
- 認証なし: サーバもサイドカーもクライアントの隣で loopback 専用に動かす前提。
- シングルユーザー規模: 派生は15分毎の1パスで、マルチテナントのワーカー群ではない。
- 当面は Linux/macOS ビルド（単一実行ロックに flock を使用。Windows は WSL で）。

## ライセンス

MIT — [LICENSE](LICENSE) 参照。独立実装（上流Honchoのコードは不使用）。
API互換性はクライアント相互運用のための意図的な設計。
