# Python Inference Services

`python/` 提供两个独立的 HTTP 推理服务。Go 主服务通过稳定的 JSON API 调用它们，Python 进程不负责任务调度、索引和业务状态。

- `embedding_service.py`：文本和图片 embedding。
- `identity_service.py`：可选的人脸检测与人脸 embedding。

两个服务都默认只监听回环地址，适合与 Go 服务部署在同一台机器上；也可以通过环境变量独立部署。

## 依赖安装

要求 Python 3.10 或更高版本。推荐使用独立虚拟环境：

```bash
python3 -m venv .venv
source .venv/bin/activate
python -m pip install --upgrade pip
```

### WeMM embedding

`requirements-model.txt` 是完整安装清单，包含 PyTorch、Transformers、Sentence Transformers 和多模态处理依赖：

```bash
python -m pip install -r python/requirements-model.txt
```

PyTorch 的 CUDA/CPU wheel 会因操作系统、GPU 和驱动版本而不同。生产环境应先按照 PyTorch 官方安装说明选择匹配版本，再安装其余依赖。

如果已经有可用的 PyTorch 环境，只安装本项目额外依赖：

```bash
python -m pip install -r python/requirements-model-extra.txt
```

`requirements-model-extra.txt` 刻意不包含 Torch、CUDA 或 cuBLAS，避免覆盖已有的硬件匹配环境。

### Identity Service

人脸服务是可选组件：

```bash
python -m pip install -r python/requirements-identity.txt
```

InsightFace 使用 ONNX Runtime。GPU 部署需要与本机驱动匹配的 `onnxruntime-gpu` 及 CUDA runtime；CPU 部署可以使用 `onnxruntime`。项目不会在 requirements 中强制选择其中一种硬件版本。

## Embedding Service

### 离线 hash 后端

hash 后端没有模型依赖，适合验证 Go API、任务队列和索引闭环。它只对文本或图片路径做稳定哈希，不理解图片内容，也不代表实际搜索质量。

```bash
EMBEDDING_BACKEND=hash \
EMBEDDING_HOST=127.0.0.1 \
EMBEDDING_PORT=7001 \
python3 python/embedding_service.py
```

### WeMM 后端

```bash
EMBEDDING_BACKEND=wemm \
WEMM_MODEL=tencent/WeMM-Embedding-2B \
EMBEDDING_DIMENSION=2048 \
EMBEDDING_DEVICE=auto \
EMBEDDING_BATCH_SIZE=8 \
python3 python/embedding_service.py
```

首次启动时，Sentence Transformers 会从 Hugging Face 加载模型并使用标准模型缓存。若模型仓库需要鉴权，请通过运行环境提供 Hugging Face token；不要把 token、代理认证信息或 `.env` 文件提交到 Git。

WeMM 的实现遵循 Sentence Transformers 的 query/document 用法：

- `role=query` 使用 `encode_query`。
- `role=document` 使用 `encode_document`。
- 输出默认做归一化并截断到 `EMBEDDING_DIMENSION`。
- 图片 document 默认包含 `{"image": <path-or-url>, "text": "Represent this image."}`。
- 图片 `original` profile 使用模型默认预处理；`compressed` profile 通过像素预算降低输入成本，但不会改变输出维度。
- Go 侧使用归一化向量的余弦相似度排序。

### Embedding API

健康检查：

```bash
curl -fsS http://127.0.0.1:7001/healthz
```

文本请求：

```bash
curl -fsS -X POST http://127.0.0.1:7001/v1/embeddings \
  -H 'content-type: application/json' \
  -d '{
    "modality": "text",
    "role": "query",
    "texts": ["一个人在雨夜中奔跑"]
  }'
```

图片请求：

```bash
curl -fsS -X POST http://127.0.0.1:7001/v1/embeddings \
  -H 'content-type: application/json' \
  -d '{
    "modality": "image",
    "images": ["/path/to/frame.jpg"],
    "image_profile": "original"
  }'
```

服务返回 `embeddings`、`model`、`dimension`、`modality` 和 `image_profile`。图片输入可以是模型加载器支持的本地路径或 URL；Go 采集流程通常发送服务端可读的代表帧路径。

## Identity Service

### InsightFace 后端

```bash
IDENTITY_BACKEND=insightface \
FACE_MODEL=buffalo_l \
IDENTITY_DEVICE=cpu \
IDENTITY_HOST=127.0.0.1 \
IDENTITY_PORT=7003 \
python3 python/identity_service.py
```

GPU 部署时将 `IDENTITY_DEVICE` 设置为 `cuda`，并确保 ONNX Runtime、CUDA 和驱动版本匹配。

### 离线 hash 后端

仅用于 HTTP 联调，不具备人脸识别能力：

```bash
IDENTITY_BACKEND=hash python3 python/identity_service.py
```

### Identity API

健康检查：

```bash
curl -fsS http://127.0.0.1:7003/healthz
```

单张图片：

```bash
curl -fsS -X POST http://127.0.0.1:7003/v1/faces/embed \
  -H 'content-type: application/json' \
  -d '{"image":"/path/to/face.jpg"}'
```

批量图片：

```bash
curl -fsS -X POST http://127.0.0.1:7003/v1/faces/embed-batch \
  -H 'content-type: application/json' \
  -d '{
    "images": ["/path/to/face-1.jpg", "/path/to/face-2.jpg"]
  }'
```

单张接口返回 `faces` 数组；每个人脸包含 `bbox`、`det_score`、`quality` 和 512 维 `embedding`。批量接口返回与输入顺序对应的结果数组。输入图片必须是 Identity Service 所在环境可读的路径。

## 配置参考

### Embedding

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `EMBEDDING_BACKEND` | `hash` | `hash` 或 `wemm` |
| `WEMM_MODEL` | `tencent/WeMM-Embedding-2B` | Hugging Face 模型 ID |
| `EMBEDDING_DIMENSION` | `256`/`2048` | hash 和 WeMM 的输出维度 |
| `EMBEDDING_DEVICE` | `auto` | `auto`、`cuda` 或 `cpu` |
| `EMBEDDING_HOST` | `127.0.0.1` | 监听地址 |
| `EMBEDDING_PORT` | `7001` | 监听端口 |
| `EMBEDDING_BATCH_SIZE` | `8` | 模型推理批大小 |
| `WEMM_IMAGE_PROMPT` | `Represent this image.` | 图片 document 的文本提示 |
| `WEMM_COMPRESSED_MIN_IMAGE_PIXELS` | `65536` | compressed profile 最小像素预算 |
| `WEMM_COMPRESSED_MAX_IMAGE_PIXELS` | `98304` | compressed profile 最大像素预算 |

### Identity

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `IDENTITY_BACKEND` | `insightface` | `insightface` 或 `hash` |
| `FACE_MODEL` | `buffalo_l` | InsightFace 模型名称 |
| `IDENTITY_DEVICE` | `cuda` | `cuda` 或 `cpu` |
| `IDENTITY_HOST` | `127.0.0.1` | 监听地址 |
| `IDENTITY_PORT` | `7003` | 监听端口 |
| `IDENTITY_MAX_BATCH` | `16` | 批量接口最大图片数 |

## 与 Go 服务集成

Go 服务默认访问：

```text
Embedding Service: http://127.0.0.1:7001
Identity Service:  http://127.0.0.1:7003
```

如果服务地址不同，设置：

```bash
EMBEDDING_ENDPOINT=http://embedding-host:7001 \
IDENTITY_ENDPOINT=http://identity-host:7003 \
./bin/search-server
```

也可以使用仓库根目录的启动脚本，但应显式指定 Python 解释器：

```bash
PYTHON_BIN=python3 PROXY_URL=direct scripts/start_embedding.sh --no-server
PYTHON_BIN=python3 scripts/start_identity.sh
```

启动脚本的网络代理、日志路径和模型参数均可通过环境变量覆盖。模型下载策略属于运行环境配置，不应写死在项目说明或提交到仓库。

## 安全注意事项

- 两个服务都接收本地图片路径；不要把它们直接暴露到不可信公网。
- 默认绑定 `127.0.0.1`，跨主机部署时应使用网络隔离、认证网关或防火墙。
- 不要在请求、日志和版本库中输出 Hugging Face token、TMDB key、OAuth secret 或云盘 token。
- Python 服务不会持久化 Go 的索引或任务状态；缓存和模型目录由运行环境管理。
- `hash` 后端只适合测试，不应被误用于生产检索或人脸识别。

## 开发检查

```bash
python3 -m py_compile python/embedding_service.py python/identity_service.py
go test ./...
```
