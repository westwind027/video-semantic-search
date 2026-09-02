#!/usr/bin/env python3
"""Minimal Python embedding inference service used by the Go search server.

The default backend is deterministic and dependency-free so the complete MVP can
run locally. Replace ``HashBackend`` with WeMM when the model runtime is ready;
the HTTP contract consumed by Go stays unchanged.
"""

from __future__ import annotations

import hashlib
import json
import math
import os
import re
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any, Protocol, Sequence


WORD_RE = re.compile(r"[a-z0-9]+", re.IGNORECASE)
CJK_RE = re.compile(r"[\u4e00-\u9fff]")


def load_dotenv() -> None:
    """Load simple KEY=VALUE entries without executing the project .env file."""

    env_path = Path(__file__).resolve().parent.parent / ".env"
    if not env_path.is_file():
        return
    for raw_line in env_path.read_text(encoding="utf-8").splitlines():
        line = raw_line.strip()
        if not line or line.startswith("#"):
            continue
        if line.startswith("export "):
            line = line[7:].lstrip()
        key, separator, value = line.partition("=")
        if not separator or not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", key.strip()):
            continue
        value = value.strip()
        if len(value) >= 2 and value[0] == value[-1] and value[0] in "'\"":
            value = value[1:-1]
        os.environ.setdefault(key.strip(), value)


load_dotenv()


def features(value: str) -> list[str]:
    normalized = value.casefold()
    result = WORD_RE.findall(normalized)
    cjk = CJK_RE.findall(normalized)
    result.extend(cjk)
    for size in (2, 3):
        result.extend("".join(cjk[index : index + size]) for index in range(len(cjk) - size + 1))
    return result


class Backend(Protocol):
    model: str
    dimension: int

    def embed_texts(self, values: Sequence[str], role: str = "document") -> list[list[float]]: ...

    def embed_images(self, values: Sequence[str]) -> list[list[float]]: ...


class HashBackend:
    """Offline fallback; image references are hashed, not visually understood."""

    def __init__(self, dimension: int = 256) -> None:
        if dimension < 8:
            raise ValueError("EMBEDDING_DIMENSION must be at least 8")
        self.model = "hash-embedding-mvp"
        self.dimension = dimension

    def _embed(self, value: str) -> list[float]:
        vector = [0.0] * self.dimension
        for token in features(value):
            digest = hashlib.blake2b(token.encode("utf-8"), digest_size=8).digest()
            bucket = int.from_bytes(digest[:4], "big") % self.dimension
            vector[bucket] += 1.0 if digest[4] & 1 else -1.0
        norm = math.sqrt(sum(item * item for item in vector))
        return [item / norm for item in vector] if norm else vector

    def embed_texts(self, values: Sequence[str], role: str = "document") -> list[list[float]]:
        return [self._embed(value) for value in values]

    def embed_images(self, values: Sequence[str]) -> list[list[float]]:
        return [self._embed(value) for value in values]


class WemmBackend:
    """WeMM-Embedding backend using the official Sentence Transformers adapter."""

    def __init__(self, model_id: str, dimension: int = 2048, device: str = "auto") -> None:
        from sentence_transformers import SentenceTransformer

        if dimension <= 0:
            raise ValueError("EMBEDDING_DIMENSION must be positive")
        self._batch_size = max(1, int(os.getenv("EMBEDDING_BATCH_SIZE", "8")))
        self._min_image_pixels = int(os.getenv("WEMM_MIN_IMAGE_PIXELS", "65536"))
        self._max_image_pixels = int(os.getenv("WEMM_MAX_IMAGE_PIXELS", "98304"))
        if self._min_image_pixels < 0 or self._max_image_pixels < 0:
            raise ValueError("WEMM image pixel limits must be non-negative")
        if self._max_image_pixels > 0 and self._min_image_pixels > self._max_image_pixels:
            raise ValueError("WEMM_MIN_IMAGE_PIXELS must not exceed WEMM_MAX_IMAGE_PIXELS")
        self._image_prompt = os.getenv("WEMM_IMAGE_PROMPT", "Represent this image.").strip()
        resolved_device = None if device == "auto" else device
        self._model = SentenceTransformer(model_id, trust_remote_code=True, device=resolved_device)
        model_config = getattr(getattr(self._model[0], "auto_model", None), "config", None)
        supported_dimensions = getattr(model_config, "matryoshka_dimensions", None)
        if supported_dimensions and dimension not in supported_dimensions:
            raise ValueError(
                f"EMBEDDING_DIMENSION={dimension} is unsupported; "
                f"choose one of {list(supported_dimensions)}"
            )
        if self._model.similarity_fn_name != "cosine":
            raise ValueError(
                f"WeMM retrieval requires cosine similarity, got {self._model.similarity_fn_name!r}"
            )
        self.model = model_id
        self.dimension = dimension
        self.similarity = self._model.similarity_fn_name

    def _encode(self, values: Sequence[Any], role: str) -> list[list[float]]:
        # WeMM's retrieval example uses encode_query for queries and
        # encode_document for indexed multimodal documents. Both methods apply
        # the same WeMM encoder here because the model has no prompt/router
        # overrides, but retaining the distinction keeps the retrieval contract
        # explicit.
        method_name = "encode_query" if role == "query" else "encode_document"
        encoder = getattr(self._model, method_name, self._model.encode)
        encode_kwargs = {
            "batch_size": self._batch_size,
            "show_progress_bar": False,
            "convert_to_numpy": True,
            "normalize_embeddings": True,
            "truncate_dim": self.dimension,
        }
        if role == "document" and (self._min_image_pixels > 0 or self._max_image_pixels > 0):
            image_kwargs = {}
            if self._min_image_pixels > 0:
                image_kwargs["min_pixels"] = self._min_image_pixels
            if self._max_image_pixels > 0:
                image_kwargs["max_pixels"] = self._max_image_pixels
            encode_kwargs["processing_kwargs"] = {"image": image_kwargs}
        result = encoder(list(values), **encode_kwargs)
        if result.ndim != 2 or result.shape[1] != self.dimension:
            raise RuntimeError(
                f"embedding output shape {tuple(result.shape)} does not match "
                f"configured dimension {self.dimension}"
            )
        return result.tolist()

    def embed_texts(self, values: Sequence[str], role: str = "document") -> list[list[float]]:
        return self._encode(values, role)

    def embed_images(self, values: Sequence[str]) -> list[list[float]]:
        samples: list[dict[str, str]] = []
        for value in values:
            sample = {"image": value}
            if self._image_prompt:
                sample["text"] = self._image_prompt
            samples.append(sample)
        return self._encode(samples, "document")


def build_backend() -> Backend:
    backend = os.getenv("EMBEDDING_BACKEND", "hash").casefold()
    if backend == "hash":
        return HashBackend(int(os.getenv("EMBEDDING_DIMENSION", "256")))
    if backend == "wemm":
        return WemmBackend(
            os.getenv("WEMM_MODEL", "tencent/WeMM-Embedding-2B"),
            int(os.getenv("EMBEDDING_DIMENSION", "2048")),
            os.getenv("EMBEDDING_DEVICE", "auto"),
        )
    raise RuntimeError(f"unsupported EMBEDDING_BACKEND={backend!r}")


BACKEND = build_backend()


class Handler(BaseHTTPRequestHandler):
    server_version = "video-embedding/0.1"

    def do_GET(self) -> None:  # noqa: N802
        if self.path != "/healthz":
            self._json(HTTPStatus.NOT_FOUND, {"error": "not found"})
            return
        self._json(
            HTTPStatus.OK,
            {
                "status": "ok",
                "model": BACKEND.model,
                "dimension": BACKEND.dimension,
                "similarity": getattr(BACKEND, "similarity", "cosine"),
                "modalities": ["text", "image"],
                "batch_size": getattr(BACKEND, "_batch_size", None),
                "min_image_pixels": getattr(BACKEND, "_min_image_pixels", None),
                "max_image_pixels": getattr(BACKEND, "_max_image_pixels", None),
            },
        )

    def do_POST(self) -> None:  # noqa: N802
        if self.path != "/v1/embeddings":
            self._json(HTTPStatus.NOT_FOUND, {"error": "not found"})
            return
        try:
            length = int(self.headers.get("Content-Length", "0"))
            if length > 16 * 1024 * 1024:
                raise ValueError("request is too large")
            payload: dict[str, Any] = json.loads(self.rfile.read(length))
            modality = payload.get("modality")
            values: Any
            if modality == "text":
                values = payload.get("texts")
                embeddings = BACKEND.embed_texts(values or [], payload.get("role", "document"))
            elif modality == "image":
                values = payload.get("images")
                embeddings = BACKEND.embed_images(values or [])
            else:
                raise ValueError("modality must be text or image")
            if not isinstance(values, list) or not values:
                raise ValueError("a non-empty input array is required")
            self._json(
                HTTPStatus.OK,
                {
                    "embeddings": embeddings,
                    "model": BACKEND.model,
                    "dimension": BACKEND.dimension,
                    "modality": modality,
                },
            )
        except (ValueError, TypeError, json.JSONDecodeError) as error:
            self._json(HTTPStatus.BAD_REQUEST, {"error": str(error)})
        except Exception as error:  # pragma: no cover - protects the long-running process
            self._json(HTTPStatus.INTERNAL_SERVER_ERROR, {"error": str(error)})

    def _json(self, status: HTTPStatus, payload: Any) -> None:
        body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, format: str, *args: Any) -> None:
        print(f"embedding: {format % args}")


def main() -> None:
    host = os.getenv("EMBEDDING_HOST", "127.0.0.1")
    port = int(os.getenv("EMBEDDING_PORT", "7001"))
    server = ThreadingHTTPServer((host, port), Handler)
    print(f"embedding service listening on http://{host}:{port}, model={BACKEND.model}")
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
