#!/usr/bin/env python3
"""Compare WeMM image vectors produced by the original and compressed profiles.

This deliberately compares vectors for the same local keyframes. It does not
modify the index and does not rebuild any media.
"""

from __future__ import annotations

import argparse
import json
import math
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any


def request_embeddings(endpoint: str, images: list[str], profile: str) -> list[list[float]]:
    payload = json.dumps(
        {"modality": "image", "images": images, "image_profile": profile}
    ).encode("utf-8")
    request = urllib.request.Request(
        endpoint.rstrip("/") + "/v1/embeddings",
        data=payload,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(request, timeout=600) as response:
            decoded: dict[str, Any] = json.load(response)
    except urllib.error.HTTPError as error:
        detail = error.read().decode("utf-8", errors="replace")
        raise RuntimeError(f"{profile} embedding returned HTTP {error.code}: {detail}") from error
    return decoded.get("embeddings") or []


def cosine(left: list[float], right: list[float]) -> float:
    if len(left) != len(right) or not left:
        return 0.0
    dot = sum(a * b for a, b in zip(left, right))
    left_norm = math.sqrt(sum(value * value for value in left))
    right_norm = math.sqrt(sum(value * value for value in right))
    if not left_norm or not right_norm:
        return 0.0
    return max(-1.0, min(1.0, dot / (left_norm * right_norm)))


def indexed_frame_paths(index_path: Path, limit: int) -> list[str]:
    state = json.loads(index_path.read_text(encoding="utf-8"))
    paths: list[str] = []
    seen: set[str] = set()
    for stored in (state.get("scenes") or {}).values():
        scene = stored.get("scene") or {}
        path = str(scene.get("preview_path") or "").strip()
        if not path:
            continue
        candidate = Path(path)
        if not candidate.is_file():
            continue
        normalized = str(candidate.resolve())
        if normalized in seen:
            continue
        seen.add(normalized)
        paths.append(normalized)
        if len(paths) >= limit:
            break
    return paths


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--endpoint", default="http://127.0.0.1:7001")
    parser.add_argument("--index", default="data/index.json")
    parser.add_argument("--sample-size", type=int, default=32)
    parser.add_argument("--batch-size", type=int, default=8)
    parser.add_argument(
        "--threshold",
        type=float,
        default=0.995,
        help="recommend compression only when every sampled cosine is at least this value",
    )
    args = parser.parse_args()
    if args.sample_size <= 0 or args.batch_size <= 0:
        parser.error("--sample-size and --batch-size must be positive")
    if not 0 < args.threshold <= 1:
        parser.error("--threshold must be between 0 and 1")

    images = indexed_frame_paths(Path(args.index), args.sample_size)
    if not images:
        raise SystemExit("no local preview_path files found in the index")

    original: list[list[float]] = []
    compressed: list[list[float]] = []
    for start in range(0, len(images), args.batch_size):
        batch = images[start : start + args.batch_size]
        original.extend(request_embeddings(args.endpoint, batch, "original"))
        compressed.extend(request_embeddings(args.endpoint, batch, "compressed"))
    if len(original) != len(images) or len(compressed) != len(images):
        raise SystemExit(
            f"embedding count mismatch: images={len(images)}, "
            f"original={len(original)}, compressed={len(compressed)}"
        )

    similarities = [cosine(left, right) for left, right in zip(original, compressed)]
    mean_similarity = sum(similarities) / len(similarities)
    minimum = min(similarities)
    maximum = max(similarities)
    print(json.dumps(
        {
            "sample_count": len(images),
            "cosine_mean": round(mean_similarity, 8),
            "cosine_min": round(minimum, 8),
            "cosine_max": round(maximum, 8),
            "drift_mean": round(1 - mean_similarity, 8),
            "drift_max": round(1 - minimum, 8),
            "threshold": args.threshold,
            "recommendation": "compressed"
            if minimum >= args.threshold
            else "original",
        },
        ensure_ascii=False,
        indent=2,
    ))


if __name__ == "__main__":
    main()
