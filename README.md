# Video Semantic Search

[English](README.en.md) · 中文

一个以 Go 为主、Python 负责模型推理的视频语义搜索引擎 MVP。

它可以把视频解析为关键帧或代表画面，使用多模态 embedding 建立场景级索引，再通过自然语言搜索返回相关视频、时间段和可访问的预览图。项目也提供可选的人脸识别、IMDb/TMDB 元数据、阿里云盘远程视频和外部播放器控制能力。

## 特性

- Go 负责 HTTP API、任务队列、视频解析、远程 Range 读取、索引、检索、静态资源和 Web UI。
- Python embedding service 支持 `hash` 离线后端和 Tencent WeMM 多模态后端。
- 文本查询与图片场景使用同一向量空间，WeMM 默认输出 2048 维归一化向量，并使用余弦相似度排序。
- 支持快速采样和关键帧/准确采样；默认快速模式最多 32 帧，准确模式最多 240 帧。
- 支持 MP4 和 Matroska/WebM 的远程元数据解析与关键帧 Range 下载，不需要完整下载远程视频。
- 支持按内容 SHA-256 去重，重复提交相同内容不会重复解析、抽帧和嵌入。
- 支持异步任务、进度 SSE、批量停止、批量删除和向量重建。
- 支持 `small`、`tiny` 或自定义尺寸的静态预览 URL。
- 可选接入 InsightFace、IMDb、TMDB，实现演员筛选和场景人脸标注。
- 可选接入阿里云盘，支持二维码登录、文件浏览和远程视频处理。
- 支持通过 API 控制已打开页面的播放器跳转、播放和关闭。

## 架构

```text
                 ┌─────────────────────────────┐
                 │          Go Search API       │
                 │  UI / Tasks / Index / Media  │
                 └──────────────┬──────────────┘
                                │ HTTP
             ┌──────────────────┴──────────────────┐
             │                                     │
   ┌─────────▼─────────┐                ┌──────────▼─────────┐
   │ Python Embedding  │                │ Python Identity     │
   │ WeMM or hash      │                │ InsightFace optional│
   └───────────────────┘                └────────────────────┘

  video → metadata/I-frames → representative frames → embeddings → search
```

Go 与 Python 服务之间使用简单的 HTTP JSON 接口，可以独立部署。默认存储使用文件索引和 SQLite，不依赖外部数据库；后续可以替换为 PostgreSQL、Qdrant 等适配器。

Python 推理服务的独立安装、API 和配置说明见 [`python/README.md`](python/README.md)。

## Docker Compose 快速启动

项目提供三套镜像：Go 搜索服务、WeMM embedding 服务和可选的
InsightFace Identity 服务。Go 镜像内置 `ffmpeg`/`ffprobe`；GPU 镜像使用
CUDA 12.8，模型权重首次启动时下载到 Docker 缓存卷，不会打进镜像。

使用 WeMM 和 InsightFace 需要已安装 NVIDIA 驱动、NVIDIA Container Toolkit
以及支持 GPU 的 Docker；仅验证 API 闭环时，可以把 `docker/.env.example`
中的两个后端改为 `hash`，不需要下载模型。

```bash
cp docker/.env.example docker/.env
# 编辑 docker/.env，按需填写 HF_TOKEN、TMDB_API_KEY 等运行时配置
mkdir -p media
docker compose --env-file docker/.env -f docker/compose.yaml pull
docker compose --env-file docker/.env -f docker/compose.yaml up -d
curl -fsS http://localhost:8000/healthz
```

默认只暴露 Go 服务的 `8000` 端口，embedding 和 Identity 通过 Compose
内部网络访问。首次启动会下载 WeMM 和 InsightFace 权重，后续启动复用
`hf-cache`、`insightface-cache` 卷。需要处理本地视频时，将视频放在
`media/` 下，并在页面/API 中使用容器内路径 `/media/<filename>`；也可以用
`VIDEO_SEARCH_MEDIA_DIR` 把 `/media` 映射到其他主机目录。

默认使用 Docker Hub 的 `clean-release-v1.0` 镜像标签；升级到其他发布版本
时修改 `IMAGE_TAG`。开发者需要从源码重新构建时，可执行
`docker compose ... up -d --build`。

```bash
docker compose --env-file docker/.env -f docker/compose.yaml logs -f
docker compose --env-file docker/.env -f docker/compose.yaml down
```

不要把包含 token 的 `docker/.env` 或其他凭据复制进镜像或提交到 Git。

## 环境要求

- Go 1.22 或更高版本
- Python 3.10 或更高版本
- `ffmpeg` 和 `ffprobe`
- 使用 WeMM 时，需要一个与当前平台匹配的 PyTorch 环境；GPU/CUDA 为可选项
- 使用人脸识别时，需要 InsightFace 及其 ONNX Runtime 依赖
- 使用 IMDb/TMDB 元数据同步时，需要 IMDb bulk 数据和 TMDB API key

项目提供的 Python requirements 文件不会强制安装特定 CUDA/cuBLAS 版本。建议先按照 PyTorch 官方说明安装适合本机的 PyTorch，再安装项目依赖。

```bash
python3 -m pip install -r python/requirements-model.txt
python3 -m pip install -r python/requirements-identity.txt  # 可选
```

如果已有可用的 PyTorch 环境，也可以只安装额外依赖：

```bash
python3 -m pip install -r python/requirements-model-extra.txt
```

## 快速启动

### 1. 构建 Go 服务

```bash
go build -o bin/search-server ./cmd/search-server
```

### 2. 启动离线 embedding 后端

`hash` 后端不理解真实图像，只适合验证 API、任务队列和索引闭环，不代表实际视觉搜索质量。

```bash
EMBEDDING_BACKEND=hash \
EMBEDDING_HOST=127.0.0.1 \
EMBEDDING_PORT=7001 \
python3 python/embedding_service.py
```

### 3. 启动 Go 服务

另开终端执行：

```bash
./bin/search-server
```

默认地址为 <http://localhost:8000>。服务启动后可以访问 `/healthz` 检查索引和 embedding 状态：

```bash
curl -fsS http://localhost:8000/healthz
```

### 4. 启动 WeMM

安装模型依赖后，将 embedding 后端切换为 WeMM：

```bash
EMBEDDING_BACKEND=wemm \
WEMM_MODEL=tencent/WeMM-Embedding-2B \
EMBEDDING_DIMENSION=2048 \
EMBEDDING_DEVICE=auto \
EMBEDDING_BATCH_SIZE=8 \
python3 python/embedding_service.py
```

模型会由 Hugging Face 客户端下载并使用其标准缓存。若模型仓库需要鉴权，请通过运行环境提供 Hugging Face token，不要把 token 写入仓库或命令历史。显存不足时，优先降低 `EMBEDDING_BATCH_SIZE`。

也可以使用启动脚本：

```bash
PYTHON_BIN=python3 PROXY_URL=direct scripts/start_embedding.sh --no-server
```

脚本支持通过 `PYTHON_BIN`、`PROXY_URL`、`EMBEDDING_BACKEND` 等环境变量覆盖默认值。脚本只负责启动服务，不会把凭据写入项目。

## Web UI 使用示例

Go 服务启动后打开 <http://localhost:8000>。以下截图来自真实运行实例：影片名称、搜索词、相关度、时间点和关键帧均来自实际索引；仅隐藏了服务端本机文件路径。

### 搜索

在搜索框输入对画面、人物、动作或氛围的自然语言描述，点击“搜索”。例如运行实例使用 `a child holding a football` 命中了《当幸福来敲门》的真实关键帧。下图使用“按片段平铺”模式，直接展示各个命中片段、时间点和相关度；也可以切换为“按视频归集”。

![搜索结果与相关度排序](assets/screenshots/search-results.png)

### 添加与处理

点击右上角上传图标，在弹窗中输入 Go 服务所在主机可访问的文件或目录路径。文件可以直接加入待处理列表；目录需要点击“扫描目录”，并可以选择是否递归扫描。确认文件可用后，选择快速采样或关键帧检测，再点击“开始批量处理”。

![添加来源与批量处理](assets/screenshots/upload-processing.png)

### 任务进度

任务提交后，右下角处理队列会通过 SSE 更新状态和进度。队列支持全选、反选、停止未完成任务，以及删除已完成、失败或已停止的记录。

![处理队列与任务状态](assets/screenshots/task-queue.png)

### 文件管理

点击右上角管理图标，可以查看已处理视频和关键帧。选中一个或多个视频后，可以批量重建 `original` 或 `compressed` 向量，也可以删除整文件；进入视频详情后，还能删除单个关键帧及其向量。

![已处理文件与关键帧管理](assets/screenshots/library-management.png)

## 导入与处理视频

### JSONL 导入

适合已经有关键帧或场景描述的媒体数据：

```bash
go run ./cmd/ingest -file /path/to/media.jsonl
```

也可以调用 `POST /v1/media` 逐条导入。

### 视频采集任务

浏览器页面中的路径是 Go 服务所在主机可访问的路径，不会通过浏览器上传或复制原视频。服务支持 Unix、Windows 和 WSL 风格路径转换，但路径最终必须在服务端可见。

```bash
curl -fsS -X POST http://localhost:8000/v1/acquisitions \
  -H 'content-type: application/json' \
  -d '{
    "local_path": "/path/to/movie.mp4",
    "source_name": "movie.mp4",
    "fast_mode": true
  }'
```

批量提交：

```bash
curl -fsS -X POST http://localhost:8000/v1/acquisitions/batch \
  -H 'content-type: application/json' \
  -d '{
    "items": [
      {"local_path": "/path/to/movie-1.mp4", "fast_mode": true},
      {"local_path": "/path/to/movie-2.mkv", "fast_mode": true}
    ]
  }'
```

任务提交立即返回，后台负责元数据准备、内容指纹、容器解析、画面提取、人脸标注和 embedding。任务进度可以通过 `GET /v1/acquisitions` 查询，也可以连接 `GET /v1/acquisitions/events` 获取 SSE 事件。

快速模式适合交互式索引，通常按固定间隔采样；准确模式会优先使用容器关键帧并进行切镜检测。画面数量可通过 `VIDEO_FAST_MAX_SCENES` 和 `VIDEO_MAX_SCENES` 调整，当前默认上限分别为 32 和 240。

远程阿里云盘视频会尽量只读取容器元数据和目标 I 帧：

1. 嗅探容器类型并读取 MP4 `moov` 或 Matroska `SeekHead`/`Cues`。
2. 获取关键帧的精确 byte range。
3. 只下载目标帧并交给 FFmpeg 解码为 JPEG。
4. 将代表帧送入 embedding service。

当容器缺少索引或格式不受支持时，系统可能回退到分块顺序读取。任务结果会记录远程读取和索引缓存信息。

## 搜索

```bash
curl -fsS -X POST http://localhost:8000/v1/search \
  -H 'content-type: application/json' \
  -d '{
    "query": "雨夜中一辆红色汽车经过街道",
    "limit": 16,
    "min_score": 0.3,
    "mode": "scene"
  }'
```

也可以使用 GET 网络接口：

```text
GET /v1/search?q=雨夜中一辆红色汽车经过街道&limit=16&mode=scene
```

搜索模式：

- `mode=media`：按视频归集，返回每个视频的最佳命中片段。
- `mode=scene`：按片段平铺，返回场景级结果。

WeMM 检索使用 `encode_query`、`encode_document` 和归一化向量，结果按余弦相似度降序排列。`limit` 表示返回的媒体或片段数量；`min_score` 是可选过滤条件。

## 预览、播放与外部控制

搜索结果中的 `preview` 是绝对 URL；也可以直接访问稳定静态路由：

```text
GET /static/frames/{media_id}/{filename}
GET /static/frames/{media_id}/{filename}?size=small
GET /static/frames/{media_id}/{filename}?size=tiny
GET /static/frames/{media_id}/{filename}?width=320&height=240&quality=80
```

其中 `small` 最大为 320×240，`tiny` 最大为 160×120，缩放保持原始宽高比。

视频播放：

```text
GET /v1/media/{media_id}/stream
```

外部程序可以控制已打开的 Web 页面：

```bash
curl -fsS -X POST http://localhost:8000/v1/player/control \
  -H 'content-type: application/json' \
  -d '{
    "action": "open",
    "media_id": "<media-id>",
    "time": 123.45,
    "autoplay": true,
    "fullscreen": true
  }'

curl -fsS -X POST http://localhost:8000/v1/player/control \
  -H 'content-type: application/json' \
  -d '{"action":"close"}'
```

播放器通过 SSE 接收命令。浏览器的自动全屏受 transient user activation 安全策略限制；如果浏览器拒绝自动全屏，页面会提供一次点击确认。

## 可选：人脸识别与 IMDb/TMDB

人脸链路默认关闭。启用前先启动 Identity Service：

```bash
IDENTITY_BACKEND=insightface \
IDENTITY_DEVICE=auto \
PYTHON_BIN=python3 \
scripts/start_identity.sh
```

然后启动 Go 服务时设置：

```bash
IDENTITY_ENABLED=true \
IDENTITY_ENDPOINT=http://127.0.0.1:7003 \
TMDB_API_KEY=<your-tmdb-api-key> \
./bin/search-server
```

Identity Service 负责检测人脸和生成 512 维向量；Go 负责 IMDb/TMDB 元数据、演员关系、参考图生命周期、阈值和场景标签。默认惰性加载：只有处理包含对应 IMDb 演员的影片时，才按需读取 TMDB profile 图片并建立缺失的人脸向量。参考图默认在推理完成后删除，只保留向量和来源元数据；如需人工审核，可设置 `IDENTITY_KEEP_REFERENCE_IMAGES=true`。

IMDb bulk 数据导入使用 `catalog-sync`：

```bash
scripts/download_imdb_datasets.sh
go build -o bin/catalog-sync ./cmd/catalog-sync

./bin/catalog-sync \
  --imdb-basics data/imdb/title.basics.tsv.gz \
  --imdb-ratings data/imdb/title.ratings.tsv.gz \
  --imdb-principals data/imdb/title.principals.tsv.gz \
  --imdb-names data/imdb/name.basics.tsv.gz \
  --imdb-akas data/imdb/title.akas.tsv.gz \
  --imdb-crew data/imdb/title.crew.tsv.gz \
  --title-id-file data/imdb/title-ids.txt \
  --tmdb
```

`--tmdb` 应使用有界选择，例如 `--title-id-file`、`--max-movies` 或年份范围，避免无意同步整个目录。所有 token、API key 和登录凭据都应通过环境变量或未提交的 `.env` 文件提供。

## 阿里云盘

页面右上角的云盘入口支持二维码登录和文件浏览。默认使用 tickstep 登录链路，不需要把个人 token 写入前端；如果使用官方 OAuth，则配置：

```bash
ALIYUNPAN_LOGIN_MODE=official
ALIYUNPAN_CLIENT_ID=<your-client-id>
ALIYUNPAN_CLIENT_SECRET=<your-client-secret>
ALIYUNPAN_REDIRECT_URI=oob
```

登录凭据只保存在服务端配置的运行目录中。不要将 `.env`、token 文件或云盘缓存提交到 Git。

## 主要配置

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `VIDEO_SEARCH_ADDR` | `:8000` | Go HTTP 服务监听地址 |
| `VIDEO_SEARCH_INDEX` | `data/index.json` | 媒体与场景索引 |
| `VIDEO_SEARCH_FRAME_DIR` | `data/frames` | 代表帧目录 |
| `VIDEO_SEARCH_TASK_FILE` | `data/acquisition_tasks.json` | 任务状态文件 |
| `VIDEO_SEARCH_PUBLIC_URL` | 当前请求 Host | 生成预览绝对 URL 的公开地址 |
| `EMBEDDING_ENDPOINT` | `http://127.0.0.1:7001` | Python embedding service 地址 |
| `EMBEDDING_BACKEND` | `hash` | `hash` 或 `wemm` |
| `EMBEDDING_DIMENSION` | `256`/`2048` | 由 embedding 后端决定 |
| `EMBEDDING_DEVICE` | `auto` | `auto`、`cuda` 或 `cpu` |
| `EMBEDDING_BATCH_SIZE` | `8` | embedding 批大小 |
| `VIDEO_FAST_MODE` | `false` | 是否默认使用快速采样 |
| `VIDEO_FAST_MAX_SCENES` | `32` | 快速模式最大画面数 |
| `VIDEO_MAX_SCENES` | `240` | 准确模式最大画面数 |
| `VIDEO_FRAME_WORKERS` | `4` | 抽帧并发数 |
| `VIDEO_REMOTE_RANGE_WORKERS` | `4` | 远程 Range 请求并发数 |
| `VIDEO_REMOTE_MP4_MOOV_WORKERS` | `4` | MP4 元数据分块并发数 |
| `VIDEO_IMAGE_BATCH_SIZE` | `32` | Go 发往 embedding 的图片批大小 |
| `VIDEO_IMAGE_PROFILE` | `original` | `original` 或 `compressed` |
| `IDENTITY_ENABLED` | `false` | 开启演员/人脸链路 |
| `IDENTITY_ENDPOINT` | `http://127.0.0.1:7003` | Identity Service 地址 |
| `IDENTITY_LAZY_LOAD` | `true` | 按影片惰性加载演员参考图 |
| `IDENTITY_MIN_REFERENCES` | `5` | 演员进入搜索候选所需的最少参考向量数 |
| `TMDB_API_KEY` | 未设置 | TMDB 元数据和头像来源 |

所有运行时数据、索引、图片、缓存和日志都应放在被忽略的目录中。生产部署建议显式设置路径，不要使用包含凭据的共享目录。

## API 概览

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| `GET` | `/healthz` | 服务和 embedding 状态 |
| `POST` | `/v1/search` | JSON 搜索 |
| `GET` | `/v1/search` | 网络查询参数搜索 |
| `POST` | `/v1/media` | 导入已有媒体/场景数据 |
| `GET` | `/v1/media` | 列出已索引媒体 |
| `GET` | `/v1/media/{id}` | 查看媒体及场景 |
| `POST` | `/v1/acquisitions` | 提交单个视频处理任务 |
| `POST` | `/v1/acquisitions/batch` | 批量提交视频处理任务 |
| `GET` | `/v1/acquisitions/events` | 任务 SSE 事件流 |
| `POST` | `/v1/acquisitions/batch/stop` | 批量停止任务 |
| `POST` | `/v1/acquisitions/batch/delete` | 删除终态任务记录 |
| `POST` | `/v1/media/embeddings/rebuild` | 批量重建向量 |
| `POST` | `/v1/media/batch/delete` | 批量删除媒体及场景 |
| `GET` | `/static/frames/...` | 访问代表帧和缩略图 |
| `POST` | `/v1/player/control` | 控制已打开页面的播放器 |

## 数据格式

`POST /v1/media` 和 JSONL 导入接受类似下面的媒体对象：

```json
{
  "media_id": "movie-001",
  "type": "movie",
  "title": "示例电影",
  "year": 2024,
  "language": ["zh"],
  "description": "影片简介",
  "tags": ["动作"],
  "scenes": [
    {
      "start": 0,
      "end": 10,
      "preview": "https://example.invalid/frame.jpg",
      "caption": "一个人在雪地里奔跑",
      "subtitle": "我们必须回去"
    }
  ]
}
```

视频采集任务会自动生成真实的 `start`、`end`、`preview` 和 `preview_path`。原始视频不会被写入索引目录。

## 安全与部署建议

- 不要提交 `.env`、API key、OAuth secret、access token、refresh token 或云盘配置文件。
- 默认只监听回环地址；需要局域网访问时再显式设置监听地址和 `VIDEO_SEARCH_PUBLIC_URL`。
- 服务目前没有内置用户认证，部署到公网前应放在认证网关或内网之后。
- 允许服务端路径处理时，请限制 Go 进程可访问的目录，并避免把不可信用户输入直接暴露给服务端。
- 浏览器只提交路径，服务不会自动获得浏览器本地文件内容；路径必须由服务端能够读取。
- 参考图默认只作为人脸推理临时输入，成功后删除；如保留图片，请单独保护其存储目录。

## 已知限制

- `hash` 后端只用于集成测试，不具备视觉或人脸语义能力。
- WeMM 首次启动需要下载模型，并且显存、批大小和图片 profile 会影响吞吐。
- 自动全屏受浏览器安全策略限制，必要时需要用户点击确认。
- 远程视频对 fragmented MP4、缺失 Cues 的 Matroska、laced block 和 MPEG-TS/AVI 等格式仍可能回退或失败。
- 当前文件索引适合 MVP 和单实例部署；大规模多实例部署应替换为共享存储和专用向量数据库。

## 开发与测试

```bash
go test ./...
go vet ./...
```

运行时生成的索引、任务、帧、模型缓存和日志不应进入版本库。提交前可以检查：

```bash
git status --short --ignored
git diff --check
```

## License

This project is licensed under the Apache License 2.0. See [LICENSE](LICENSE).

The Alibaba Cloud Drive integration uses
[`github.com/tickstep/aliyunpan-api`](https://github.com/tickstep/aliyunpan-api)
v0.2.9, which is also distributed under Apache License 2.0. See
[THIRD_PARTY_NOTICES.txt](THIRD_PARTY_NOTICES.txt) for the attribution.
