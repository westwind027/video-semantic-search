# Video Semantic Search MVP Handoff

更新时间：2026-09-08

## 0. 最新进展

当前 `HEAD` 为 `25375d1`（`fix: 修复 MKV 关键帧采样失败导致的远程提取卡死`）。原视频搜索能力基线为 `b4fcf20`，身份/元数据组件及本交接文档仍是工作区未提交改动。最近一轮功能已经完成并验证：

- 页面保持纯 B/S，不依赖 Electron 或浏览器文件选择器；用户直接输入 Windows 路径（如 `E:/Movies/movie.mp4`）或 WSL 路径，Go 服务负责路径转换、文件/目录判断和后续处理。
- 上传与处理流程支持单文件检查后直接入队，目录检查后再按递归选项扫描视频。
- 任务队列支持勾选、全选/反选、批量停止排队/运行任务、批量删除已完成/失败/已停止任务，以及清理全部终态记录。
- 新增任务接口：`POST /v1/acquisitions/batch/stop`、`POST /v1/acquisitions/batch/delete`；新增路径检查接口：`POST /v1/files/inspect`。
- 2026-09-08 已将影片准备从提交请求中解耦：本地/阿里云盘入口现在只把任务写入任务持久化文件并立即入队，worker 领取后调用与 `POST /v1/metadata/movies/prepare` 共用的 `MoviePreparationService`，再执行抽帧、人物识别和 WeMM embedding。TMDB/Identity 慢或暂时失败不会阻塞批量提交；准备失败会落为 `failed` 任务并保留具体原因，便于修复后重试。
- 任务状态已增加 SSE 推送：`GET /v1/acquisitions/events` 连接后先发送快照，再推送增量状态变化；前端不再每 2 秒轮询任务，断线或事件缓冲溢出时由 10 秒低频刷新恢复一致状态。
- 本地任务领取后会先切换为 `running / hashing_content` 并通过 SSE 展示“正在计算文件指纹”；完整 SHA-256 计算与 IMDb/TMDB/人脸库准备并行，避免多 GB 文件在指纹阶段长时间显示为“等待处理”。指纹仍在去重和视频处理前完成，保留内容级去重语义。
- 已执行 `go test ./...`，全部通过；当前 `data/index.json` 持久化状态为 2 个视频、267 个画面。本次检查时 Go 服务已监听 `:8000`、WeMM 已监听 `:7001`；Identity tagging 仍按默认配置关闭，启动方式见 §4。

删除任务记录只影响任务历史，不会删除原视频、关键帧或向量；批量删除接口也会拒绝仍在处理中的任务。

最近新增人脸识别增强组件（当前改动尚未单独提交）：

- 新增独立 Python Identity Service，默认 InsightFace `buffalo_l`，只负责检测、对齐和输出 512 维人脸向量；Go 不引入 InsightFace/ONNX Runtime。`IDENTITY_BACKEND=hash` 仅用于离线 API 冒烟测试，不具备真实身份识别能力。
- 新增 `internal/identity` 元数据域，生产默认使用 SQLite `data/identity.db` 持久化 Movie、Person、MovieCast、PersonImage、FaceVector 和 ScenePeople；旧 `data/identity.json` 只作为一次性迁移源保留，不与 WeMM 的 `data/index.json` 混用。SQLite 对 IMDb/TMDB 外部 ID 建唯一索引，并在迁移/写入时合并重复实体。
- 新增 TMDB Movie/Cast 同步能力和保守的文件名标题/年份解析；TMDB 只在配置 `TMDB_API_KEY` 并调用同步接口时访问，客户端请求语言固定为 `en-US`。
- reference image 支持本地路径或 URL；本地路径直接复用，URL 会下载到 `data/identity-faces/<person_id>/`，要求恰好检测到一张脸且检测分数达标后才写入 face vector。
- 现有 Scene 代表帧可以参与 cast 限定的人物识别；惰性加载和 `catalog-sync` 默认每人最多建立 8 张 reference，匹配器最多读取 `FACE_REFERENCE_MAX_PER_PERSON`（默认 10）张并取 top-3 分数均值，再经过阈值和 margin 双重判定。低检测分或不确定结果保持 unknown，不强行贴标签。
- 2026-09-08 性能检查发现旧 Identity 启动路径因缺少 Python 环境的 CUDA/cuDNN 动态库路径而实际使用 `CPUExecutionProvider`；`scripts/start_identity.sh` 现在自动导出 NVIDIA wheel 的 `lib` 目录，健康检查必须确认包含 `CUDAExecutionProvider`。同一批 20 张图实测 CUDA 热身后单图约 0.02~0.08 秒，历史 CPU 路径约 0.6~1.7 秒/场景。
- Scene 人脸推理新增可选 `BatchClient`：内置 PythonClient 默认按 `IDENTITY_SCENE_BATCH_SIZE=8` 发送 `/v1/faces/embed-batch`，旧 Identity 服务自动回退到单图接口；ScenePeople 在 SQLite/FileStore 中也按整部影片一次事务提交。当前 CUDA 热身后的 32 张图实测逐张约 2.43 秒、批量约 0.62 秒。批量接口仍在 Python 侧串行保护 InsightFace 推理，主要收益来自减少 HTTP/JSON 往返，后续可再评估真正的模型级 batch。
- 2026-09-08 TMDB 下载链路完成性能收敛：`SyncMovieByIMDbID` 对同一 TMDB 演员只请求一次 profile，并默认以 `TMDB_PROFILE_WORKERS=4` 做有界并发（上限 16）；同一客户端的并发重复请求由 single-flight 合并。入库/prepare 路径使用 IMDb actor/actress 名称先过滤 TMDB credits，非 IMDb Person 不再请求 profile。profile 图片默认使用 `w500` 而不是 `original`，可通过 `TMDB_PROFILE_IMAGE_SIZE` 调整。`ApplyTMDBResult` 的 Person/PersonImage 写入改为批量事务；惰性加载会持久化完整 profile 清单和 `tmdb_profile_images_loaded` 标记，重启后不重复拉取已成功的清单；同一影片的 reference ingest 也合并为一次有界任务。`catalog-sync --skip-imdb-import` 可在 IMDb 已完成导入后跳过重复 bulk 扫描，SQLite 生产路径也不再按影片重复写 IMDb Person。

本轮已补齐 IMDb/TMDB 演员数据采集组件：`cmd/catalog-sync` 流式读取 IMDb `title.basics`、`title.ratings`、`title.principals`、`name.basics`、`title.akas` 和 `title.crew`；以 IMDb ID 作为影片和人物稳定主键。导入器把完整 IMDb 电影元数据、评分、别名、主创、角色和 actor/actress 关系写入 SQLite，不把全量目录堆在 Go 内存中。开启 `--tmdb` 后，逐部通过 IMDb ID 解析 TMDB 影片并读取完整 cast/profile 清单，但只将能绑定到 IMDb cast 的演员 profile 作为 reference 图片进入 Identity Service 做单脸过滤和 512D 向量化。当前工作区 `data/imdb/` 六个 TSV 均已下载并通过 gzip 校验。
- 新增 `scripts/download_imdb_datasets.sh`：从 IMDb bulk snapshot 断点下载六个 TSV 文件，使用 `.part` 临时文件原子落盘；检测到 `aria2c` 时使用多连接下载，并且只续传带 aria2 控制文件的 partial，避免与旧 `curl` partial 混用。
- 旧 `data/identity.json` 快照曾有 34,871 条 Movie、138,911 位 Person、186,262 条 cast 关系；全量 IMDb-only 导入初始写入 756,047 部 Movie、1,358,781 位 Person、4,137,737 条 cast/角色关系、4,969,377 条别名、162 张 reference、158 个 face vector，`schema_meta.imdb_imported` 已记录。此前的 34,870 部只是历史 JSON 子集，主要集中在 2025/2026 年；现在已完成当前 `title.basics.tsv.gz` 中全部 756,047 条 `movie` 行的导入。此次没有执行 TMDB，影片的 TMDB cast/profile 继续由 worker 领取任务后的 `/prepare` 惰性处理。Phase 2 的“至少 100 位演员、每人 5~20 张”尚未达标。
- TMDB 头像链路已经用显式 `TMDB_PROXY_URL` 和 `buffalo_l` + CUDA 做过真实调用验证；全量同步仍需配置 `TMDB_API_KEY`、控制每人图片数和批次，不应无上限调用 TMDB、下载头像或保存原图。
- 身份采集已调整为“向量优先、头像临时化”：`ReferenceIngestor` 默认删除 TMDB 下载的临时头像，只保存向量、来源 URL、图片尺寸、质量、状态和稳定 image ID；`IDENTITY_KEEP_REFERENCE_IMAGES=true` 才保留文件。搜索服务启用 `IDENTITY_LAZY_LOAD=true` 后，仅在处理某个视频且其 cast 缺少向量时，惰性获取 TMDB 电影元数据和演员头像，默认每人最多 8 张（`IDENTITY_REFERENCE_MAX_PER_PERSON`），同一演员通过 IMDb/TMDB ID 去重，向量跨影片复用。服务启动时还会清理旧版本遗留的远程头像 `local_path`，但不会清理本地参考图。
- `ReferenceIngestor` 对头像下载和推理使用有界 worker；已存在的同一 image/vector 会跳过，URL 图片落盘到 `data/identity-faces/<person_id>/`，失败、多人脸和低检测分数保持独立状态，重启 `catalog-sync` 可继续。
- 当前命令支持 `--title-id-file`、`--min-year`、`--max-year`、`--max-movies` 控制批次；不设置 title ID 时可以按年份/数量扫描 IMDb 电影。IMDb-only 导入可以全量运行并直接写 SQLite；带 `--tmdb` 的 enrichment 仍要求有界选择，以控制 TMDB 请求和头像推理成本。
- 新增 `POST /v1/metadata/movies/prepare` 作为可复用的影片准备接口：按 `movie_id`、标题/年份或文件名解析 IMDb Movie；缺少完整 TMDB cast/profile 时自动同步并通过 IMDb cast 绑定已有演员；启用 Identity 时继续补齐每位去重演员的 face vector，只有全部达到 `IDENTITY_MIN_REFERENCES`（默认 5）才返回 `ready=true`。前端本地文件和阿里云盘批量入库不再逐部等待该接口，而是先立即提交任务；worker 领取任务后调用同一准备服务，未 ready 的任务标记失败并保留原因。
- IMDb 的 `title.principals` 中 `actor/actress` 关系和对应 `name.basics` 是唯一的 Person/cast 来源；TMDB 只通过英文 `en-US` 影片查询补充已有 IMDb Person 的 TMDB ID、英文显示信息和 profile image，不创建 TMDB-only Person，也不覆盖 IMDb 角色关系。同一演员跨电影复用一个 IMDb Person 和同一组 reference/face vectors。
- `GET /v1/metadata/persons?q=...` 只返回“已经抽帧的媒体 → 已解析 IMDb Movie → IMDb actor/actress cast”范围内、且至少有一个 face vector 的去重演员；未抽帧影片或未完成 face bank 的人员不会出现在搜索候选中。2026-09-06 又增加了启动时的身份修复：旧 JSON 迁移产生的无 IMDb/TMDB ID 人物，只有在同名且唯一对应一个外部身份时才会合并，并迁移其 cast、头像、向量和场景关联；多个真实外部身份同名时保持独立。启动时还会删除历史 TMDB-only/无 IMDb Person 及其 cast、头像、向量和场景关系；删除前应保留 SQLite 备份。演员首屏候选上限提高到 100，前端也按外部身份做防御性去重。
- 搜索页新增“影片”可搜索下拉框，候选只来自当前已完成抽帧的媒体库；选择后通过 `media_id` 把语义搜索限定到该视频。结果卡片新增“简介”和“人员”标签气泡，点击后按需读取 IMDb Movie/cast，并在弹窗中展示简介或去重后的演员角色信息。
- 2026-09-06 启动清理实测删除 34 个无 IMDb Person、483 条非权威/孤儿 cast、111 张 PersonImage、108 个 FaceVector；当前库为 1,358,746 个 IMDb Person、4,137,254 条 `imdb_principals` cast、51 张 PersonImage、50 个 FaceVector、0 个 TMDB cast。清理前在线备份保存在 `data/identity.db.pre-imdb-person-cleanup-20260906.sqlite`。
- `Scene.person_ids` 已进入主索引，语义搜索支持 `person_id` 或精确演员名 `person` 过滤；按片段返回时会带回 `person_ids`。
- 新增 `POST /v1/media/{id}/persons/rebuild` 异步重建人物标签，不重新抽帧、不重算 WeMM；管理详情中提供“重建人物标签”按钮。
- 新增 Identity 运行脚本 `scripts/start_identity.sh`；自动视频打标默认关闭，启用方式见 §4.3。
- 本轮验证包括 `go test ./...`、`go vet ./...`、身份/API/索引/搜索/采集包 `go test -race`、Python 编译和脚本语法检查；hash 后端 HTTP 冒烟通过。共享 Hunyuan3D 环境已补装 InsightFace，真实 `buffalo_l` + CUDA provider 已完成启动验证。

本次实际运行验证（2026-09-05）：Identity 已加载 CUDA provider 和 `buffalo_l`。基于现有《当幸福来敲门》235 个场景和 `Tenet` 32 个场景，`POST /v1/media/{id}/persons/rebuild` 均 completed；Pursuit 有 73 个场景命中 9 位演员，Tenet 有 10 个场景命中 John David Washington、Elizabeth Debicki。WeMM 场景向量为 2048D，Identity 向量为 512D，远程 TMDB 头像临时目录无残留。当前持久化索引已是 2 部媒体/267 个场景、83 个场景带人物标签；历史验证中的其他样例和任务记录已不在当前索引/任务文件中。

新增**从画面起播**能力：搜索结果卡片中的任意命中画面可以直接点开播放器，并 seek 到**该画面抽帧的真实时间戳**（`Scene.PreviewTime`）：容器索引采样抽的是目标时间前最近的关键帧，与场景起始时间可能相差半个场景长度；处理器现把实际帧时间写入场景（旧索引在搜索时从 `metadata.frame_extraction_sources` 按帧号回填），搜索结果带 `preview_time` 字段，前端优先用它起播。播放地址为 `GET /v1/media/{id}/stream`：本地视频用 `http.ServeContent` 从原路径流出；阿里云盘视频由 Go 服务反向代理——浏览器永不接触签名 URL，签名 URL 短期缓存（10 分钟）并在上游 401/403 时自动刷新重试。代理端带**磁盘块缓存**（`internal/api/stream_cache.go`）：按 1 MiB 块落盘到 `data/stream-cache/`，未命中块流式转发给浏览器并同时写缓存（起播只需一次 RTT），读者离开后由后台接管下完当前块；后续块并行预取（预取最多占 4 个上游槽，浏览器请求优先），播放请求还会**优先预热文件尾块**（MKV 的 Cues 索引在尾部，浏览器读完头部会跳读尾部，实测预热后尾读从 ~40s 降到 3ms）；每媒体上游并发闸默认 8 路（`VIDEO_STREAM_CONCURRENCY` 可调）聚合单连接限速（实测单连接仅 ~100 KB/s）；响应中途的上游瞬时失败会从当前字节重建 reader 续传（最多 3 次），避免浏览器收到 ERR_CONTENT_LENGTH_MISMATCH；总缓存体积 LRU 淘汰（`VIDEO_STREAM_CACHE_BYTES`，默认 2 GiB；`VIDEO_STREAM_CACHE_DIR=off` 禁用）。**Web 扫码登录（Web token 免手工获取，2026-09）**：`ALIYUNPAN_WEB_REFRESH_TOKEN` 不再是唯一途径——页面「扫码登录」按钮现在**同时创建两个扫码会话并并排展示两个二维码**：① tickstep（云盘 API）与 ② web（云端转码，`POST /v1/connectors/alipan/login/web/start`；账号已有 web token 时自动隐藏第二码），各自独立轮询，两个都完成（或第二码被跳过）后才进入云盘目录；由 passport 接口（`passport.aliyundrive.com/newlogin/qrcode/generate.do` + `query.do`，实测可用）生成二维码并轮询 qrCodeStatus（NEW/SCANED/CONFIRMED/EXPIRED/CANCELED），CONFIRMED 时 base64 解码 bizExt 取 `pds_login_result.refreshToken`，经 `GetAccessTokenFromRefreshToken` 换取 Web token 后自动持久化（profile `web_refresh_token`）并激活 webClient（`internal/alipan/weblogin.go`）。后台自动刷新：进程内 10 分钟 tick 检查，access token 到期前 30 分钟内自动用轮换 refresh token 换新（自举：重启后无 token 时也会从已存 refresh token 恢复），扫码确认后手机若有安全验证提示按提示操作即可。
**云端转码 HLS 播放（云盘视频首选路径，2026-09）**：原文件单连接被限速（~100 KB/s）且并发下载触发 403，改用阿里云盘 `get_video_preview_play_info`（category=live_transcoding）**云端转码**：返回各清晰度 m3u8（H.264/AAC，浏览器原生解码，顺带解决 MKV/HEVC 不支持问题），分片走 CDN 直连实测 ~7 MB/s（较原文件快约 70 倍），任意 seek 秒开。注意：**官方 OpenAPI 无此接口**，必须走网页版 Web token 体系（tickstep/aliyunpan-api 库 v0.2.9）——用浏览器登录取得的 RefreshToken 配置 `ALIYUNPAN_WEB_REFRESH_TOKEN`（.env），Manager 懒初始化 WebPanClient（AppId `25dzX3vbYqktVxyX` + CreateSession），web refresh token 轮换后自动持久化回 `data/alipan_profiles.json` 的 `web_refresh_token` 字段；CDN 对 m3u8/分片做 Referer 签名校验（必须 `Referer: https://www.aliyundrive.com/`，浏览器 JS 无法设置该头），故由服务端代理：`GET /v1/media/{id}/transcode` 返回代理 playlist 路径，`/transcode/playlist` 拉取 m3u8 并把每个分片改写到 `/transcode/proxy?u=...`（带 Referer/UA 转发，host 白名单 aliyundrive.net/alipan.com 防 SSRF，嵌套 playlist 递归重写），签名 URL 按 media 缓存 10 分钟（`internal/api/transcode.go`）。前端用 hls.js（jsDelivr CDN）加载代理 playlist，转码不可用/未完成时自动回退原文件 /stream 块缓存路径。
纯 JS 自研播放器（无第三方库），提供播放/暂停、进度、音量、倍速和全屏；HEVC/H.265 编码或 MKV 容器浏览器不支持时给出明确提示（当前索引的 `001.mkv` 是 MKV 容器，Chrome/Edge 对 H.264 编码的 MKV 部分支持，播放取决于编码而非播放器）。

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

身份增强链路作为可选旁路：

```text
IMDb Movie + actor/actress cast ──→ English TMDB lookup (en-US)
                                      ↓ existing IMDb Person images
                              Python Identity Service
                                      ↓ 512D face vectors
Scene preview frame → cast-filtered matcher → Scene.person_ids
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
- 相同内容去重分两层：提交时远程任务按云盘文件身份/`content_hash` 同步去重；本地文件的 SHA-256 延迟到 worker 领取任务时计算，与运行中任务和已完成媒体比对后直接折叠为 `completed`（`media_id` 指向已有媒体），不会重复解析、抽帧和 embedding。对于启用影片准备的本地任务，SHA-256 读取和元数据/人脸库准备并行；任务阶段会先报告 `hashing_content` 或 `preparing_metadata`，而不是停留在初始排队文案。
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

**推荐：一键脚本**（默认同时启动 embedding + Go server；Go 服务自动 source `.env`、清除代理、`EMBEDDING_ENDPOINT` 指向 `EMBEDDING_PORT`）：

```bash
cd /home/zephyr/go/src/video-semantic-search
EMBEDDING_PORT=7002 EMBEDDING_LOG=logs/embedding-service-7002.log \
  bash scripts/start_embedding.sh
```

- embedding 已在运行时脚本会友好退出，因此单独重跑一次即可只重启 Go server
- `--no-server`（或 `SEARCH_SERVER=0`）只启 embedding，保持旧用法
- `.build/search-server` 缺失且 `/usr/local/go/bin/go` 存在时会自动构建（TMPDIR/GOCACHE 落在 `.build/`）

手工启动（等价，显式列出可调参数）：

```bash
cd /home/zephyr/go/src/video-semantic-search
mkdir -p .build && go build -o .build/search-server ./cmd/search-server

set -a
source .env
set +a
unset HTTP_PROXY HTTPS_PROXY ALL_PROXY http_proxy https_proxy all_proxy
EMBEDDING_ENDPOINT=http://127.0.0.1:7002 \
VIDEO_FAST_MODE=false \
VIDEO_MAX_SCENES=240 \
VIDEO_FAST_MAX_SCENES=32 \
VIDEO_REMOTE_FRAME_WORKERS=8 \
.build/search-server
```

注意：启动前会加载 `.env` 中的服务配置（例如 `TMDB_API_KEY`、`HF_TOKEN` 或 `ALIYUNPAN_WEB_REFRESH_TOKEN`），敏感配置不要提交仓库；除 `EMBEDDING_ENDPOINT` 外上面显式列出的 `VIDEO_*` 全部等于代码默认值，脚本方式省略它们行为完全一致。阿里云盘登录态在 `data/alipan_profiles.json`（`VIDEO_SEARCH_ALIPAN_CONFIG` 可改）。

Go 的远程文件请求使用显式直连 HTTP client，外部 `ffmpeg` / `ffprobe` 子进程也会清理代理环境。

默认地址和数据：

- Go API：`http://127.0.0.1:8000`
- Python embedding：`http://127.0.0.1:7001`
- Python Identity：`http://127.0.0.1:7003`（仅 `IDENTITY_ENABLED=true` 时参与入库/打标）
- 当前验证实例：`http://127.0.0.1:7001`
- 索引：`data/index.json`
- Identity 数据库：`data/identity.db`
- Identity 旧 JSON 迁移源：`data/identity.json`（只读保留）
- 任务：`data/acquisition_tasks.json`
- 画面：`data/frames/<media_id>/`
- 服务日志：`logs/search-server.log`
- embedding 日志：`logs/embedding-service.log`

### 4.3 Python Identity Service（可选）

安装到复用的 Hunyuan3D 环境，只补身份识别包：

```bash
cd /home/zephyr/go/src/video-semantic-search
PYTHON_BIN=/home/zephyr/go/src/hunyuan3d/.venv/bin/python \
  python -m pip install -r python/requirements-identity.txt
```

启动 InsightFace `buffalo_l`：

```bash
cd /home/zephyr/go/src/video-semantic-search
IDENTITY_PORT=7003 scripts/start_identity.sh
```

离线检查 HTTP 链路时可使用确定性 hash 后端，但它不理解人脸内容：

```bash
IDENTITY_BACKEND=hash IDENTITY_PORT=7003 scripts/start_identity.sh
```

Go 服务默认不自动调用身份服务。确认 Identity Service 可用后，重启 Go 服务并设置：

```bash
IDENTITY_ENABLED=true \
IDENTITY_ENDPOINT=http://127.0.0.1:7003 \
VIDEO_SEARCH_IDENTITY_DB=data/identity.db \
VIDEO_SEARCH_IDENTITY_FILE=data/identity.json \
.build/search-server
```

InsightFace 第一次启动可能下载 `buffalo_l` 模型；模型加载后常驻内存，每次请求不会重复加载。Go 侧设置 `IDENTITY_TIMEOUT`、`FACE_MATCH_THRESHOLD`、`FACE_MARGIN_THRESHOLD`、`FACE_MIN_DET_SCORE`、`IDENTITY_MIN_REFERENCES`（默认 5）和 `IDENTITY_REFERENCE_MAX_PER_PERSON`（默认 8）可调整识别策略。启用 Identity 后，影片准备接口会把“全部去重 cast 演员达到最少 reference 数”作为 `ready` 门槛；未启用时只检查 IMDb/TMDB，并明确返回 `face_bank_status=not_configured`。

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
| `VIDEO_SEARCH_USE_IMAGE_EMBEDDING` | 开启 | 设为 `false` 才会回退到旧的文本 caption 嵌入；图片入库 + 文本查询是设计意图 |
| `VIDEO_IMAGE_PROFILE` | `original` | 图片 embedding 档位（`original` / `compressed`） |
| `VIDEO_IMAGE_BATCH_SIZE` | 由引擎决定 | Go 发送图片 embedding 的批大小 |
| `VIDEO_SCENE_OVERLAP` | `1` | 准确模式相邻分镜窗口重叠（秒） |
| `VIDEO_SCENE_SAMPLE_FPS` | `2` | 准确模式分镜窗口采样帧率 |
| `VIDEO_SCENE_DETECTION_WIDTH` | `640` | 分镜检测缩放宽度 |
| `VIDEO_STREAM_CONCURRENCY` | `8` | 单媒体流媒体代理的上游并发闸（块缓存预取共享该额度） |
| `VIDEO_STREAM_CACHE_BYTES` | `2 GiB` | /stream 块磁盘缓存 LRU 预算；空值关闭缓存 |
| `VIDEO_STREAM_CACHE_DIR` | `data/stream-cache` | 块缓存目录；`off` 禁用 |
| `VIDEO_SEARCH_INDEX` | `data/index.json` | 索引文件路径 |
| `VIDEO_SEARCH_TASK_FILE` | `data/acquisition_tasks.json` | 采集任务持久化文件 |
| `VIDEO_SEARCH_FRAME_DIR` | `data/frames` | 抽帧根目录 |
| `VIDEO_SEARCH_ADDR` | `:8000` | HTTP 监听地址 |
| `VIDEO_SEARCH_ALIPAN_CONFIG` | `data/alipan_profiles.json` | 阿里云盘登录态持久化文件 |
| `ALIYUNPAN_TRANSCODE_TEMPLATES` | `264_720p,264_1080p,264_480p` | 云端转码清晰度优先级 |
| `IDENTITY_ENABLED` | `false` | 是否在采集后自动进行 Scene 人物识别 |
| `IDENTITY_ENDPOINT` | `http://127.0.0.1:7003` | Python Identity Service 地址 |
| `VIDEO_SEARCH_IDENTITY_DB` | `data/identity.db` | SQLite 人物、影片、cast、reference、face vector 和 Scene 标签存储 |
| `VIDEO_SEARCH_IDENTITY_FILE` | `data/identity.json` | 旧版 JSON 迁移源；数据库非空后不再读取 |
| `IDENTITY_TIMEOUT` | `2m` | Go 调用身份服务超时 |
| `FACE_MODEL` | `buffalo_l` | InsightFace 模型名称 |
| `IDENTITY_DEVICE` | `cuda` | `cuda` 或 `cpu` |
| `FACE_MATCH_THRESHOLD` | `0.45` | 人脸匹配最低余弦分数，需按数据校准 |
| `FACE_MARGIN_THRESHOLD` | `0.05` | 最优与次优演员分数差，过小则 unknown |
| `FACE_MIN_DET_SCORE` | `0.60` | 最低人脸检测分数 |
| `FACE_REFERENCE_MAX_PER_PERSON` | `10` | 每个演员参与匹配的 reference 上限 |
| `IDENTITY_SCENE_BATCH_SIZE` | `8` | Scene 人脸推理批次；过大时降低，需不超过 Identity Service 的 `IDENTITY_MAX_BATCH` |
| `IDENTITY_MAX_BATCH` | `16` | Python Identity Service 接受的单次场景路径上限 |
| `TMDB_API_KEY` | 未设置 | 配置后才能调用 TMDB 同步 |
| `TMDB_BASE_URL` | `https://api.themoviedb.org/3` | TMDB API 地址，可用于测试代理服务 |
| `TMDB_PROXY_URL` | 空 | Go worker、metadata prepare 和 catalog-sync 使用的显式 HTTP/HTTPS 代理；不影响媒体请求或 embedding |
| `TMDB_PROFILE_WORKERS` | `4`（最大 `16`） | 单部影片及同一客户端跨影片获取 TMDB 演员 profile 的全局并发上限；遇到 429/403 时降为 2 或 1 |
| `TMDB_PROFILE_IMAGE_SIZE` | `w500` | catalog-sync/惰性人脸向量下载的 TMDB 头像尺寸；只作为临时输入 |
| `IDENTITY_LAZY_LOAD` | `true` | 采集视频时是否按当前影片 cast 惰性获取 TMDB 元数据/头像并构建缺失向量 |
| `IDENTITY_MIN_REFERENCES` | `5` | 影片准备完成所要求的每位去重演员最少 ready face vector 数量 |
| `IDENTITY_REFERENCE_MAX_PER_PERSON` | `8` | 每位演员最多保留/构建的 reference 向量数量；选择时在 TMDB profile 顺序中分散 |
| `IDENTITY_KEEP_REFERENCE_IMAGES` | `false` | 是否保留 TMDB 临时头像文件；默认只保留向量和来源元数据 |
| `IDENTITY_REFERENCE_PROXY_URL` | 使用 `TMDB_PROXY_URL` | 惰性头像下载使用的显式 HTTP/HTTPS 代理 |
| `EMBEDDING_ENDPOINT` | `http://127.0.0.1:7001` | Python 服务地址（启动脚本自动按 `EMBEDDING_PORT` 推导） |
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
- `GET /v1/acquisitions/events`：SSE 任务事件流；首条为任务快照，后续为任务更新/删除事件。
- `GET /v1/acquisitions`、`GET /v1/acquisitions/{id}`：任务列表和详情。
- `DELETE /v1/acquisitions/{id}`：取消未完成任务（queued/running → `canceled`）；终态任务则移除该条记录。
- `POST /v1/acquisitions/batch/stop`：批量停止选中的 queued/running 任务，body 为 `{"task_ids":["..."]}`。
- `POST /v1/acquisitions/batch/delete`：批量删除选中的 completed/failed/canceled 任务记录，正在处理的任务不会被删除。
- `POST /v1/acquisitions/clear`：批量移除终态任务记录，body 可指定状态；页面“清理终态”会指定 `completed`、`failed`、`canceled`，返回 `{"removed":n}`。
- `GET /v1/media`、`GET /v1/media/{id}`：已处理文件和场景。
- `GET /v1/media/{id}/stream`：原始视频流播放（GET/HEAD）。本地文件直接流出；云盘文件由服务端代理并自动处理签名 URL 过期。浏览器可直接 seek。
- `POST /v1/media/{id}/embeddings/rebuild`：异步重建单个视频的向量，body 为 `{"profile":"original"}` 或 `{"profile":"compressed"}`；只读取已提取关键帧，不重新解析视频。
- `POST /v1/media/embeddings/rebuild`：批量异步重建选中视频的向量，body 为 `{"media_ids":["..."],"profile":"original"}` 或 `{"profile":"compressed"}`；返回多个任务和逐项失败信息。
- `POST /v1/media/batch/delete`：批量删除选中的媒体，body 为 `{"media_ids":["..."]}`；返回 `deleted` 和逐项 `failures`，同时清理关键帧目录。
- `GET /v1/media/{id}/frames/{filename}`：查看 JPG。
- `DELETE /v1/media/{id}/scenes/{scene_id}`：删除关键帧和向量。
- `DELETE /v1/media/{id}`：删除整部媒体及其向量。
- `POST /v1/media/{id}/persons/rebuild`：只重建该视频的 Scene 人物标签。
- `POST /v1/files/inspect`：检查单个服务端本地路径，并返回 `file` / `directory` 类型及可用性。
- `POST /v1/files/validate`：批量检查服务端本地文件是否可读。
- `POST /v1/files/scan`：扫描服务端本地目录中的视频文件。
- `POST /v1/metadata/movies/resolve`：根据标题、年份或视频文件名解析本地 Movie。
- `POST /v1/metadata/movies`、`GET /v1/metadata/movies?q=...&limit=...`：创建或有界查看 Movie 元数据；SQLite 中保存完整 IMDb 影片字段和别名；电影搜索支持精确、前缀和包含匹配，标题关键词不必从第一个词开始。
- `GET /v1/metadata/movies/{id}`：查看 Movie、cast 和去重后的 `people`；`POST /v1/metadata/movies/{id}/cast` 更新 cast。
- `POST /v1/metadata/movies/{id}/sync`：使用 `TMDB_API_KEY` 同步 Movie、Top 40 cast 和 profile image URL；这是轻量管理同步，不代表完整 TMDB cast 已准备。
- `POST /v1/metadata/movies/prepare`：按 `movie_id`、标题/年份或文件名检查 IMDb；必要时用英文 TMDB 查询补充已有 IMDb cast 的 profile，启用 Identity 时继续补齐人脸向量，并返回 `ready` 与缺失演员列表。采集 worker 复用同一准备服务。
- `POST /v1/metadata/persons`、`GET /v1/metadata/persons`、`GET /v1/metadata/persons/{id}`：创建、查看演员；带 `q`（或 `query`）和 `limit` 参数时，只从已经抽帧媒体关联的 IMDb actor/actress cast 中返回有 face vector 的去重演员（`limit` 最大 100）。
- `POST /v1/persons/{id}/faces`：从本地路径或 URL 下载 reference image、检测单人脸并写入 512D 向量；`GET` 查看状态，`POST .../sync` 重试指定图片，`POST .../rebuild` 重建已有图片。
- `POST /v1/search`：语义搜索，支持按媒体或片段返回；可用 `media_id` 限定到一个已索引视频。
- `GET /healthz`：服务和索引健康检查。

搜索排序使用 embedding 相似度降序，默认返回 Top 16；页面支持按视频归集或按片段平铺。当前搜索阈值默认大于 0.3，可由请求覆盖。设置 `media_id` 后先限定已索引媒体，再执行 WeMM 语义排序；设置 `person_id` 或 `person` 后，会先过滤 `Scene.person_ids`，不会把非该演员的片段混入结果。页面影片条件使用已抽帧媒体驱动的可搜索单选下拉框；演员条件使用本地数据驱动的可搜索多选下拉框，提交 `person_ids` 数组；多选按 OR 语义处理，即命中任一已选演员即可进入语义排序，单演员旧参数保持兼容。结果卡片的“简介/人员”按钮通过影片详情弹窗按需展示 IMDb 简介、演员与角色。

向量重建的图片 profile：`original` 使用 WeMM 模型默认图片处理，`compressed` 使用 `WEMM_COMPRESSED_MIN_IMAGE_PIXELS` / `WEMM_COMPRESSED_MAX_IMAGE_PIXELS` 限制像素预算。默认 profile 为 `original`，Go 服务可通过 `VIDEO_IMAGE_PROFILE` 配置新采集任务。管理弹窗中可对单个或勾选多个已处理视频发起重建，任务会复用右下角任务栏。用 `scripts/compare_wemm_profiles.py` 可从现有关键帧计算两种 profile 的逐图余弦相似度；脚本不修改索引。

## 8. 验证命令

```bash
cd /home/zephyr/go/src/video-semantic-search
go test ./...
go test -race ./internal/source ./internal/acquisition
go test -race ./internal/identity ./internal/api ./internal/store ./internal/search ./internal/acquisition
go vet ./...
python3 -m py_compile python/identity_service.py
bash -n scripts/start_identity.sh scripts/download_imdb_datasets.sh
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
curl -fsS http://127.0.0.1:7001/healthz | jq
curl -fsS http://127.0.0.1:7003/healthz | jq
pgrep -af 'video-semantic-search-server|embedding_service.py|identity_service.py'
pgrep -af 'ffmpeg|ffprobe'
```

检查图片是否重复：

```bash
md5sum data/frames/<media-id>/frame-*.jpg \
  | awk '{print $1}' | sort | uniq -c
```

## 9. 与 `face-recognition-mvp-plan.md` 的对照

结论：当前实现已经打通“全量 IMDb catalog/actor cast → 英文 TMDB enrichment → 去重 Person 的多 reference face vector → 单帧 Scene 标签 → 演员过滤 + WeMM 搜索”的可运行闭环；当前运行库已完成约 756,047 条 IMDb `movie` 行及其评分、别名、主创、角色和演员基础信息导入，TMDB enrichment 按 worker 领取影片任务后的 `/prepare` 惰性执行，尚未对全目录批量调用 TMDB。影片任务提交与准备已解耦，人员搜索已限制到已抽帧影片。2026-09-08 已修复 Identity 的 CUDA 动态库加载问题，并加入 Scene 人脸推理/标签批处理；仍属于单帧、非租户化的 MVP 验证版，尚未满足规划中的全部验收标准。

| 规划项 | 状态 | 当前实现与证据 |
|---|---|---|
| 身份与 WeMM 分离 | 已完成 | `python/identity_service.py` 独立提供检测/对齐/512D embedding；Go 的 `internal/identity` 负责业务匹配和持久化。`IDENTITY_BACKEND=hash` 仅用于联调。 |
| Phase 1：Movie/Person/Cast、TMDB sync、Movie Resolver | IMDb 已全量导入，TMDB 按需处理 | `cmd/catalog-sync` + IMDb TSV 流式导入已把 756,047 部 Movie、评分、别名、主创、角色和 actor/actress 关系写入 SQLite；IMDb ID 是 Person/cast 权威来源，TMDB 仅按 `en-US` 查询并通过 IMDb cast 绑定 profile/image，不创建 TMDB-only Person；IMDb/TMDB ID 唯一索引负责去重；TMDB profile 请求按 IMDb 演员名单过滤并以 4 路有界并发执行；文件名标题/年份解析和 worker 侧 `/prepare` 已实现。当前 `tmdb_status` 尚未批量填充，解析依赖本地已导入 IMDb Movie，未知影片不会仅凭文件名自动查询外部数据库。 |
| Phase 2：Reference Face Bank | 部分完成 | 生产存储已切到 SQLite，PersonImage 保存全部 TMDB profile 来源和稳定顺序，按同一演员去重后可选取最多 8 张分散 reference；启用 Identity 时影片只有在每位 cast 演员达到至少 5 张 ready vector 才算准备完成。规划目标至少 100 位演员、每人 5~20 张，当前真实数据规模仍未达标。 |
| Phase 3：单帧 Scene Identity | 已完成（有条件） | `TaggerService` 复用 Scene Preview Frame，按影片 cast 限定候选并写入 `Scene.person_ids` 和 `ScenePeople` 审计记录；当前索引 267 个场景中 83 个带标签。前提是影片已关联 cast 且候选人物已有向量，否则保留 unknown。 |
| Phase 4：演员过滤 + WeMM | 已完成（API/UI） | `person`/`person_id`/`person_ids` 会先过滤 Scene，再进行 WeMM 相似度排序；多选是 OR。规划中的自然语言 Query Analyzer（如“Leonardo DiCaprio 开车”自动拆出人物和语义词）尚未实现，调用方仍需单独传人物过滤字段。 |
| Phase 5：每 Scene 三帧聚合 | 未完成 | 当前只处理一个代表帧；没有 25%/50%/75% 三帧采样和跨帧投票/聚合。 |
| Benchmark 与阈值校准 | 部分完成 | 已有 CUDA/CPU provider 检查、真实 Identity 单图/批量耗时 smoke test 和批处理回归测试；仍没有规划中的标注 probe 集、Top-1/Top-3、FAR/FRR/Unknown、Actor Search Precision/Recall@10，阈值仍为启发式默认值。 |
| 规模、租户和正式向量存储 | 部分完成 | SQLite 已承担完整 IMDb/身份目录和事务导入，避免 JSON 全量重写；`tenant_id` 尚未进入模型/过滤链路，face vector 仍存 SQLite BLOB，后续再评估 PostgreSQL/Qdrant。 |

当前身份重建还有两个交接注意点：identity-only rebuild 会批量写回主索引的 `Scene.person_ids`，但不会把 `MovieID`/影片元数据回写到主索引媒体记录；另外当候选 cast 没有任何 face vector 时，tagger 会直接返回，不会主动清空旧的人物标签。两点在做数据迁移或重跑标签前都应先处理。

## 10. 尚未完成与优先级

1. 用最新二进制重新跑当前 Inception 文件（HEVC MP4），确认 403 后能继续取样、后段画面不重复，并把 `download_ratio` 与修复前的 50.6 MB 对比记录到本文档 §3.2。
2. 补一个真实 Matroska/WebM 云盘文件的验证：当前 MKV 路径只有合成 fixture 级证据（`TestRemoteProcessUsesContainerIndexBeforeFFprobe`、`TestRemoteMatroskaIndexDownloadsOnlyMetadata`），尚未在真实电影上跑过。
3. 为索引阶段增加实时 Range 字节数、当前阶段耗时和阶段超时，避免页面长时间停在“读取容器索引”而没有反馈；目前已经有 MP4 分块并发和索引缓存，但还没有逐阶段耗时字段。
4. 根据实际服务端限流测试 `VIDEO_REMOTE_FRAME_WORKERS=4/8/12`，记录总耗时、成功率、Range 流量和 GPU 利用率。
5. 对真实批量提交（几十个本地大文件 + 多个云盘文件）压测队列：确认 worker 数与 ffmpeg 并发、GPU 占用的平衡，并验证排队中取消、重复内容折叠在多 worker 下的行为。
6. 在线播放已切换云端转码 HLS（见 §0）；原文件 /stream 块缓存路径保留为回退。后续：web RefreshToken 过期后的重新登录引导（当前需手工更新 `ALIYUNPAN_WEB_REFRESH_TOKEN`）、转码分片磁盘缓存、更高清晰度（HD）切换按钮。
7. 补齐剩余容器与异常路径：fragmented MP4（`mvex`）、无 `Cues` 或 `Cues` 不带 `CueRelativePosition` 的 Matroska、laced block、多视频轨选择、MPEG-TS/AVI 容器；目前这些情况会回退到 FFmpeg 通用读取或报告局部不可解码。
8. 将当前 `FileStore` 替换为 PostgreSQL + Qdrant，并增加视频级粗召回和场景级精排。
9. 身份组件下一步：补齐至少 100 位演员的真实 reference 数据，为阈值建立 benchmark，并支持每个 Scene 的 3 帧聚合；SQLite 已完成第一阶段，TMDB profile 已有有界并发、请求合并和 IMDb cast 过滤，后续再增加阶段耗时/限流指标、租户隔离和可替换的向量后端。当前默认视频处理链路已经改为 IMDb 本地 cast → 按需 TMDB → 临时头像 → face vector；`catalog-sync --tmdb` 仍保留给离线批量 enrichment，不建议对全球目录无上限执行。
10. 用 CUDA provider 对一部完整影片重跑人物标签，记录从抽帧、Identity 批量推理到 WeMM 的分阶段耗时，并确认批量大小 8 在当前 GPU 显存和识别准确率之间的平衡。

不要把 `.env`、`data/alipan_profiles.json`、下载 URL、access token 或 refresh token 提交到仓库。
