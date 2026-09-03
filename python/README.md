# Embedding inference service

这是 Go 主服务唯一依赖的 Python 进程。它提供：

- `GET /healthz`
- `POST /v1/embeddings`，请求体为 `{"modality":"text","texts":[...]}` 或 `{"modality":"image","images":[...]}`。图片请求可选 `image_profile`：`original` 使用 WeMM 模型默认图片处理；`compressed` 使用 `WEMM_COMPRESSED_MIN_IMAGE_PIXELS` / `WEMM_COMPRESSED_MAX_IMAGE_PIXELS` 限制图片像素预算。两种 profile 只影响图片预处理，不改变 2048 维输出和余弦检索协议。

默认 `HashBackend` 只用于离线验证链路；它对 image URL 做稳定哈希，并不理解图像内容。设置 `EMBEDDING_BACKEND=wemm` 后，服务使用 `tencent/WeMM-Embedding-2B`，默认从 Hugging Face 加载，支持 `role=query|document` 和 image path/URL。WEMM 图片 document 默认使用 `{"image": path, "text": "Represent this image."}`；可通过 `WEMM_IMAGE_PROMPT=` 关闭提示词。保持上述响应格式即可，Go 服务不需要改动。

本项目复用 `/home/zephyr/go/src/hunyuan3d/.venv` 作为模型运行时。该环境已经提供 Torch/CUDA；只补充本目录的额外包：

```bash
/home/zephyr/go/src/hunyuan3d/.venv/bin/python -m pip install -r python/requirements-model-extra.txt
```

WEMM 模型首次加载时将通过 `HTTP_PROXY`/`HTTPS_PROXY` 下载并缓存权重。官方建议使用 `transformers==5.2.0` 做严格复现；当前复用环境的 Transformers 5.x 已能加载 WEMM 的远程模型代码，如需严格复现可单独评估是否调整该环境版本。

```bash
cd video-semantic-search
python3 python/embedding_service.py
```
