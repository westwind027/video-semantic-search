# Video Semantic Search MVP Handoff

更新时间：2026-09-03

## 1. 当前目标与架构

主架构是 Go，Python 只负责 embedding 推理。

```text
本地文件 / 阿里云盘文件
        ↓
Go acquisition manager
        ↓
容器字节嗅探 → MP4 / Matroska 索引解析（只读元数据）
        ↓
静态切镜检测或快速均匀采样
        ↓
按索引精确拉取 I 帧 sample → 提取为 JPG
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
- 远程容器由**首字节嗅探**识别（`ftyp` → MP4，`1A 45 DF A3` → Matroska/WebM），不再依赖云盘文件名后缀；文件名是用户数据，后缀经常缺失、写错或被改名。
- MP4 使用 `github.com/Eyevinn/mp4ff` 解析 `moov`：先用一个 32 KiB 头部窗口沿顶层 box 头跳转定位 `moov`（`mdat` 在前的非 faststart 布局也只花 16 字节），再把大 `moov` 拆成默认 1 MiB 的精确 Range，并发拉取后在内存中重组解码，取得视频轨道、时长、编码、每个 I 帧 sample 的偏移和长度；默认最多 4 路并发。
- MP4 解析后的索引会按远程内容指纹/文件身份和文件大小缓存到 `data/frames/.container-index-cache/`，缓存只保存关键帧偏移、时间戳和编码配置，不保存原视频或 `moov` 原始字节；重复处理命中缓存时跳过 MP4 元数据下载。任务元数据的 `remote_container_index_cache` 为 `stored`、`hit`、`miss` 或 `disabled`。
- Matroska/WebM 使用自研 EBML 索引器：从 `SeekHead` 直接取得 `Info` / `Tracks` / `Cues` 的位置，`Cues` 给出每个关键帧的 cluster 偏移；cluster 与 block 头**惰性解析**，只在真正抽帧时才定位到字节区间，所以上千个 cue 不会变成上千次请求。
- I 帧样本单独下载：MP4 转 Annex-B（含 `avcC`/`hvcC` 参数集），Matroska 的 VP8/VP9/AV1 封成单帧 IVF，再交给 `ffmpeg -f h264|hevc|ivf -i pipe:0 -frames:v 1` 解码；只下载一个关键帧的字节，不下载它周围的媒体数据。
- 远程视频使用 HTTP Range：`FetchRange` 请求**精确字节区间**，容器元数据与关键帧样本永不落到对齐 chunk 网格上；区间缓存按有序不重叠 block 组织，二分查找、部分命中只补缺口、LRU 淘汰并有内存上限。
- Range 请求支持并发去重（相同区间合并为一次请求）、重试、缓存、连接池和有界预取。
- 索引不可用（fragmented MP4、无 Cues、容器不支持、元数据不完整）时自动回退到只监听回环地址的 FFmpeg Range adapter，并在任务元数据中说明原因。
- 远程索引命中后默认走关键帧采样，最多 240 个代表画面；显式快速模式仍最多 32 个画面。未指定采样间隔时默认按 30 秒采样。远程关键帧抽取默认 8 路并发，可配置。
- 单个时间点抽帧失败不会直接取消整部视频；可继续处理其他画面并在任务元数据中记录警告。
- CUDA 抽帧失败会自动回退 CPU，并在处理器生命周期内禁用反复失败的 CUDA 尝试。
- 长视频 sample 时间戳由 `stts` 单次线性累加得到 64 位解码时间，既避免 mp4ff 的 32 位时间回绕，也避免逐帧调用 `GetDecodeTime` 的 O(K×E) 开销。
- 过期的云盘签名 URL 已增加 401/403 自动刷新回调；该改动需要重新编译并用真实视频复测。
- 阿里云盘 access token 会在到期前自动续期：官方 OAuth 使用 refresh token，tickstep 使用登录 ticket 的 refresh 接口；Go 服务后台每分钟检查，状态接口和实际云盘请求也会触发一次带并发保护的刷新。若 refresh token/ticket 已失效，状态接口会报告续期失败，需要重新登录。
- 相同内容去重分两层：提交时远程任务按云盘文件身份/`content_hash` 同步去重；本地文件的 SHA-256 延迟到 worker 领取任务时计算，与运行中任务和已完成媒体比对后直接折叠为 `completed`（`media_id` 指向已有媒体），不会重复解析、抽帧和 embedding。
- 采集任务采用**提交即入队**模型：`Submit` 只做参数校验并落盘任务记录，不读取文件内容（零读盘），页面可以一次性批量提交任意多个任务；`VIDEO_TASK_WORKERS` 个 worker 从队列顺序领取执行，默认 2，避免无上限并发导致 ffmpeg/embedding 互相拖垮。排队中的任务可随时取消（状态直接变为 `canceled`），不会占用 worker。
- 页面任务栏支持批量提交（单次 HTTP 往返）、勾选后批量停止排队/运行任务、批量删除已完成/失败/已停止任务，以及一键清理所有终态记录。对应接口为 `POST /v1/acquisitions/batch/stop`、`POST /v1/acquisitions/batch/delete` 和 `POST /v1/acquisitions/clear`。
- 上传弹窗采用左侧路径来源、右侧待处理清单、底部提交栏布局；用户输入 Windows 或 WSL 路径并点击“检查路径”，文件直接加入待处理清单，目录只登记为来源，点击“扫描目录”后才把视频加入清单，递归扫描选项位于其下方。待处理文件支持全选、反选、清除所选、单条移除，检查与“开始批量处理”固定在底部。
- 本地路径入口统一支持 WSL 转换：`E:\\Movies\\movie.mp4` 和 `E:/Movies/movie.mp4` 会转换为 `/mnt/e/Movies/movie.mp4`，文件校验、目录扫描和采集任务提交都会使用转换后的路径。
- B/S 页面不依赖浏览器暴露本机绝对路径；用户直接输入 Go 服务所在机器可访问的 Windows 或 WSL 路径，由 Go 服务统一转换、检查和处理。Go 服务和 Python embedding 服务仍分别独立启动。
- 管理弹窗的重建策略、全选/取消全选、重建和删除统一放在顶部工具栏；单选时显示“重新构建向量”“删除整文件”，多选时显示“批量重建向量”“批量删除”。详情区域只展示文件信息和关键帧，避免操作按钮挤压标题。批量删除会同步删除索引、关键帧目录和对应向量，但不会删除原视频。
- 任务状态写入 `data/acquisition_tasks.json`，刷新页面或重启服务后可以查看历史状态。

## 3. 远程流量：修复前的问题与修复后的实测

### 3.1 历史问题（修复前）

测试文件：`Inception.2010.1080p.BluRay.x265-RARBG.mp4`，远程大小约 2.31 GiB，时长约 8888 秒，视频编码 HEVC，1920×800。

- 32 个目标画面中 7 个成功，25 个产生警告。
- 已成功的 6 个走了直接 sample 解码，1 个走了普通远程 seek。
- 失败主要是抽帧期间远程 Range 返回 HTTP 403，随后一个末尾位置等待超时。
- 当时 Range 共下载约 50.6 MB，即**平均每个成功画面约 7 MB**。

50.6 MB / 7 帧这个数字直接暴露了两个放大来源，现已修复：

1. **I 帧下载放大 10～40 倍**：`RangeReader` 把所有读取对齐到固定 4 MiB chunk 网格，一个 100～500 KB 的 HEVC I 帧要连带拉取 4～8 MiB。现在关键帧样本走 `FetchRange` 精确区间，一个 12162 字节的 sample 就只下载 12162 字节、只发 1 次请求（`TestRemoteMP4IndexDownloadsOnlyMetadata` 逐字节断言）。
2. **元数据放大与投机预取**：原先用 `mp4.DecodeFile` 顺序解析并预取最多 4 个 chunk（16 MiB）。现在定位 `moov` 只读 box 头，`moov` 本体按精确区间分块并发拉取；解析结果还会持久化缓存。
3. **缓存无上限**：原先 chunk map 从不淘汰，长电影会把媒体数据钉在内存里。现在区间缓存有 LRU 与内存上限。
4. **MKV 完全不支持**：原先只按文件名判断 MP4，Matroska/WebM 一律退化成 FFmpeg 通用远程读取（每个时间点靠多次 seek 猜测，可能顺序读完整个文件）。现在有完整的 EBML 索引器。

### 3.2 修复后的实测流量（合成 fixture，`go test -v -run TestRemote`）

| 场景 | 文件大小 | Range 请求 | 下载字节 | 占比 |
|---|---:|---:|---:|---:|
| MP4 索引（faststart） | 1.28 MB | 1 | 32768 | 2.57% |
| MP4 索引（`moov` 在 `mdat` 之后） | 1.28 MB | 3 | 34577 | 2.71% |
| MP4 单个 I 帧（12162 字节） | — | 1 | 12162 | 精确等于 sample |
| Matroska/H.264 索引 | 987 KB | 2 | 65659 | 6.65% |
| Matroska 单个 I 帧（12949 字节） | — | 2 | 12973 | 只多 24 字节 cluster 探测 |
| WebM/VP9 索引 | 463 KB | 2 | 65766 | 14.2% |
| WebM 单个 I 帧（7282 字节） | — | 2 | 7300 | 只多 18 字节 cluster 探测 |
| 端到端采集 2 个画面（MP4/H.264） | 987 KB | 4 | 61918 | 6.27% |
| 端到端采集 2 个画面（Matroska/H.264） | 987 KB | 7 | 94858 | 9.61% |
| 端到端采集 2 个画面（WebM/VP9） | 463 KB | 7 | 80365 | 17.36% |

请求数与文件时长基本无关：索引阶段是常数级（MP4 1～3 次、Matroska 2 次），抽帧阶段 MP4 每个 sample 1 次、Matroska 每个新 cluster 2 次。Matroska 的两次请求是 128 字节的 cluster 探测（一次拿到 cluster 头、timestamp 和首个 block 头）加上 payload 缺口；探测窗口已经覆盖的那部分 payload 会从区间缓存命中，所以总字节只比 sample 本身多十几到几十字节。Matroska 的索引字节量里 64 KiB 是固定的头部窗口（一次读入 `SeekHead`/`Info`/`Tracks`），因此小文件占比偏高、真实电影（GB 级）会降到千分之一以下。

真实云盘文件仍需用最新二进制复测：合成 fixture 验证的是**协议与放大倍数**，不验证服务端限流、签名 URL 过期和网络抖动。

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
- WeMM 图片尺寸不再由应用层注入 `min_pixels/max_pixels`，使用模型 processor 的默认策略；显存不足时只调整 `EMBEDDING_BATCH_SIZE`。

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
VIDEO_FAST_MODE=false \
VIDEO_MAX_SCENES=240 \
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
| `VIDEO_MAX_SCENES` | `240` | 关键帧采样/准确模式最多画面数 |
| `VIDEO_FAST_MAX_SCENES` | `32` | 快速均匀采样最多画面数 |
| `VIDEO_FRAME_WORKERS` | `4` | 普通抽帧并发数 |
| `VIDEO_REMOTE_FRAME_WORKERS` | `8` | 远程 MP4 直接 sample 抽帧并发数 |
| `VIDEO_FRAME_WIDTH` | `640` | JPG 输出宽度 |
| `VIDEO_FRAME_TIMEOUT` | `45s` | 单个抽帧尝试超时 |
| `VIDEO_FFMPEG_HWACCEL` | `auto` | `auto`、`cuda` 或 `none` |
| `VIDEO_SCENE_WORKERS` | `4` | 准确模式分镜窗口并发数 |
| `VIDEO_SCENE_REFINE_WINDOWS` | `false` | 是否对候选窗口做精细切镜检测 |
| `VIDEO_REMOTE_CHUNK_SIZE` | `4194304` | FFmpeg 顺序读取的对齐 chunk（字节）；容器元数据与关键帧样本**不使用**该网格，始终是精确区间 |
| `VIDEO_REMOTE_CACHE_BYTES` | `100663296` | 单次采集的 Range 区间缓存上限（字节），超出后按 LRU 淘汰 |
| `VIDEO_REMOTE_MP4_MOOV_WORKERS` | `4` | MP4 大 `moov` 分块下载并发数；遇到 429/403 或限流时可降为 2 |
| `VIDEO_REMOTE_MP4_MOOV_CHUNK_SIZE` | `1048576` | MP4 `moov` 分块大小（字节）；只影响元数据首次下载 |
| `VIDEO_REMOTE_INDEX_CACHE_DIR` | `data/frames/.container-index-cache` | 已解析 MP4 索引缓存目录；设为空可关闭缓存 |
| `VIDEO_TASK_WORKERS` | `2` | 同时执行的采集任务数；本地大视频多路并发会争抢 ffmpeg/CPU，云盘任务还要受远端限流约束 |
| `VIDEO_IMAGE_BATCH_SIZE` | 由引擎决定 | Go 发送图片 embedding 的批大小 |
| `EMBEDDING_ENDPOINT` | `http://127.0.0.1:7001` | Python 服务地址 |
| `EMBEDDING_TIMEOUT` | `5m` | Go 调用 embedding 超时 |

远程视频性能优先时建议：

```bash
VIDEO_FAST_MODE=true \
VIDEO_MAX_SCENES=240 \
VIDEO_FAST_MAX_SCENES=32 \
VIDEO_REMOTE_FRAME_WORKERS=8 \
VIDEO_FRAME_WIDTH=640 \
VIDEO_FFMPEG_HWACCEL=auto
```

如果远端服务出现 429、403 或带宽抖动，将 `VIDEO_REMOTE_FRAME_WORKERS` 调低到 `4`；如果 Range 延迟高且没有限流，可以尝试 `12`，不建议无限增加。

`VIDEO_REMOTE_CHUNK_SIZE` 只影响回退到 FFmpeg 通用远程读取时的顺序吞吐：调大它对索引抽帧没有任何帮助（那些读取已经是精确区间），反而会在回退路径上一次拉取更多无用数据。`VIDEO_REMOTE_MP4_MOOV_WORKERS` 和 `VIDEO_REMOTE_MP4_MOOV_CHUNK_SIZE` 只影响 MP4 首次索引；默认 4 路 × 1 MiB，若云盘限流则降低并发，若单路带宽低且没有 429 可逐步提高到 8。`VIDEO_REMOTE_INDEX_CACHE_DIR` 命中后不会重新读取 `moov`，但仍会按需 Range 拉取代表关键帧。快速采样最多 32 个画面，关键帧采样/准确模式最多 240 个画面；两者仍受 `sample_interval` 和视频时长共同影响。

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

批量提交（单次请求入队多个任务，逐条返回各任务创建结果与失败原因）：

```bash
curl -fsS -X POST http://127.0.0.1:8000/v1/acquisitions/batch \
  -H 'Content-Type: application/json' \
  --data '{"items":[{"source":"alipan","drive_id":"<drive-id>","file_id":"<file-id-1>","type":"movie"},{"source":"alipan","drive_id":"<drive-id>","file_id":"<file-id-2>","type":"movie"}]}'
```

查看实际抽帧方式、Range 流量和每个 sample 的定位：

```bash
curl -s http://127.0.0.1:8000/v1/media/<media-id> | jq '.metadata'
```

重点字段：

- `remote_range.requests` / `remote_range.bytes_downloaded`：实际 Range 请求和下载量。
- `remote_range.cached_blocks` / `remote_range.cached_bytes`：结束时区间缓存里的 block 数与字节数（受 `VIDEO_REMOTE_CACHE_BYTES` 限制）。
- `remote_range.download_ratio`：`bytes_downloaded / size`，即真正下载了远端文件的多大比例。索引命中时远小于 1；接近 1 说明回退到了 FFmpeg 顺序读取。
- `remote_container_index`：容器索引摘要，包含 `container`（`mp4` / `matroska`）、`keyframes`、`codec`、`duration`、`first_offset`、`last_end`；MP4 额外有 `moov_bytes` 与 `nal_length_size`，Matroska 额外有 `codec_id`、`cues_bytes`、`timestamp_scale`、`segment_data`。
- `frame_extraction_methods`：`indexed_sample`（直接解码索引里的 I 帧）、`indexed_proxy`（用索引时间戳做 FFmpeg seek 锚点）、`proxy_exact`（无索引时的普通 seek）的数量。
- `frame_extraction_sources`：scene index、sample number、sample timestamp、offset、size 和摘要。
- `frame_extraction_warnings`：局部画面失败原因。

健康的远程任务应该看到 `frame_source` 为 `remote_mp4_keyframe_sample` 或 `remote_matroska_keyframe_sample`，`frame_extraction_methods` 以 `indexed_sample` 为主，`download_ratio` 在千分位量级。

## 7. API 和页面

- `GET /`：搜索页面。
- `POST /v1/acquisitions`：创建异步本地或阿里云盘采集任务，立即返回 `queued`，不读取文件内容。
- `POST /v1/acquisitions/batch`：批量创建采集任务（202），返回 `tasks` 与按提交序号排列的 `failures`。
- `GET /v1/acquisitions`、`GET /v1/acquisitions/{id}`：任务列表和详情。
- `DELETE /v1/acquisitions/{id}`：取消未完成任务（queued/running → `canceled`）；终态任务则移除该条记录。
- `POST /v1/acquisitions/batch/stop`：批量停止选中的 queued/running 任务，body 为 `{"task_ids":["..."]}`。
- `POST /v1/acquisitions/batch/delete`：批量删除选中的 completed/failed/canceled 任务记录，正在处理的任务不会被删除。
- `POST /v1/acquisitions/clear`：批量移除终态任务记录，body 可指定状态；页面“清理终态”会指定 `completed`、`failed`、`canceled`，返回 `{"removed":n}`。
- `GET /v1/media`、`GET /v1/media/{id}`：已处理文件和场景。
- `POST /v1/media/{id}/embeddings/rebuild`：异步重建单个视频的向量，body 为 `{"profile":"original"}` 或 `{"profile":"compressed"}`；只读取已提取关键帧，不重新解析视频。
- `POST /v1/media/embeddings/rebuild`：批量异步重建选中视频的向量，body 为 `{"media_ids":["..."],"profile":"original"}` 或 `{"profile":"compressed"}`；返回多个任务和逐项失败信息。
- `POST /v1/media/batch/delete`：批量删除选中的媒体，body 为 `{"media_ids":["..."]}`；返回 `deleted` 和逐项 `failures`，同时清理关键帧目录。
- `GET /v1/media/{id}/frames/{filename}`：查看 JPG。
- `DELETE /v1/media/{id}/scenes/{scene_id}`：删除关键帧和向量。
- `DELETE /v1/media/{id}`：删除整部媒体及其向量。
- `POST /v1/search`：语义搜索，支持按媒体或片段返回。
- `GET /healthz`：服务和索引健康检查。

搜索排序使用 embedding 相似度降序，默认返回 Top 16；页面支持按视频归集或按片段平铺。当前搜索阈值默认大于 0.3，可由请求覆盖。

向量重建的图片 profile：`original` 使用 WeMM 模型默认图片处理，`compressed` 使用 `WEMM_COMPRESSED_MIN_IMAGE_PIXELS` / `WEMM_COMPRESSED_MAX_IMAGE_PIXELS` 限制像素预算。默认 profile 为 `original`，Go 服务可通过 `VIDEO_IMAGE_PROFILE` 配置新采集任务。管理弹窗中可对单个或勾选多个已处理视频发起重建，任务会复用右下角任务栏。用 `scripts/compare_wemm_profiles.py` 可从现有关键帧计算两种 profile 的逐图余弦相似度；脚本不修改索引。

## 8. 验证命令

```bash
cd /home/zephyr/go/src/video-semantic-search
go test ./...
go test -race ./internal/source ./internal/acquisition
go vet ./...
```

远程流量上界由这几个测试守住，改动 Range 或索引代码后先看它们（需要本机 `ffmpeg`，缺少编码器时自动 skip）：

```bash
go test ./internal/source -run TestFetchRange -v
go test ./internal/acquisition -run 'TestRemote|TestEBML|TestBuildContainerIndex|TestExtractFrameFromIndexed' -v
```

- `internal/source/range_reader_test.go`：精确区间不放大、区间缓存合并、并发去重、LRU 淘汰、未知大小发现、签名 URL 刷新。
- `internal/acquisition/container_index_test.go`：EBML vint/元素头/子元素遍历的纯单元测试，容器嗅探分派，Matroska/WebM 索引的请求数与字节上界，H.264/VP9/AV1 三条 elementary 解码路径。
- `internal/acquisition/processor_test.go`：MP4 两种 `moov` 布局的流量上界、单个 I 帧逐字节断言，以及 MP4/MKV/WebM 端到端采集（`ffprobe` 被换成 `exit 99` 的假脚本，确保索引真的取代了探测）。

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

1. 用最新二进制重新跑当前 Inception 文件（HEVC MP4），确认 403 后能继续取样、后段画面不重复，并把 `download_ratio` 与修复前的 50.6 MB 对比记录到本文档 §3.2。
2. 补一个真实 Matroska/WebM 云盘文件的验证：当前 MKV 路径只有合成 fixture 级证据（`TestRemoteProcessUsesContainerIndexBeforeFFprobe`、`TestRemoteMatroskaIndexDownloadsOnlyMetadata`），尚未在真实电影上跑过。
3. 为索引阶段增加实时 Range 字节数、当前阶段耗时和阶段超时，避免页面长时间停在“读取容器索引”而没有反馈；目前已经有 MP4 分块并发和索引缓存，但还没有逐阶段耗时字段。
4. 根据实际服务端限流测试 `VIDEO_REMOTE_FRAME_WORKERS=4/8/12`，记录总耗时、成功率、Range 流量和 GPU 利用率。
5. 对真实批量提交（几十个本地大文件 + 多个云盘文件）压测队列：确认 worker 数与 ffmpeg 并发、GPU 占用的平衡，并验证排队中取消、重复内容折叠在多 worker 下的行为。
6. 补齐剩余容器与异常路径：fragmented MP4（`mvex`）、无 `Cues` 或 `Cues` 不带 `CueRelativePosition` 的 Matroska、laced block、多视频轨选择、MPEG-TS/AVI 容器；目前这些情况会回退到 FFmpeg 通用读取或报告局部不可解码。
7. 将当前 `FileStore` 替换为 PostgreSQL + Qdrant，并增加视频级粗召回和场景级精排。

不要把 `.env`、`data/alipan_profiles.json`、下载 URL、access token 或 refresh token 提交到仓库。
