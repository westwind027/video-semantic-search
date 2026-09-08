#!/usr/bin/env python3
"""Face detection and embedding service for the video search identity path.

The service is intentionally separate from ``embedding_service.py``. Go owns
movie metadata, cast filtering, thresholds and persistence; this process only
loads InsightFace and turns one image into zero or more face embeddings.
"""

from __future__ import annotations

import hashlib
import json
import math
import os
import threading
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any, Protocol


class BackendUnavailable(RuntimeError):
    pass


class Backend(Protocol):
    model: str
    dimension: int
    providers: list[str]

    def embed(self, image: str) -> list[dict[str, Any]]: ...


class HashBackend:
    """Deterministic offline backend for API and Go integration tests only."""

    model = "hash-face-mvp"
    dimension = 512
    providers = ["offline"]

    def embed(self, image: str) -> list[dict[str, Any]]:
        data = Path(image).read_bytes()
        digest = hashlib.blake2b(data, digest_size=64).digest()
        vector = []
        for index in range(self.dimension):
            byte = digest[index % len(digest)]
            vector.append((byte / 127.5) - 1.0)
        norm = math.sqrt(sum(value * value for value in vector))
        if norm:
            vector = [value / norm for value in vector]
        return [{"bbox": [0.0, 0.0, 1.0, 1.0], "det_score": 1.0, "quality": 1.0, "embedding": vector}]


class InsightFaceBackend:
    def __init__(self, model_name: str, device: str) -> None:
        try:
            import cv2
            from insightface.app import FaceAnalysis
        except Exception as error:  # pragma: no cover - depends on local model env
            raise BackendUnavailable(
                "InsightFace is unavailable; install python/requirements-identity.txt "
                f"in the shared environment: {error}"
            ) from error

        self._cv2 = cv2
        self.model = model_name
        self.dimension = 512
        resolved = device.casefold().strip()
        if resolved == "cpu":
            providers = ["CPUExecutionProvider"]
            ctx_id = -1
        else:
            providers = ["CUDAExecutionProvider", "CPUExecutionProvider"]
            ctx_id = 0
        self._app = FaceAnalysis(name=model_name, providers=providers)
        self._app.prepare(ctx_id=ctx_id, det_size=(640, 640))
        self.providers = list(getattr(self._app, "providers", providers))

    def embed(self, image: str) -> list[dict[str, Any]]:
        frame = self._cv2.imread(image)
        if frame is None:
            raise ValueError(f"cannot decode image: {image}")
        height, width = frame.shape[:2]
        result: list[dict[str, Any]] = []
        for face in self._app.get(frame):
            bbox = [float(value) for value in face.bbox.tolist()]
            det_score = float(getattr(face, "det_score", 0.0))
            face_area = max(0.0, bbox[2] - bbox[0]) * max(0.0, bbox[3] - bbox[1])
            frame_area = max(1.0, float(width * height))
            size_quality = min(1.0, math.sqrt(face_area / frame_area) / 0.25)
            quality = max(0.0, min(1.0, det_score * size_quality))
            embedding = getattr(face, "embedding", None)
            if embedding is None:
                continue
            result.append(
                {
                    "bbox": bbox,
                    "det_score": det_score,
                    "quality": quality,
                    "embedding": [float(value) for value in embedding.tolist()],
                }
            )
        return result


_backend: Backend | None = None
_backend_error: str | None = None
_backend_lock = threading.Lock()
_inference_lock = threading.Lock()


def build_backend() -> Backend:
    backend_name = os.getenv("IDENTITY_BACKEND", "insightface").casefold().strip()
    if backend_name == "hash":
        return HashBackend()
    if backend_name != "insightface":
        raise BackendUnavailable(f"unsupported IDENTITY_BACKEND={backend_name!r}")
    return InsightFaceBackend(os.getenv("FACE_MODEL", "buffalo_l"), os.getenv("IDENTITY_DEVICE", "cuda"))


def get_backend() -> Backend:
    global _backend, _backend_error
    if _backend is not None:
        return _backend
    with _backend_lock:
        if _backend is not None:
            return _backend
        try:
            _backend = build_backend()
            _backend_error = None
        except Exception as error:
            _backend_error = str(error)
            raise BackendUnavailable(_backend_error) from error
        return _backend


class Handler(BaseHTTPRequestHandler):
    server_version = "video-identity/0.1"

    def do_GET(self) -> None:  # noqa: N802
        if self.path != "/healthz":
            self._json(HTTPStatus.NOT_FOUND, {"error": "not found"})
            return
        try:
            backend = get_backend()
            payload = {
                "status": "ok",
                "model": backend.model,
                "dimension": backend.dimension,
                "backend": os.getenv("IDENTITY_BACKEND", "insightface"),
                "providers": backend.providers,
            }
        except BackendUnavailable as error:
            payload = {
                "status": "degraded",
                "backend": os.getenv("IDENTITY_BACKEND", "insightface"),
                "error": str(error),
            }
        self._json(HTTPStatus.OK, payload)

    def do_POST(self) -> None:  # noqa: N802
        if self.path not in ("/v1/faces/embed", "/v1/faces/embed-batch"):
            self._json(HTTPStatus.NOT_FOUND, {"error": "not found"})
            return
        try:
            length = int(self.headers.get("Content-Length", "0"))
            if length <= 0 or length > 1 * 1024 * 1024:
                raise ValueError("request must contain a non-empty body under 1 MiB")
            payload = json.loads(self.rfile.read(length))
            if not isinstance(payload, dict):
                raise ValueError("request body must be a JSON object")
            batch = self.path == "/v1/faces/embed-batch"
            if batch:
                images = payload.get("images")
                if not isinstance(images, list) or not images:
                    raise ValueError("images must be a non-empty list of local image paths")
                max_batch = max(1, int(os.getenv("IDENTITY_MAX_BATCH", "16")))
                if len(images) > max_batch:
                    raise ValueError(f"images must contain at most {max_batch} paths")
            else:
                images = [payload.get("image")]
            image_paths: list[Path] = []
            for image in images:
                if not isinstance(image, str) or not image.strip():
                    raise ValueError("image must be a local image path")
                image_path = Path(image).expanduser()
                if not image_path.is_file():
                    raise ValueError(f"image does not exist: {image}")
                image_paths.append(image_path)
            backend = get_backend()
            # ONNX Runtime/GPU models are not reliably reentrant across all
            # provider versions; keep the service concurrent at HTTP level but
            # serialize the small inference critical section.
            with _inference_lock:
                faces_by_image = [backend.embed(str(image_path)) for image_path in image_paths]
            if batch:
                results = [
                    {"faces": faces, "model": backend.model, "dimension": backend.dimension}
                    for faces in faces_by_image
                ]
                self._json(HTTPStatus.OK, {"results": results, "model": backend.model, "dimension": backend.dimension})
            else:
                self._json(
                    HTTPStatus.OK,
                    {
                        "faces": faces_by_image[0],
                        "model": backend.model,
                        "dimension": backend.dimension,
                    },
                )
        except BackendUnavailable as error:
            self._json(HTTPStatus.SERVICE_UNAVAILABLE, {"error": str(error)})
        except (ValueError, TypeError, json.JSONDecodeError) as error:
            self._json(HTTPStatus.BAD_REQUEST, {"error": str(error)})
        except Exception as error:  # pragma: no cover - protects the daemon
            self._json(HTTPStatus.INTERNAL_SERVER_ERROR, {"error": str(error)})

    def _json(self, status: HTTPStatus, payload: Any) -> None:
        body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, format: str, *args: Any) -> None:
        print(f"identity: {format % args}")


def main() -> None:
    host = os.getenv("IDENTITY_HOST", "127.0.0.1")
    port = int(os.getenv("IDENTITY_PORT", "7003"))
    server = ThreadingHTTPServer((host, port), Handler)
    print(f"identity service listening on http://{host}:{port}")
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
