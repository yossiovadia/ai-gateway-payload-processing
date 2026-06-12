"""Headroom sidecar wrapper — adds /v1/compress-raw endpoint.

Starts the standard headroom proxy and adds a raw compression endpoint
that calls ContentRouter.compress() directly, bypassing message-level
protection logic. The Go BBR plugin handles selection (what to compress
vs protect); this endpoint just compresses whatever it receives.
"""

import json
import os
import sys
import threading
import time

import uvicorn
from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse

app = FastAPI()

_router = None
_router_lock = threading.Lock()


def _get_router():
    global _router
    if _router is None:
        with _router_lock:
            if _router is None:
                from headroom.transforms.content_router import ContentRouter
                from headroom.transforms.kompress_compressor import KompressCompressor
                router = ContentRouter()
                # Pre-load Kompress so it's available for text compression
                try:
                    kc = KompressCompressor()
                    kc.preload()
                    router._kompress = kc
                except Exception as e:
                    print(f"Warning: Kompress not available: {e}")
                _router = router
    return _router


@app.post("/v1/compress-raw")
async def compress_raw(request: Request):
    """Compress text blocks directly via ContentRouter — no message protection."""
    body = await request.json()
    texts = body.get("texts", [])
    if not texts:
        return JSONResponse({"results": []})

    router = _get_router()
    results = []
    for text in texts:
        if not text or not isinstance(text, str):
            results.append({
                "compressed": text or "",
                "original_tokens": 0,
                "compressed_tokens": 0,
            })
            continue
        result = router.compress(text)
        # Use character-based token estimation (4 chars ≈ 1 token)
        # ContentRouter's total_compressed_tokens is unreliable
        orig_tokens = len(text) // 4
        comp_tokens = len(result.compressed) // 4
        results.append({
            "compressed": result.compressed,
            "original_tokens": orig_tokens,
            "compressed_tokens": comp_tokens,
        })

    return JSONResponse({"results": results})


@app.get("/healthz")
@app.get("/readyz")
async def health():
    return {"status": "ok"}


def main():
    port = int(os.environ.get("HEADROOM_RAW_PORT", "8788"))
    host = os.environ.get("HEADROOM_RAW_HOST", "0.0.0.0")

    # Start the standard headroom proxy in a background thread
    def run_headroom_proxy():
        proxy_port = int(os.environ.get("HEADROOM_PORT", "8787"))
        proxy_host = os.environ.get("HEADROOM_HOST", "0.0.0.0")
        os.system(f"headroom proxy --port {proxy_port} --host {proxy_host}")

    proxy_thread = threading.Thread(target=run_headroom_proxy, daemon=True)
    proxy_thread.start()

    # Give headroom proxy a moment to start
    time.sleep(2)

    # Start the raw compression server
    print(f"compress-raw server listening on {host}:{port}")
    uvicorn.run(app, host=host, port=port, log_level="info")


if __name__ == "__main__":
    main()
