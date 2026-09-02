# Video Semantic Search MVP Handoff

更新时间：2026-09-02

## 1. 当前目标与架构

主架构是 Go，Python 只负责 embedding 推理。

```text
本地文件 / 阿里云盘文件
        ↓
Go acquisition manager
        ↓
元数据与 MP4 sample table 解析
        ↓
静态切镜检测或快速均匀采样
        ↓
关键帧 / 代表画面提取为 JPG
        ↓ HTTP
Python WeMM embedding service
        ↓
Go FileStore + 场景向量检索
        ↓
按视频归集或按片段平铺返回
```

Go 负责 HTTP API、任务状态、来源接入、远程 Range 读取、视频处理、持久化和排序；Python 服务常驻加载 WeMM 模型，避免每次请求重新加载模型。

## 2. 已实现能力

- 本地视频按路径处理，不复制原视频，只在 `data/frames/<media_id>/` 保存 JPG。
- 阿里云盘通过已登录账户解析文件和短期下载地址。
- MP4 使用 `github.com/Eyevinn/mp4ff` 读取容器索引，优先取得视频轨道、时长、编码、I 帧 sample 的偏移和长度。
- 远程视频使用 HTTP Range + 4 MiB chunk cache，只下载 MP4 头部、索引和需要的媒体 sample。
- Range 请求支持并发去重、重试、缓存、连接池和有界预取。
- 远程 MP4 默认快速模式，最多 32 个代表画面；远程关键帧抽取默认 8 路并发，可配置。
- 单个时间点抽帧失败不会直接取消整部视频；可继续处理其他画面并在任务元数据中记录警告。
- CUDA 抽帧失败会自动回退 CPU，并在处理器生命周期内禁用反复失败的 CUDA 尝试。
- 长视频 sample 时间戳使用 64 位解码时间，避免 mp4ff 的 32 位时间累加回绕造成错误定位。
- 过期的云盘签名 URL 已增加 401/403 自动刷新回调；该改动需要重新编译并用真实视频复测。
- 相同本地文件按 SHA-256 去重；同一内容重复提交不会重复解析、抽帧和 embedding。
- 任务状态写入 `data/acquisition_tasks.json`，刷新页面或重启服务后可以查看历史状态。

## 3. 当前真实视频验证结果

测试文件：`Inception.2010.1080p.BluRay.x265-RARBG.mp4`，远程大小约 2.31 GiB，时长约 8888 秒，视频编码 HEVC，1920×800。

最近一次真实任务是在签名 URL 自动刷新改动部署前执行的，结果如下：

- MP4 索引可以读取，未下载完整视频。
- 32 个目标画面中 7 个成功，25 个产生警告。
- 已成功的 6 个走了直接 sample 解码，1 个走了普通远程 seek。
- 失败主要是抽帧期间远程 Range 返回 HTTP 403，随后一个末尾位置等待超时。
- 当时 Range 共下载约 50.6 MB，说明没有完整下载视频。

因此，当前不能宣称这个视频已经达到“稳定且 30 秒完成”。下一步必须使用最新二进制重新运行同一个文件，确认签名 URL 刷新和关键帧定位修复是否生效。

## 4. 启动方式

### 4.1 Python embedding

复用已有环境，不重复安装 CUDA、Torch 等基础包：

```bash
cd /home/zephyr/go/src/video-semantic-search
PYTHON_BIN=/home/zephyr/go/src/hunyuan3d/.venv/bin/python \
  python -m pip install -r python/requirements-model-extra.txt
```

常驻启动脚本：

```bash
cd /home/zephyr/go/src/video-semantic-search
PROXY_URL=http://192.168.50.199:10810 \
  EMBEDDING_PORT=7002 \
  scripts/start_embedding.sh
```

`start_embedding.sh` 默认使用 Hugging Face 普通 HTTP 下载并复用缓存，模型下载阶段才使用代理；推理服务本身只监听回环地址。模型已下载时也可以关闭代理：

```bash
PROXY_URL=direct EMBEDDING_PORT=7002 scripts/start_embedding.sh
```

主要 Python 参数：

- `PYTHON_BIN`：Python 解释器，默认 `/home/zephyr/go/src/hunyuan3d/.venv/bin/python`。
- `EMBEDDING_BACKEND`：`wemm` 或 `hash`，默认 `wemm`。
- `WEMM_MODEL`：默认 `tencent/WeMM-Embedding-2B`。
- `EMBEDDING_DEVICE`：默认 `cuda`。
- `EMBEDDING_PORT`：默认 `7001`。
- `EMBEDDING_BATCH_SIZE`：默认 `8`，显存不足时降为 `4` 或 `1`。
- `WEMM_MIN_IMAGE_PIXELS` / `WEMM_MAX_IMAGE_PIXELS`：默认 `65536` / `98304`。

模型下载和监控：

```bash
WEMM_DOWNLOAD_MODE=http scripts/download_wemm.sh
scripts/watch_wemm_download.sh --interval 10
```

模型下载脚本支持 `WEMM_DOWNLOAD_MODE=xet`，但当前远程视频处理和 Go 服务不应继承代理环境。

### 4.2 Go 服务

推荐先编译，再启动服务：

```bash
cd /home/zephyr/go/src/video-semantic-search
go build -o /tmp/video-semantic-search-server ./cmd/search-server

set -a
source .env
set +a
unset HTTP_PROXY HTTPS_PROXY ALL_PROXY http_proxy https_proxy all_proxy
EMBEDDING_ENDPOINT=http://127.0.0.1:7002 \
VIDEO_FAST_MODE=true \
VIDEO_FAST_MAX_SCENES=32 \
VIDEO_REMOTE_FRAME_WORKERS=8 \
/tmp/video-semantic-search-server
```

Go 的远程文件请求使用显式直连 HTTP client，外部 `ffmpeg` / `ffprobe` 子进程也会清理代理环境。

默认地址和数据：

- Go API：`http://127.0.0.1:8000`
- Python embedding：`http://127.0.0.1:7001`
- 当前测试实例：`http://127.0.0.1:7002`
- 索引：`data/index.json`
- 任务：`data/acquisition_tasks.json`
- 画面：`data/frames/<media_id>/`
- 服务日志：`logs/search-server.log`
- embedding 日志：`logs/embedding-service.log`

## 5. 关键启动参数

| 参数 | 默认值 | 作用 |
|---|---:|---|
| `VIDEO_FAST_MODE` | `false` | 跳过完整静态切镜扫描，使用快速采样 |
| `VIDEO_FAST_MAX_SCENES` | `32` | 快速模式最多画面数 |
| `VIDEO_FRAME_WORKERS` | `4` | 普通抽帧并发数 |
| `VIDEO_REMOTE_FRAME_WORKERS` | `8` | 远程 MP4 直接 sample 抽帧并发数 |
| `VIDEO_FRAME_WIDTH` | `640` | JPG 输出宽度 |
| `VIDEO_FRAME_TIMEOUT` | `45s` | 单个抽帧尝试超时 |
| `VIDEO_FFMPEG_HWACCEL` | `auto` | `auto`、`cuda` 或 `none` |
| `VIDEO_SCENE_WORKERS` | `4` | 准确模式分镜窗口并发数 |
| `VIDEO_SCENE_REFINE_WINDOWS` | `false` | 是否对候选窗口做精细切镜检测 |
| `VIDEO_IMAGE_BATCH_SIZE` | 由引擎决定 | Go 发送图片 embedding 的批大小 |
| `EMBEDDING_ENDPOINT` | `http://127.0.0.1:7001` | Python 服务地址 |
| `EMBEDDING_TIMEOUT` | `5m` | Go 调用 embedding 超时 |

远程视频性能优先时建议：

```bash
VIDEO_FAST_MODE=true \
VIDEO_FAST_MAX_SCENES=32 \
VIDEO_REMOTE_FRAME_WORKERS=8 \
VIDEO_FRAME_WIDTH=640 \
VIDEO_FFMPEG_HWACCEL=auto
```

如果远端服务出现 429、403 或带宽抖动，将 `VIDEO_REMOTE_FRAME_WORKERS` 调低到 `4`；如果 Range 延迟高且没有限流，可以尝试 `12`，不建议无限增加。

## 6. 远程视频任务命令

提交任务时只传阿里云盘的 drive/file 标识，Go 会临时获取签名 URL：

```bash
curl -fsS -X POST http://127.0.0.1:8000/v1/acquisitions \
  -H 'Content-Type: application/json' \
  --data '{
    "source":"alipan",
    "drive_id":"<drive-id>",
    "file_id":"<file-id>",
    "source_name":"Inception.2010.1080p.BluRay.x265-RARBG.mp4",
    "type":"movie",
    "sample_interval":30,
    "scene_threshold":0.35,
    "fast_mode":true
  }'
```

查询任务：

```bash
curl -s http://127.0.0.1:8000/v1/acquisitions/<task-id> \
  | jq '{state,stage,percent,message,error,media_id}'
```

查看实际抽帧方式、Range 流量和每个 sample 的定位：

```bash
curl -s http://127.0.0.1:8000/v1/media/<media-id> | jq '.metadata'
```

重点字段：

- `remote_range.requests` / `remote_range.bytes_downloaded`：实际 Range 请求和下载量。
- `frame_extraction_methods`：`indexed_sample`、`indexed_proxy`、`proxy_exact` 的数量。
- `frame_extraction_sources`：scene index、sample number、sample timestamp、offset、size 和摘要。
- `frame_extraction_warnings`：局部画面失败原因。

## 7. API 和页面

- `GET /`：搜索页面。
- `POST /v1/acquisitions`：创建异步本地或阿里云盘采集任务。
- `GET /v1/acquisitions`、`GET /v1/acquisitions/{id}`：任务列表和详情。
- `DELETE /v1/acquisitions/{id}`：停止未完成任务。
- `GET /v1/media`、`GET /v1/media/{id}`：已处理文件和场景。
- `GET /v1/media/{id}/frames/{filename}`：查看 JPG。
- `DELETE /v1/media/{id}/scenes/{scene_id}`：删除关键帧和向量。
- `DELETE /v1/media/{id}`：删除整部媒体及其向量。
- `POST /v1/search`：语义搜索，支持按媒体或片段返回。
- `GET /healthz`：服务和索引健康检查。

搜索排序使用 embedding 相似度降序，默认返回 Top 16；页面支持按视频归集或按片段平铺。当前搜索阈值默认大于 0.3，可由请求覆盖。

## 8. 验证命令

```bash
cd /home/zephyr/go/src/video-semantic-search
go test ./...
go test -race ./internal/source ./internal/acquisition
go vet ./...
```

检查运行状态：

```bash
curl -fsS http://127.0.0.1:8000/healthz | jq
curl -fsS http://127.0.0.1:7002/healthz | jq
pgrep -af 'video-semantic-search-server|embedding_service.py'
pgrep -af 'ffmpeg|ffprobe'
```

检查图片是否重复：

```bash
md5sum data/frames/<media-id>/frame-*.jpg \
  | awk '{print $1}' | sort | uniq -c
```

## 9. 尚未完成与优先级

1. 用签名 URL 自动刷新版本重新跑当前 Inception 文件，确认 403 后能继续取样，并确认后段画面不重复。
2. 为 MP4 索引阶段增加实时 Range 字节数、当前阶段耗时和阶段超时，避免页面长时间停在“读取容器索引”而没有反馈。
3. 根据实际服务端限流测试 `VIDEO_REMOTE_FRAME_WORKERS=4/8/12`，记录总耗时、成功率、Range 流量和 GPU 利用率。
4. 对 fragmented MP4、非 MP4 容器、多音轨、异常 sample table 和损坏码流增加专门 fallback；目前异常会回退到 FFmpeg 通用读取或报告局部不可解码。
5. 将当前 `FileStore` 替换为 PostgreSQL + Qdrant，并增加视频级粗召回和场景级精排。

不要把 `.env`、`data/alipan_profiles.json`、下载 URL、access token 或 refresh token 提交到仓库。
