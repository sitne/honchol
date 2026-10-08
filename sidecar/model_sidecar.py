#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""honchol model sidecar — local HTTP service for bekko embeddings + LFM2-ENJP-MT translation.

起動例:  python3 model_sidecar.py   (SIDECAR_PORT=8793; venv 推奨)
systemd: deploy/honchol-sidecar.service

Endpoints:
  GET  /health    -> {"ok": true, "embed": {...}, "translate": {...}, "uptime_s": ...}
  POST /embed     -> {"texts": ["..."]} => {"vectors": [[...]], "dim": 384, "model": "...", "ms": ...}
  POST /translate -> {"texts": ["..."], "max_new_tokens": 48} => {"translations": ["..."], "model": "...", "ms": ...}

注意: loopback 専用（認証なし）。Host/Origin ガード + 同時実行スロット（503）を内蔵。
bekko は mean-pooling + L2 正規化。翻訳は LFM2-350M-ENJP-MT（onnx-community quantized）— greedy デコード + KV キャッシュ。
bekko は初回起動時に HF Hub から固定リビジョンを取得（SIDECAR_BEKKO_DIR 指定時はそのローカルパスを使用）。
"""
import json
import os
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import numpy as np
import onnxruntime as ort
from transformers import AutoTokenizer

os.environ.setdefault("HF_HUB_DISABLE_XET", "1")  # Xet 停止問題の回避（実測）

BEKKO_REPO = os.environ.get("SIDECAR_BEKKO_REPO", "hotchpotch/bekko-embedding-v1-a8m")
BEKKO_REVISION = os.environ.get("SIDECAR_BEKKO_REVISION", "c721113d59a1d91b447450324f51c4b3332c924a")
SNAP = os.environ.get("SIDECAR_BEKKO_DIR", "")


def resolve_bekko_dir():
    """bekko のローカルディレクトリを解決。未取得なら HF Hub から固定リビジョンを取得。"""
    if SNAP:
        return SNAP
    from huggingface_hub import snapshot_download

    return snapshot_download(BEKKO_REPO, revision=BEKKO_REVISION)
MODEL_NAME = os.environ.get("SIDECAR_BEKKO_NAME", "bekko-embedding-v1-a8m")
PORT = int(os.environ.get("SIDECAR_PORT", "8793"))
BATCH = int(os.environ.get("SIDECAR_BATCH", "64"))  # ORT に渡すサブバッチ（attention メモリ保護）
MAXLEN = int(os.environ.get("SIDECAR_MAXLEN", "512"))
MAX_TEXTS = int(os.environ.get("SIDECAR_MAX_TEXTS", "512"))
MAX_BODY = int(os.environ.get("SIDECAR_MAX_BODY_BYTES", str(8 << 20)))
EMBED_THREADS = int(os.environ.get("SIDECAR_EMBED_THREADS", "2"))
MAX_EMBED_CHARS = int(os.environ.get("SIDECAR_MAX_TEXT_CHARS", "20000"))
MAX_TRANSLATE_CHARS = int(os.environ.get("SIDECAR_MAX_TRANSLATE_CHARS", "2000"))
TRANSLATE_MAX_NEW_CAP = int(os.environ.get("SIDECAR_TRANSLATE_MAX_NEW_CAP", "128"))
MAX_CONCURRENCY = int(os.environ.get("SIDECAR_MAX_CONCURRENCY", "8"))

LFM2_DIR = os.environ.get("SIDECAR_LFM2_DIR", os.path.expanduser("~/models/lfm2-enjp-mt-onnx"))
LFM2_NAME = os.environ.get("SIDECAR_LFM2_NAME", "LFM2-350M-ENJP-MT")
LFM2_THREADS = int(os.environ.get("SIDECAR_LFM2_THREADS", "4"))
TRANSLATE_MAX_NEW = int(os.environ.get("SIDECAR_TRANSLATE_MAX_NEW", "48"))
TRANSLATE_SYSTEM = os.environ.get("SIDECAR_TRANSLATE_SYSTEM", "Translate to English.")
TRANSLATE_MAX_TEXTS = int(os.environ.get("SIDECAR_TRANSLATE_MAX_TEXTS", "16"))

START = time.time()

# 同時実行数スロット（DoS・スレッド爆発の抑止。取得失敗は 503）
SLOTS = threading.BoundedSemaphore(MAX_CONCURRENCY)


class BekkoEmbed:
    def __init__(self, snap):
        self.tok = AutoTokenizer.from_pretrained(snap)
        onnx_path = os.path.join(snap, "onnx", "model.onnx")
        so = ort.SessionOptions()
        so.intra_op_num_threads = EMBED_THREADS  # 4コア共有のため控えめ（perf レビュー対応）
        self.sess = ort.InferenceSession(onnx_path, so, providers=["CPUExecutionProvider"])
        self.innames = {i.name for i in self.sess.get_inputs()}
        v = self.embed(["ping"])
        self.dim = int(v.shape[1])

    def embed(self, texts):
        # サブバッチ分割（512件一括は attention で数GB → OOM。順序は維持）
        chunks = []
        for i in range(0, len(texts), BATCH):
            chunks.append(self._embed_chunk(list(texts[i : i + BATCH])))
        if not chunks:
            return np.zeros((0, self.dim), dtype=np.float32)
        return np.concatenate(chunks, axis=0).astype(np.float32)

    def _embed_chunk(self, texts):
        enc = self.tok(
            list(texts), padding=True, truncation=True, max_length=MAXLEN, return_tensors="np"
        )
        feeds = {
            "input_ids": enc["input_ids"].astype(np.int64),
            "attention_mask": enc["attention_mask"].astype(np.int64),
        }
        if "token_type_ids" in self.innames:
            feeds["token_type_ids"] = np.zeros_like(feeds["input_ids"])
        out = self.sess.run(None, feeds)[0]
        if out.ndim == 3:
            mask = feeds["attention_mask"][..., None].astype(np.float32)
            emb = (out * mask).sum(1) / np.clip(mask.sum(1), 1e-6, None)
        else:
            emb = out
        emb = emb / np.clip(np.linalg.norm(emb, axis=1, keepdims=True), 1e-12, None)
        return emb.astype(np.float32)


def _to_past_name(out_name):
    """present_* 出力名 → 対応する past_* 入力名。"""
    if out_name.startswith("present_conv"):
        return "past_conv" + out_name[len("present_conv") :]
    if out_name.startswith("present."):
        return "past_key_values." + out_name[len("present.") :]
    return None


class Lfm2Translator:
    """LFM2-350M-ENJP-MT — ONNX quantized, greedy decode with KV cache."""

    def __init__(self, model_dir):
        self.tok = AutoTokenizer.from_pretrained(model_dir)
        so = ort.SessionOptions()
        so.intra_op_num_threads = LFM2_THREADS
        self.sess = ort.InferenceSession(
            os.path.join(model_dir, "onnx", "model_quantized.onnx"),
            so,
            providers=["CPUExecutionProvider"],
        )
        self.in_names = [i.name for i in self.sess.get_inputs()]
        self.out_names = [o.name for o in self.sess.get_outputs()]
        self.lock = threading.Lock()
        # 初期状態（conv: [1,C,3] / kv: [1,heads,0,dim]）— 動的次元は 1 / 0 に置換
        self.init_states = {}
        for i in self.sess.get_inputs():
            if not i.name.startswith("past_"):
                continue
            shape = []
            for d in i.shape:
                if isinstance(d, int):
                    shape.append(d)
                else:
                    shape.append(0 if "past_sequence" in str(d) else 1 if "batch" in str(d) else 0)
            self.init_states[i.name] = np.zeros(shape, dtype=np.float32)
        self.eos = self.tok.eos_token_id  # 7 = <|im_end|>

    def translate(self, text, max_new=None):
        max_new = int(max_new or TRANSLATE_MAX_NEW)
        msgs = [
            {"role": "system", "content": TRANSLATE_SYSTEM},
            {"role": "user", "content": text},
        ]
        ids = self.tok.apply_chat_template(msgs, add_generation_prompt=True, tokenize=True)
        if hasattr(ids, "keys"):  # dict / BatchEncoding（transformers 5.x）
            ids = ids["input_ids"]
        elif hasattr(ids, "tolist"):
            ids = ids.tolist()
        if ids and isinstance(ids[0], (list, tuple)):
            ids = list(ids[0])
        ids = [int(t) for t in ids]
        with self.lock:
            states = {k: v.copy() for k, v in self.init_states.items()}
            total = len(ids)
            cur = np.array([ids], dtype=np.int64)
            am = np.ones((1, total), dtype=np.int64)
            generated = []
            for _ in range(max_new):
                feeds = {"input_ids": cur, "attention_mask": am}
                feeds.update(states)
                out = self.sess.run(None, feeds)
                nxt = int(np.argmax(out[0][0, -1, :]))
                if nxt == self.eos:
                    break
                generated.append(nxt)
                for name, val in zip(self.out_names, out):
                    pn = _to_past_name(name)
                    if pn is not None:
                        states[pn] = val
                total += 1
                cur = np.array([[nxt]], dtype=np.int64)
                am = np.ones((1, total), dtype=np.int64)
        return self.tok.decode(generated, skip_special_tokens=True).strip()


EMBED = None
TRANSLATE = None


class Handler(BaseHTTPRequestHandler):
    server_version = "honchol-sidecar/0.2"
    protocol_version = "HTTP/1.1"

    def _send(self, code, obj):
        body = json.dumps(obj, ensure_ascii=False).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, fmt, *args):
        sys.stderr.write("[sidecar] %s - %s\n" % (self.address_string(), fmt % args))

    def _read_json(self):
        try:
            n = int(self.headers.get("Content-Length", "0"))
        except ValueError:
            n = 0
        if n <= 0 or n > MAX_BODY:
            self._send(400, {"error": "bad content-length"})
            return None
        try:
            return json.loads(self.rfile.read(n))
        except Exception:
            self._send(400, {"error": "bad json"})
            return None

    def _origin_host_ok(self):
        # DNSリバインディング・ブラウザ経由の blind POST 対策（honchol-serve の guard と同思想）
        from urllib.parse import urlparse

        host = (self.headers.get("Host") or "").strip().lower()
        if host.startswith("["):
            hostname = host.split("]", 1)[0][1:]
        else:
            hostname = host.rsplit(":", 1)[0] if ":" in host else host
        if hostname not in ("127.0.0.1", "localhost", "::1"):
            return False
        origin = self.headers.get("Origin")
        if origin:
            try:
                oh = (urlparse(origin).hostname or "").lower()
            except ValueError:
                return False
            if oh not in ("127.0.0.1", "localhost", "::1"):
                return False
        return True

    def do_GET(self):
        if self.path == "/health":
            if not self._origin_host_ok():
                self._send(403, {"error": "forbidden"})
                return
            self._send(200, {
                "ok": True,
                "uptime_s": round(time.time() - START, 1),
                "embed": {
                    "model": MODEL_NAME if EMBED else None,
                    "dim": EMBED.dim if EMBED else None,
                },
                "translate": {
                    "model": LFM2_NAME if TRANSLATE else None,
                    "loaded": TRANSLATE is not None,
                },
            })
            return
        self._send(404, {"error": "not found"})

    def do_POST(self):
        if not self._origin_host_ok():
            self._send(403, {"error": "forbidden"})
            return
        if not SLOTS.acquire(blocking=False):
            self._send(503, {"error": "busy"})
            return
        try:
            if self.path == "/embed":
                self._handle_embed()
                return
            if self.path == "/translate":
                self._handle_translate()
                return
            self._send(404, {"error": "not found"})
        finally:
            SLOTS.release()

    def _handle_embed(self):
        payload = self._read_json()
        if payload is None:
            return
        texts = payload.get("texts")
        if (
            not isinstance(texts, list)
            or not texts
            or len(texts) > MAX_TEXTS
            or not all(isinstance(t, str) for t in texts)
            or max(len(t) for t in texts) > MAX_EMBED_CHARS
        ):
            self._send(400, {"error": "texts must be a non-empty list of strings (<= %d, each <= %d chars)" % (MAX_TEXTS, MAX_EMBED_CHARS)})
            return
        t0 = time.time()
        try:
            vecs = EMBED.embed(texts)
        except Exception as e:  # noqa: BLE001
            self._send(500, {"error": "embed failed: %s" % e})
            return
        self._send(200, {
            "vectors": [list(map(float, v)) for v in vecs],
            "dim": EMBED.dim,
            "model": MODEL_NAME,
            "ms": round((time.time() - t0) * 1000, 1),
        })

    def _handle_translate(self):
        if TRANSLATE is None:
            self._send(503, {"error": "translator not loaded"})
            return
        payload = self._read_json()
        if payload is None:
            return
        texts = payload.get("texts")
        if (
            not isinstance(texts, list)
            or not texts
            or len(texts) > TRANSLATE_MAX_TEXTS
            or not all(isinstance(t, str) for t in texts)
            or max(len(t) for t in texts) > MAX_TRANSLATE_CHARS
        ):
            self._send(400, {"error": "texts must be a non-empty list of strings (<= %d, each <= %d chars)" % (TRANSLATE_MAX_TEXTS, MAX_TRANSLATE_CHARS)})
            return
        try:
            max_new = int(payload.get("max_new_tokens") or TRANSLATE_MAX_NEW)
        except (TypeError, ValueError):
            self._send(400, {"error": "bad max_new_tokens"})
            return
        max_new = max(1, min(max_new, TRANSLATE_MAX_NEW_CAP))
        t0 = time.time()
        try:
            outs = [TRANSLATE.translate(t, max_new) for t in texts]
        except Exception as e:  # noqa: BLE001
            self._send(500, {"error": "translate failed: %s" % e})
            return
        self._send(200, {
            "translations": outs,
            "model": LFM2_NAME,
            "ms": round((time.time() - t0) * 1000, 1),
        })


def main():
    global EMBED, TRANSLATE
    snap = resolve_bekko_dir()
    print("[sidecar] loading bekko from %s ..." % snap, flush=True)
    t0 = time.time()
    EMBED = BekkoEmbed(snap)
    t1 = time.time()
    print("[sidecar] bekko ready: dim=%d load=%.1fs" % (EMBED.dim, t1 - t0), flush=True)
    try:
        print("[sidecar] loading LFM2 from %s ..." % LFM2_DIR, flush=True)
        TRANSLATE = Lfm2Translator(LFM2_DIR)
        print("[sidecar] LFM2 ready: load=%.1fs eos=%s" % (time.time() - t1, TRANSLATE.eos), flush=True)
    except Exception as e:  # noqa: BLE001
        print("[sidecar] LFM2 load FAILED: %s — /translate は 503" % e, flush=True)
    print("[sidecar] ready: dim=%d port=%d" % (EMBED.dim, PORT), flush=True)
    httpd = ThreadingHTTPServer(("127.0.0.1", PORT), Handler)
    httpd.serve_forever()


if __name__ == "__main__":
    main()
