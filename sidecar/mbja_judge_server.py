#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""honchol judge sidecar — optional local relevance scorer (ModernBERT-Ja + GLiClass head).

POST /judge {"query": str, "texts": [str]} → {"scores": [float], "model": "mbja130m", "ms": ...}
GET  /health

Experimental: the Go server's kind=mbja path talks to this endpoint. It expects a locally
exported ONNX encoder + tokenizer (see SIDECAR_MBJA_DIR / SIDECAR_MBJA_ONNX).
In our zero-shot probes the scores were not reliable enough to replace the default judge —
kept for experimentation with fine-tuned relevance heads.

レシピ（学習時と同型の choice 組立・held-out プローブ AUC 0.822）:
  Question: How is the text related to the search query: {q}?
  labels: [It is relevant / It is unrelated / insufficient evidence]
  score = softmax(logits[:3])[0] = P(relevant)
"""
import json
import os
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import numpy as np
import onnxruntime as ort
from transformers import AutoTokenizer

MODEL_DIR = os.environ.get("SIDECAR_MBJA_DIR", os.path.expanduser("~/models/mbja-130m"))
ONNX_PATH = os.environ.get("SIDECAR_MBJA_ONNX", os.path.expanduser("~/models/mbja-130m.onnx"))
PORT = int(os.environ.get("SIDECAR_JUDGE_PORT", "8795"))
MAXLEN = int(os.environ.get("SIDECAR_JUDGE_MAXLEN", "256"))
MAX_TEXTS = int(os.environ.get("SIDECAR_JUDGE_MAX_TEXTS", "64"))
MAX_BODY = int(os.environ.get("SIDECAR_JUDGE_MAX_BODY_BYTES", str(2 << 20)))
MAX_QUERY_CHARS = int(os.environ.get("SIDECAR_JUDGE_MAX_QUERY_CHARS", "2000"))
MAX_TEXT_CHARS = int(os.environ.get("SIDECAR_JUDGE_MAX_TEXT_CHARS", "4000"))
MAX_CONCURRENCY = int(os.environ.get("SIDECAR_JUDGE_MAX_CONCURRENCY", "8"))
NAME = os.environ.get("SIDECAR_MBJA_NAME", "mbja130m")

LABEL_MARKER = "<<LABEL>>"
SEP_MARKER = "<<SEP>>"

START = time.time()

# 同時実行数スロット（DoS・スレッド爆発の抑止。取得失敗は 503）
SLOTS = threading.BoundedSemaphore(MAX_CONCURRENCY)


def build_prompt(q: str, t: str) -> str:
    """学習時の choice レシピと同型のプロンプトを組む。"""
    labels = [
        f"It is relevant to the search query: {q}",
        f"It is unrelated to the search query: {q}",
        "insufficient evidence",
    ]
    text = f"Question: {'How is the text related to the search query: ' + q + '?'}\n\nContext:\n{t}"
    return "".join(f"{LABEL_MARKER}{l}" for l in labels) + SEP_MARKER + text


class Scorer:
    def __init__(self):
        t0 = time.time()
        self.tok = AutoTokenizer.from_pretrained(MODEL_DIR)
        so = ort.SessionOptions()
        so.intra_op_num_threads = int(os.environ.get("SIDECAR_JUDGE_THREADS", "4"))
        self.sess = ort.InferenceSession(ONNX_PATH, so, providers=["CPUExecutionProvider"])
        self.lock = threading.Lock()
        self.load_s = round(time.time() - t0, 1)
        print(f"[judge] loaded {NAME}: load={self.load_s}s maxlen={MAXLEN}", flush=True)

    def score(self, query: str, texts: list[str]) -> list[float]:
        prompts = [build_prompt(query, t) for t in texts]
        enc = self.tok(prompts, padding=True, truncation=True, max_length=MAXLEN, return_tensors="np")
        feeds = {"input_ids": enc["input_ids"].astype(np.int64),
                 "attention_mask": enc["attention_mask"].astype(np.int64)}
        with self.lock:
            logits = self.sess.run(None, feeds)[0]
        out = []
        for i in range(len(texts)):
            z = logits[i, :3].astype(np.float64)
            e = np.exp(z - z.max())
            out.append(float((e / e.sum())[0]))
        return out


SCORER = None


class Handler(BaseHTTPRequestHandler):
    server_version = "honchol-mbja-judge/0.1"

    def log_message(self, fmt, *args):
        print("[judge] " + (fmt % args), flush=True)

    def _read_body(self):
        try:
            n = int(self.headers.get("Content-Length", "0"))
        except ValueError:
            return None
        if n <= 0 or n > MAX_BODY:
            return None
        return self.rfile.read(n)

    def _send(self, code: int, obj) -> None:
        b = json.dumps(obj, ensure_ascii=False).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    def _origin_host_ok(self):
        # DNSリバインディング・blind POST 対策（honchol-serve / model_sidecar と同思想）
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
                "ok": True, "model": NAME, "maxlen": MAXLEN,
                "uptime_s": round(time.time() - START, 1),
                "loaded": SCORER is not None,
            })
        else:
            self._send(404, {"error": "not found"})

    def do_POST(self):
        if not self._origin_host_ok():
            self._send(403, {"error": "forbidden"})
            return
        if not SLOTS.acquire(blocking=False):
            self._send(503, {"error": "busy"})
            return
        try:
            self._do_post_inner()
        finally:
            SLOTS.release()

    def _do_post_inner(self):
        if self.path != "/judge":
            self._send(404, {"error": "not found"})
            return
        if SCORER is None:
            self._send(503, {"error": "model not loaded"})
            return
        raw = self._read_body()
        if raw is None:
            self._send(400, {"error": "bad body"})
            return
        try:
            req = json.loads(raw)
        except Exception as e:  # noqa: BLE001
            self._send(400, {"error": f"bad json: {e}"})
            return
        if not isinstance(req, dict):
            self._send(400, {"error": "body must be a JSON object"})
            return
        q = req.get("query") or ""
        texts = req.get("texts") or []
        if not isinstance(q, str) or len(q) > MAX_QUERY_CHARS:
            self._send(400, {"error": f"query must be a string (<= {MAX_QUERY_CHARS} chars)"})
            return
        if (
            not isinstance(texts, list)
            or not texts
            or len(texts) > MAX_TEXTS
            or not all(isinstance(t, str) for t in texts)
            or max(len(t) for t in texts) > MAX_TEXT_CHARS
        ):
            self._send(400, {"error": f"texts must be 1..{MAX_TEXTS} strings (each <= {MAX_TEXT_CHARS} chars)"})
            return
        t0 = time.time()
        try:
            scores = SCORER.score(q, [str(t) for t in texts])
        except Exception as e:  # noqa: BLE001
            self._send(500, {"error": f"score failed: {e}"})
            return
        ms = round((time.time() - t0) * 1000, 1)
        self._send(200, {"scores": scores, "model": NAME, "ms": ms})


def main():
    global SCORER
    SCORER = Scorer()
    srv = ThreadingHTTPServer(("127.0.0.1", PORT), Handler)
    print(f"[judge] ready: model={NAME} port={PORT}", flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    main()
