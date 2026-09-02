# Video Semantic Search MVP

这是根据 `docs/video-semantic-search-engine-design.md` 落地的 Phase 1 MVP，主架构是 Go：

```text
Storyboard / Preview JSONL
        ↓
Go 导入与伪 Scene 建模
        ↓ HTTP
Python embedding inference service
        ↓
Go 本地索引（场景向量 + 元数据过滤）
        ↓
Scene → Media 聚合
        ↓
Go HTTP API / Web UI
```

Go 负责业务流程、持久化、检索排序和对外接口；Python 只负责 embedding 推理。默认 Python backend 是零依赖的确定性 fallback，用于把闭环跑起来；它不是最终的视觉语义模型。设置 `EMBEDDING_BACKEND=wemm` 后，Python 使用 WEMM，Go 侧接口无需修改。

## 启动

先启动 Python embedding 服务。当前机器复用已有的 Hunyuan3D 环境；它已经包含 Python 3.13、Torch 2.7.1+cu128 和 CUDA 运行时，只需补充本仓库的额外依赖：

```bash
cd video-semantic-search
/home/zephyr/go/src/hunyuan3d/.venv/bin/python -m pip install -r python/requirements-model-extra.txt
```

可以直接用脚本启动 Python embedding 服务（复用 `/home/zephyr/go/src/hunyuan3d/.venv`）：

```bash
cd video-semantic-search
scripts/start_embedding.sh
```

脚本默认启动 WEMM，使用普通 HTTP 下载并复用 Hugging Face 缓存；服务日志写入 `logs/embedding-service.log`。如果只想离线验证 Go 闭环，可使用 `EMBEDDING_BACKEND=hash scripts/start_embedding.sh`。

手动启动 WEMM embedding 服务时，Hugging Face 下载显式使用本机代理：

```bash
cd video-semantic-search
export HTTP_PROXY=http://192.168.50.199:10810
export HTTPS_PROXY=http://192.168.50.199:10810
export ALL_PROXY=http://192.168.50.199:10810
export EMBEDDING_BACKEND=wemm
export WEMM_MODEL=tencent/WeMM-Embedding-2B
export EMBEDDING_DIMENSION=2048
export WEMM_IMAGE_PROMPT='Represent this image.'
export EMBEDDING_DEVICE=cuda
export EMBEDDING_BATCH_SIZE=8
/home/zephyr/go/src/hunyuan3d/.venv/bin/python python/embedding_service.py
```

也可以使用脚本启动并记录下载日志，再在另一个终端定时查看进度和速度：

```bash
cd video-semantic-search
scripts/download_wemm.sh
# 另一个终端
scripts/watch_wemm_download.sh --interval 10
```

监控脚本会显示模型进度、最近速度、预计剩余时间、embedding 进程状态和到代理的 TCP 连接数。日志默认写入 `logs/wemm-download.log`。启动脚本默认使用 Hugging Face 普通 HTTP Range 下载，异常中断后会复用 `.incomplete` 文件继续传输；下载进程异常退出时最多自动重试 3 轮。若要显式测试 Xet，可设置 `WEMM_DOWNLOAD_MODE=xet scripts/download_wemm.sh`。

首次启动会从 Hugging Face 下载 WEMM 权重并复用 `HF_HOME` 缓存；官方模型支持文本、图片、视频的统一向量。本次精度测试使用模型卡示例的 2048 维输出，并为每个图片 document 加上统一的 `Represent this image.` 提示词。

若只验证 Go 闭环，不下载大模型，可省略上述 WEMM 环境变量，使用默认 hash fallback。

另开终端启动 Go 主服务：

```bash
cd video-semantic-search
VIDEO_FAST_MODE=true VIDEO_FAST_MAX_SCENES=32 VIDEO_FRAME_WORKERS=8 \
  go run ./cmd/search-server
```

页面默认勾选快速模式。以当前约 7000 秒视频为例，快速模式会限制为最多 32 个代表画面，目标是将交互式处理控制在几十秒量级；如果要完整检测每个静态切镜，取消页面勾选并使用准确模式，但耗时会随视频时长增长。

右上角的云盘图标用于阿里云盘账户接入。若没有个人 OpenAPI 应用凭据，默认使用 tickstep 的扫码登录链路：服务端生成二维码并轮询登录状态，access token 只写入服务端本地文件。若你有自己的 OpenAPI 应用，也可以显式设置 `ALIYUNPAN_LOGIN_MODE=official`，使用官方 OAuth 的 `oob` 授权码流程。环境变量写在 `.env` 时，启动前先执行 `set -a; source .env; set +a`：

```bash
export ALIYUNPAN_CLIENT_ID='your-openapi-client-id'
export ALIYUNPAN_CLIENT_SECRET='your-openapi-client-secret'
export ALIYUNPAN_LOGIN_MODE='official'
export ALIYUNPAN_REDIRECT_URI='oob'
```

如果没有个人开发者凭据，使用当前默认的 tickstep 扫码模式即可，不需要填写上面两个官方凭据：

```bash
export ALIYUNPAN_LOGIN_MODE='tickstep'
```

登录凭据只写入 `VIDEO_SEARCH_ALIPAN_CONFIG` 指定的本地文件（默认 `data/alipan_profiles.json`），文件权限为 `0600`，HTTP 响应和页面都不会返回 access token/refresh token。tickstep 模式依赖 `ALIYUNPAN_TICKSTEP_BROKER_URL`（默认 `https://api.tickstep.com`）；官方模式则只调用阿里云盘 OAuth 和 OpenAPI。后续云盘文件操作会复用这套账户会话。

导入 JSONL 数据可以直接使用 Go ingest command：

```bash
go run ./cmd/ingest -file data/sample.jsonl
```

也可以调用 `POST /v1/media` 单条写入一个 JSON object。

搜索：

```bash
curl -s http://127.0.0.1:8000/v1/search \
  -H 'content-type: application/json' \
  -d '{"query":"夕阳下两个人骑摩托车","limit":16,"min_score":0.3,"mode":"media"}'
```

然后打开 <http://127.0.0.1:8000>。主页面只保留搜索与结果区域；右上角上传图标打开批量解析弹窗，管理图标打开已处理文件和关键帧管理。页面的本地视频输入是 Go 服务端路径，不会通过浏览器上传或复制原视频；处理时只在帧目录生成 JPEG 代表帧。普通浏览器出于安全限制不一定会暴露拖拽文件的绝对路径，因此可直接在弹窗中粘贴路径或扫描目录。

默认配置：

- Go API：`127.0.0.1:8000`
- Python embedding：`127.0.0.1:7001`
- Go 索引：`data/index.json`
- 抽取帧：`data/frames/<media_id>`
- embedding：`EMBEDDING_BACKEND=hash`、256 维；真实多模态检索使用 `EMBEDDING_BACKEND=wemm`、2048 维

可通过 `VIDEO_SEARCH_ADDR`、`VIDEO_SEARCH_INDEX`、`VIDEO_SEARCH_FRAME_DIR`、`VIDEO_SEARCH_TASK_FILE`、`VIDEO_SCENE_WORKERS`、`VIDEO_SCENE_OVERLAP`、`VIDEO_SCENE_SAMPLE_FPS`、`VIDEO_SCENE_DETECTION_WIDTH`、`VIDEO_FRAME_WORKERS`、`VIDEO_FRAME_WIDTH`、`VIDEO_FRAME_TIMEOUT`、`VIDEO_FFMPEG_HWACCEL`、`VIDEO_SCENE_REFINE_WINDOWS`、`VIDEO_FAST_MODE`、`VIDEO_FAST_MAX_SCENES`、`VIDEO_IMAGE_BATCH_SIZE`、`EMBEDDING_ENDPOINT`、`EMBEDDING_TIMEOUT`、`EMBEDDING_DIMENSION`、`WEMM_IMAGE_PROMPT`、`EMBEDDING_BATCH_SIZE`、`WEMM_MIN_IMAGE_PIXELS`、`WEMM_MAX_IMAGE_PIXELS` 覆盖。`VIDEO_SCENE_WORKERS` 默认 4，`VIDEO_FRAME_WORKERS` 默认 4，`VIDEO_FRAME_WIDTH` 默认 640，`VIDEO_FRAME_TIMEOUT` 默认 45s；准确模式默认先用关键帧做低成本变化检测，再把关键帧时间作为切点候选，避免对整部电影启动大量随机 ffmpeg。若设置 `VIDEO_SCENE_REFINE_WINDOWS=true`，才会对候选窗口做 2 FPS、640 像素宽度的原始阈值精检，边界更精确但耗时明显增加。`VIDEO_FFMPEG_HWACCEL` 默认 `auto`，分镜检测和抽帧都优先尝试 CUDA，失败时自动回退 CPU；10-bit 视频会使用匹配的 `p010le` 下载格式，也可设为 `cuda` 或 `none`。设置 `VIDEO_FAST_MODE=true`，或在上传弹窗选择快速模式，会跳过静态切镜扫描，按 `sample_interval` 均匀采样，并最多生成 `VIDEO_FAST_MAX_SCENES=32` 个低分辨率代表帧；这是交互式快速索引，可能漏掉很短的镜头，准确模式适合离线完整处理。`EMBEDDING_BATCH_SIZE` 默认 8；`WEMM_MIN_IMAGE_PIXELS` 默认 65536、`WEMM_MAX_IMAGE_PIXELS` 默认 98304，用于将高分辨率图片限制在约 240p～384px 视觉输入范围，防止视觉 token 爆炸；显存不足时将 batch 降为 4 或 1。搜索默认只展示 WeMM 余弦相似度严格大于 0.3 的结果，按相关度降序返回 16 个媒体；`mode=media` 按视频归集，`mode=scene` 按片段平铺，`limit` 分别表示媒体或片段 TopN。不使用 RRF、最大值归一化或词法分数混入排序。设置 `VIDEO_SEARCH_USE_IMAGE_EMBEDDING=true` 后，Go 会把抽取的本地帧通过 image modality 发给 Python；默认关闭，因为当前 hash fallback 不理解图像。任务状态默认保存到 `data/acquisition_tasks.json`，浏览器刷新后可继续看到历史任务；服务重启时，未完成任务会标记为“服务重启，任务未完成”，不会伪装成仍在运行。图像嵌入默认按 32 帧一批发送（可通过 `VIDEO_IMAGE_BATCH_SIZE` 调整），并在任务栏报告批次进度；单帧 ffmpeg 超过超时时间会终止，避免任务卡死。

采集任务提交前会对源文件计算 SHA-256 内容指纹。相同内容即使路径或文件名不同，也会复用已处理媒体或正在执行的任务，不会重复解析、抽帧和嵌入；文件内容发生变化后会创建新任务。旧版本索引若尚未保存指纹，则对同一 `local_path` 做兼容去重。

这里要区分两种能力：WeMM 可以直接接收 `{"video": "/path/to/video.mp4"}` 得到一条视频级向量，但这条向量只能回答“哪部视频相关”，不能定位命中的时间段。本 MVP 为了返回 `start/end/preview`，采用“本地视频 → 分镜边界 → 代表帧 → image embedding → 场景向量”；视频级 embedding 后续作为媒体级粗召回，场景向量继续负责时间定位。

## 数据格式

每个媒体对象至少需要 `title` 和一个 `scenes`：

```json
{
  "media_id": "movie-001",
  "type": "movie",
  "title": "示例电影",
  "year": 2024,
  "language": ["zh"],
  "description": "影片简介",
  "tags": ["动作"],
  "source_url": "https://example.com/movie-001",
  "scenes": [
    {
      "start": 0,
      "end": 10,
      "preview": "https://example.com/frame.jpg",
      "preview_path": "data/frames/frame.jpg",
      "caption": "一个人在雪地里奔跑",
      "subtitle": "我们必须回去"
    }
  ]
}
```

MVP 将每个 Storyboard/Preview frame 当作一个 pseudo scene；后续再替换为真实 shot detection 和 scene grouping。

## HTTP 接口

- `POST /v1/media`：写入或重新索引一部媒体及其全部伪场景。
- `GET /v1/media`：查看累计索引的媒体。
- `GET /v1/media/{media_id}`：查看媒体和场景。
- `GET /v1/media/{media_id}/frames/{filename}`：查看 Go 抽取的代表帧。
- `DELETE /v1/media/{media_id}/scenes/{scene_id}`：删除一个关键帧及其对应向量。
- `DELETE /v1/media/{media_id}`：删除媒体及其场景索引。
- `POST /v1/acquisitions`：提交服务端本地视频路径，异步执行解析、分镜、抽帧和嵌入。
- `GET /v1/acquisitions`、`GET /v1/acquisitions/{task_id}`：查看采集任务。
- `DELETE /v1/acquisitions/{task_id}`：停止未完成的采集任务。
- `POST /v1/files/validate`：批量检查服务端本地文件是否可读。
- `POST /v1/files/scan`：扫描服务端本地目录中的视频文件。
- `GET /v1/connectors/alipan/status`：查看阿里云盘登录状态（只返回公开账户信息）。
- `POST /v1/connectors/alipan/login/start`：创建扫码授权会话（默认 tickstep；可切换 official）。
- `GET /v1/connectors/alipan/login/{session_id}/qr`：取得授权二维码 PNG。
- `GET /v1/connectors/alipan/login/{session_id}/status`：轮询 tickstep 扫码状态。
- `POST /v1/connectors/alipan/login/complete`：用授权码完成 `oob` 登录。
- `GET /v1/connectors/alipan/login/callback`：配置可访问回调地址时，用 OAuth 回调完成登录。
- `POST /v1/connectors/alipan/logout`：删除本地阿里云盘凭据。
- `POST /v1/search`：按自然语言检索，可用 `type`、`year`、`language`、`limit`、`min_score`、`mode` 过滤。
- `GET /healthz`：查看索引数量和 embedding 服务状态。

采集任务遇到 `ffprobe` 无法解析、`Invalid NAL unit`、`Invalid data found`、`moov atom not found` 等错误时，会统一标记为“文件损坏或无法解码”。单个时间点没有画面时，处理器会先尝试附近时间点；附近位置仍全部无法输出帧，也会按不可解码文件处理。

## 下一步

1. 将 `FileStore` 替换为 PostgreSQL + Qdrant adapter，并保留 `IndexStore` 接口。
2. 增加媒体级 video embedding，形成“视频粗召回 → 场景精排”的两级索引。
3. 完成阿里云盘文件浏览、Range 读取和远程视频采集；登录 connector 已先落地，文件 connector 后续复用同一官方账户会话。
4. 增加来源 connector、批量任务队列、字幕分段和独立 text embedding。
4. 加入 scene grouping、Media entity resolution、fingerprint 和 Recall@10 benchmark。
