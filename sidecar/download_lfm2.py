#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""LFM2-350M-ENJP-MT ONNX（量子化版）の取得 — サイドカー翻訳用。"""
import os

os.environ.setdefault("HF_HUB_DISABLE_XET", "1")  # Xet 停止問題の回避（実測）

from huggingface_hub import snapshot_download

# 固定リビジョン（supply-chain 対策）。上書きする場合のみ環境変数で。
REVISION = os.environ.get("SIDECAR_LFM2_REVISION", "29e6195eb1f63ddb3fa29c320edd4f4b25fa4ae7")
DEST = os.environ.get("SIDECAR_LFM2_DEST", os.path.expanduser("~/models/lfm2-enjp-mt-onnx"))

p = snapshot_download(
    "onnx-community/LFM2-350M-ENJP-MT-ONNX",
    revision=REVISION,
    local_dir=DEST,
    allow_patterns=[
        "onnx/model_quantized.onnx",
        "onnx/model_quantized.onnx_data",
        "config.json",
        "generation_config.json",
        "tokenizer.json",
        "tokenizer_config.json",
        "special_tokens_map.json",
        "chat_template.jinja",
    ],
)
print("downloaded to", p)
print("hint: point SIDECAR_LFM2_DIR at this directory if it is not ~/models/lfm2-enjp-mt-onnx")
