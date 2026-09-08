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

## 人脸识别增强 MVP

人脸身份链路与 WeMM 分离：Go 管理 Movie/Cast、reference face、阈值和 Scene 标签，Python Identity Service 只负责 InsightFace `buffalo_l` 的检测与 512 维 embedding。安装时复用现有 Hunyuan3D 环境，不重复安装 CUDA/Torch：

```bash
/home/zephyr/go/src/hunyuan3d/.venv/bin/python -m pip install -r python/requirements-identity.txt
```

启动身份服务：

```bash
IDENTITY_PORT=7003 scripts/start_identity.sh
```

然后重启 Go 服务并打开自动打标：

```bash
IDENTITY_ENABLED=true IDENTITY_ENDPOINT=http://127.0.0.1:7003 \
  VIDEO_SEARCH_IDENTITY_DB=data/identity.db \
  VIDEO_SEARCH_IDENTITY_FILE=data/identity.json \
  .build/search-server
```

可通过 `POST /v1/metadata/movies`、`POST /v1/metadata/movies/{id}/cast` 手工建立电影与演员 cast，也可配置 `TMDB_API_KEY` 后调用 `POST /v1/metadata/movies/{id}/sync`。正式入库任务先立即入队，worker 领取后使用与 `POST /v1/metadata/movies/prepare` 相同的准备服务：按文件名/标题定位 IMDb 影片，必要时同步完整 TMDB cast/profile，启用 Identity 时继续补齐去重演员的人脸向量；未 `ready` 的任务会记录失败原因，不阻塞批量提交。reference 图片使用 `POST /v1/persons/{id}/faces`，支持服务端本地路径或 URL；识别视频可调用 `POST /v1/media/{id}/persons/rebuild`。搜索页面的演员候选只来自已有 face vector 的去重 Person；搜索请求增加 `person` 或 `person_id` 后，会先按 `Scene.person_ids` 过滤，再做 WeMM 语义排序。

`IDENTITY_BACKEND=hash scripts/start_identity.sh` 只用于离线验证 HTTP 链路，输出的向量不具备真实人脸识别能力。完整设计、阈值原则和后续 benchmark 见 [`docs/face-recognition-mvp-plan.md`](docs/face-recognition-mvp-plan.md)，当前交接状态见 [`docs/handoff.md`](docs/handoff.md)。

### IMDb 电影元数据与演员人脸库

下载 IMDb 官方 bulk TSV 快照，并按 IMDb ID 将完整电影元数据、评分、演员角色关系、演员基础信息和别名/主创信息流式导入 SQLite；加上有界的 `--tmdb` 选择后，会同步每部电影的完整 TMDB cast、每位演员的 profile images，并通过已启动的 Identity Service 建立本地人脸向量：

```bash
scripts/download_imdb_datasets.sh
go build -o .build/catalog-sync ./cmd/catalog-sync
set -a; source .env; set +a
.build/catalog-sync \
  --imdb-basics data/imdb/title.basics.tsv.gz \
  --imdb-ratings data/imdb/title.ratings.tsv.gz \
  --imdb-principals data/imdb/title.principals.tsv.gz \
  --imdb-names data/imdb/name.basics.tsv.gz \
  --imdb-akas data/imdb/title.akas.tsv.gz \
  --imdb-crew data/imdb/title.crew.tsv.gz \
  --title-id-file data/imdb/title-ids.txt \
  --tmdb
```

`--title-id-file` 每行一个 `tt...`；也可用 `--min-year`、`--max-year`、`--max-movies` 做批次。IMDb-only 导入可以不设上限；`--tmdb` 必须有界。SQLite 会以 IMDb/TMDB ID 去重，重复运行会跳过已存在的 face vector，适合中断后继续。默认数据库为 `data/identity.db`；旧版 `data/identity.json` 会在首次启动时只读迁移并保留。IMDb catalog 已经导入后，重复执行 TMDB 批次可加 `--skip-imdb-import`，跳过再次扫描/写入 IMDb bulk 数据。

若 TMDB 直连不可用，让 Go worker 和 catalog-sync 的 TMDB 请求显式使用代理，例如：`TMDB_PROXY_URL=http://192.168.50.199:10810 .build/search-server` 或 `TMDB_PROXY_URL=http://192.168.50.199:10810 .build/catalog-sync ...`；媒体请求和 embedding 仍保持直连。

视频采集启用 `IDENTITY_ENABLED=true` 后，默认开启 `IDENTITY_LAZY_LOAD=true`：只为当前视频的 IMDb cast 获取缺失的 TMDB 演员头像，默认每人最多 8 张 `w500` 图片，并在稳定 profile 顺序上分散选择；同一 IMDb/TMDB 演员跨影片共享向量。TMDB profile 查询默认 4 路有界并发，并按 IMDb 演员名单提前过滤 TMDB credits；已成功读取的演员 profile 清单会持久化，后续重启不重复请求。离线 `catalog-sync --tmdb` 也默认使用 `w500`，可用 `TMDB_PROFILE_IMAGE_SIZE` 调整，避免无意下载 `original` 大图。图片仅作为临时人脸推理输入，成功后删除，只保留 512D 向量和来源元数据。TMDB 图片下载可使用 `TMDB_PROXY_URL` 或更具体的 `IDENTITY_REFERENCE_PROXY_URL`。如需保留图片用于人工审核，设置 `IDENTITY_KEEP_REFERENCE_IMAGES=true`。

若只验证 Go 闭环，不下载大模型，可省略上述 WEMM 环境变量，使用默认 hash fallback。

另开终端启动 Go 主服务：

```bash
cd video-semantic-search
VIDEO_FAST_MODE=true VIDEO_FRAME_WORKERS=8 \
  go run ./cmd/search-server
```

上传弹窗直接输入 Go 服务所在机器上的文件或目录路径，不依赖浏览器文件选择器。输入 `E:\\Movies\\movie.mp4`、`E:/Movies/movie.mp4` 或 `/mnt/e/Movies/movie.mp4` 后点击“检查路径”；文件会直接加入待处理列表，目录则显示“扫描目录”和递归选项。服务端会将 Windows 路径转换为 WSL 路径，原视频不会上传或复制。

页面默认勾选快速模式。采集任务由后台 worker 池顺序执行，默认同时处理 2 个任务，可用 `VIDEO_TASK_WORKERS` 调整；排队中的任务可随时取消，失败或已取消的记录可在页面上批量清理。快速模式按 `sample_interval` 均匀采样，最多 32 个画面；关键帧采样/准确模式最多 240 个画面。采样间隔越小，画面越密集、抽帧和 embedding 耗时越长。如果要完整检测每个静态切镜，取消页面勾选并使用准确模式，但耗时会随视频时长增长。

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

登录凭据只写入 `VIDEO_SEARCH_ALIPAN_CONFIG` 指定的本地文件（默认 `data/alipan_profiles.json`），文件权限为 `0600`，HTTP 响应和页面都不会返回 access token/refresh token。tickstep 模式依赖 `ALIYUNPAN_TICKSTEP_BROKER_URL`（默认 `https://api.tickstep.com`）；官方模式则只调用阿里云盘 OAuth 和 OpenAPI。服务会在 access token 到期前自动续期：官方模式使用 refresh token，tickstep 模式使用登录 ticket；后台每分钟主动检查，实际调用云盘 API 时也会再次检查。后续云盘文件操作会复用这套账户会话。

导入 JSONL 数据可以直接使用 Go ingest command：

```bash
go run ./cmd/ingest -file data/sample.jsonl
```

也可以调用 `POST /v1/media` 单条写入一个 JSON object。

已处理视频可以在管理弹窗中选择 `原样（模型默认）` 或 `压缩`，点击“重新构建向量”。这是异步任务，只重新读取现有关键帧，不重新解析视频、不重新抽帧；任务进度会出现在右下角任务栏。服务端也支持：

```bash
curl -s http://127.0.0.1:8000/v1/media/<media_id>/embeddings/rebuild \
  -H 'content-type: application/json' \
  -d '{"profile":"compressed"}'
```

管理弹窗顶部工具栏提供向量策略、全选/取消全选、重建和删除操作；勾选一个视频时按钮显示“重新构建向量”“删除整文件”，勾选多个时显示“批量重建向量”“批量删除”。批量重建接口为 `POST /v1/media/embeddings/rebuild`，批量删除接口为 `POST /v1/media/batch/delete`，二者都按 `media_ids` 逐项返回结果；删除会同步移除索引、关键帧和对应向量，但不会删除原视频。

重建前可用现有关键帧评估两种图片预处理的向量漂移；脚本只读数据，不修改索引：

```bash
scripts/compare_wemm_profiles.py --endpoint http://127.0.0.1:7001 --index data/index.json --sample-size 32
```

脚本默认要求所有样本的余弦相似度至少达到 `0.995` 才建议压缩，可用 `--threshold` 调整。Go 服务的默认图片 profile 是 `original`，也可用 `VIDEO_IMAGE_PROFILE=compressed` 配置新任务默认使用压缩 profile；已有视频不会自动改变。

搜索：

```bash
curl -s http://127.0.0.1:8000/v1/search \
  -H 'content-type: application/json' \
  -d '{"query":"夕阳下两个人骑摩托车","limit":16,"min_score":0.3,"mode":"media"}'
```

然后打开 <http://127.0.0.1:8000>。主页面只保留搜索与结果区域；右上角上传图标打开批量解析弹窗，弹窗采用左侧路径来源、右侧待处理清单、底部提交栏布局，支持全选/反选、清除所选和单条移除。文件路径检查通过后直接进入待处理清单；目录路径检查通过后，点击“扫描目录”才会把发现的视频加入清单，递归扫描选项位于其下方。管理图标打开已处理文件和关键帧管理。页面的本地视频输入是 Go 服务端路径，不会通过浏览器上传或复制原视频；处理时只在帧目录生成 JPEG 代表帧。API 接收到 `E:\\Movies\\movie.mp4` 或 `E:/Movies/movie.mp4` 时会自动转换为 WSL 的 `/mnt/e/Movies/movie.mp4` 再检查和处理。

默认配置：

身份元数据相关参数为 `VIDEO_SEARCH_IDENTITY_DB`（默认 `data/identity.db`）、`VIDEO_SEARCH_IDENTITY_FILE`（旧 JSON 迁移源）、`IDENTITY_MIN_REFERENCES`（默认 5）和 `IDENTITY_REFERENCE_MAX_PER_PERSON`（默认 8）。

图片向量重建支持 `VIDEO_IMAGE_PROFILE=original|compressed`；压缩 profile 的像素范围由 `WEMM_COMPRESSED_MIN_IMAGE_PIXELS` 和 `WEMM_COMPRESSED_MAX_IMAGE_PIXELS` 控制。Go 服务默认使用 `original`，已处理视频可以在管理弹窗中单独或批量选择 profile 重建。

- Go API：`127.0.0.1:8000`
- Python embedding：`127.0.0.1:7001`
- Python Identity：`127.0.0.1:7003`（启用身份链路时）
- Go 索引：`data/index.json`
- Identity SQLite：`data/identity.db`
- 抽取帧：`data/frames/<media_id>`
- embedding：`EMBEDDING_BACKEND=hash`、256 维；真实多模态检索使用 `EMBEDDING_BACKEND=wemm`、2048 维

可通过 `VIDEO_SEARCH_ADDR`、`VIDEO_SEARCH_INDEX`、`VIDEO_SEARCH_FRAME_DIR`、`VIDEO_SEARCH_TASK_FILE`、`VIDEO_SCENE_WORKERS`、`VIDEO_SCENE_OVERLAP`、`VIDEO_SCENE_SAMPLE_FPS`、`VIDEO_SCENE_DETECTION_WIDTH`、`VIDEO_FRAME_WORKERS`、`VIDEO_FRAME_WIDTH`、`VIDEO_FRAME_TIMEOUT`、`VIDEO_FFMPEG_HWACCEL`、`VIDEO_SCENE_REFINE_WINDOWS`、`VIDEO_FAST_MODE`、`VIDEO_MAX_SCENES`、`VIDEO_FAST_MAX_SCENES`、`VIDEO_REMOTE_FRAME_WORKERS`、`VIDEO_REMOTE_CHUNK_SIZE`、`VIDEO_REMOTE_CACHE_BYTES`、`VIDEO_IMAGE_BATCH_SIZE`、`EMBEDDING_ENDPOINT`、`EMBEDDING_TIMEOUT`、`EMBEDDING_DIMENSION`、`WEMM_IMAGE_PROMPT`、`EMBEDDING_BATCH_SIZE` 覆盖。`VIDEO_SCENE_WORKERS` 默认 4，`VIDEO_FRAME_WORKERS` 默认 4，`VIDEO_FRAME_WIDTH` 默认 640，`VIDEO_FRAME_TIMEOUT` 默认 45s；准确模式默认先用关键帧做低成本变化检测，再把关键帧时间作为切点候选，避免对整部电影启动大量随机 ffmpeg。若设置 `VIDEO_SCENE_REFINE_WINDOWS=true`，才会对候选窗口做 2 FPS、640 像素宽度的原始阈值精检，边界更精确但耗时明显增加。`VIDEO_FFMPEG_HWACCEL` 默认 `auto`，分镜检测和抽帧都优先尝试 CUDA，失败时自动回退 CPU；10-bit 视频会使用匹配的 `p010le` 下载格式，也可设为 `cuda` 或 `none`。设置 `VIDEO_FAST_MODE=true`，或在上传弹窗选择快速模式，会跳过静态切镜扫描，按 `sample_interval` 均匀采样，最多 32 个画面；关键帧采样/准确模式最多 240 个画面，未指定采样间隔时默认按 30 秒采样。这是交互式快速索引，可能漏掉很短的镜头，准确模式适合离线完整处理。`EMBEDDING_BATCH_SIZE` 默认 8；WeMM 图片不再由应用层注入 `min_pixels/max_pixels`，交给 WeMM processor 的默认策略处理；如果显存不足，优先将 batch 降为 4 或 1。搜索默认只展示 WeMM 余弦相似度严格大于 0.3 的结果，按相关度降序返回 16 个媒体；`mode=media` 按视频归集，`mode=scene` 按片段平铺，`limit` 分别表示媒体或片段 TopN。不使用 RRF、最大值归一化或词法分数混入排序。设置 `VIDEO_SEARCH_USE_IMAGE_EMBEDDING=true` 后，Go 会把抽取的本地帧通过 image modality 发给 Python；默认关闭，因为当前 hash fallback 不理解图像。任务状态默认保存到 `data/acquisition_tasks.json`，浏览器刷新后可继续看到历史任务；服务重启时，未完成任务会标记为“服务重启，任务未完成”，不会伪装成仍在运行。图像嵌入默认按 32 帧一批发送（可通过 `VIDEO_IMAGE_BATCH_SIZE` 调整），只控制每批大小，不限制总画面数，并在任务栏报告批次进度；单帧 ffmpeg 超过超时时间会终止，避免任务卡死。

远程（阿里云盘）视频不下载整个文件。容器类型由**首字节嗅探**判定（偏移 4 处是 `ftyp` → MP4，`1A 45 DF A3` → Matroska/WebM），而不是看云盘文件名后缀；MP4 用 `github.com/Eyevinn/mp4ff` 解析 `moov`，大 `moov` 默认按 1 MiB 分块、4 路并发 Range 拉取，并缓存解析后的索引；Matroska 用自研 EBML 解析器从 `SeekHead` 直接取 `Info`/`Tracks`/`Cues`，两者都只读元数据。抽帧时按索引**精确 Range** 下载单个 I 帧样本，MP4 转成 Annex-B、Matroska 的 VP8/VP9/AV1 封成单帧 IVF，再交给 `ffmpeg -f h264|hevc|ivf` 解码成 JPG。`VIDEO_REMOTE_CHUNK_SIZE`（默认 4194304）只用于索引不可用时回退的 FFmpeg 顺序读取，`VIDEO_REMOTE_CACHE_BYTES`（默认 100663296）是单次采集的区间缓存上限，超出按 LRU 淘汰；`VIDEO_REMOTE_MP4_MOOV_WORKERS`、`VIDEO_REMOTE_MP4_MOOV_CHUNK_SIZE` 控制 MP4 首次元数据下载，`VIDEO_REMOTE_INDEX_CACHE_DIR` 控制索引缓存目录。任务元数据里的 `remote_range.download_ratio`、`remote_container_index` 与 `remote_container_index_cache` 记录真实流量、索引摘要和缓存状态，详见 `docs/handoff.md`。

采集 worker 领取本地任务后才计算源文件 SHA-256 内容指纹；相同内容即使路径或文件名不同，也会复用已处理媒体或正在执行的任务，不会重复解析、抽帧和嵌入；文件内容发生变化后会创建新任务。旧版本索引若尚未保存指纹，则对同一 `local_path` 做兼容去重。这样任务提交不会因读完整视频或等待 TMDB 而阻塞。

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
- `GET /v1/media/{media_id}/stream`：播放原始视频。本地文件直接流出；云盘文件由服务端按 Range 代理，签名 URL 过期自动刷新，并带 4 MiB 磁盘块缓存与并行预取（聚合单连接限速），页面可从任意搜索结果画面起播到该画面抽帧的真实时间。缓存目录 `data/stream-cache/`（LRU，默认 2 GiB，`VIDEO_STREAM_CACHE_DIR=off` 禁用；上游并发 `VIDEO_STREAM_CONCURRENCY`，默认 8）。
- `GET /v1/media/{media_id}/frames/{filename}`：查看 Go 抽取的代表帧。
- `DELETE /v1/media/{media_id}/scenes/{scene_id}`：删除一个关键帧及其对应向量。
- `DELETE /v1/media/{media_id}`：删除媒体及其场景索引。
- `POST /v1/media/batch/delete`：批量删除选中的媒体、关键帧及其向量，body 为 `{"media_ids":["..."]}`。
- `POST /v1/acquisitions`：提交服务端本地视频路径，任务立即入队（`queued`）返回，不读取文件内容；由后台 worker 领取后先执行 IMDb/TMDB/人脸库准备，再处理视频。
- `POST /v1/acquisitions/batch`：单次请求批量入队多个采集任务，逐条返回创建结果与失败原因；不会等待 TMDB 或人脸库准备。
- `GET /v1/acquisitions`、`GET /v1/acquisitions/{task_id}`：查看采集任务。
- `DELETE /v1/acquisitions/{task_id}`：取消排队/运行中的任务；终态任务则移除记录。
- `POST /v1/acquisitions/batch/stop`：批量停止选中的排队/运行任务，body 为 `{"task_ids":["..."]}`。
- `POST /v1/acquisitions/batch/delete`：批量删除选中的已完成、失败或已停止任务记录，正在处理的任务需要先停止。
- `POST /v1/acquisitions/clear`：按状态批量移除终态任务记录；页面的“清理终态”会清理 `completed`、`failed` 与 `canceled`。
- `POST /v1/files/validate`：批量检查服务端本地文件是否可读。
- `POST /v1/files/inspect`：检查服务端本地路径，并返回文件/目录类型及可用性。
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
3. 用真实阿里云盘文件复测 MP4/Matroska 容器索引的实际流量（当前证据来自合成 fixture），并补齐 fragmented MP4、无 `Cues` 的 Matroska、laced block 和 MPEG-TS/AVI 等回退路径。
4. 增加来源 connector、批量任务队列、字幕分段和独立 text embedding。
4. 加入 scene grouping、Media entity resolution、fingerprint 和 Recall@10 benchmark。
