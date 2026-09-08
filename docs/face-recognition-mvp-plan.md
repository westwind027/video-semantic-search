# 人脸识别增强视频语义搜索 MVP 规划

## 1. MVP 目标

在现有视频语义搜索系统上增加“演员身份识别”闭环：

```text
电影元数据
→ 获取 cast
→ 获取演员 reference images
→ 生成人脸 embedding
→ 视频入库时识别人脸
→ Scene.person_ids
→ person filter + WeMM semantic search
```

最终支持：

```text
Leonardo DiCaprio 开车
Tom Hardy 雪地
某演员 战斗
```

解析为：

```text
person_id = xxx
AND
WeMM(scene) ≈ semantic_query
```

---

## 2. 当前系统基础

现有系统已经具备：

```text
视频采集
→ 关键帧 / Scene 抽取
→ WeMM Image Embedding
→ Scene Vector Search
→ 搜索结果播放定位
```

当前架构：

```text
Go
├── API
├── Acquisition
├── Video Processing
├── Search
└── Storage

Python
└── WeMM Embedding Service
```

本 MVP 只新增一条身份识别链路，不重构现有主流程。

---

## 3. 核心原则

### 3.1 人物身份与场景语义分离

WeMM 负责：

```text
场景 / 动作 / 物体 / 环境 / 视觉语义
```

人脸模型负责：

```text
演员身份
```

Scene 最终结构：

```json
{
  "scene_id": "...",
  "media_id": "...",
  "person_ids": ["person_xxx"],
  "visual_embedding": [...]
}
```

### 3.2 先缩小 cast，再做人脸搜索

不做：

```text
Face → 全球演员 ANN
```

而做：

```text
media_id
→ movie cast
→ candidate person_ids
→ 只在候选演员 reference embeddings 中比对
```

这是 MVP 准确率的关键。

### 3.3 每个演员保留多张 reference

推荐：

```text
5~20 张 / actor
```

用于覆盖：

```text
年龄
发型
胡须
角度
光照
妆容
```

---

## 4. MVP 非目标

第一版不把“无边界地一次性同步全球 IMDb 与 TMDB/头像”作为启动动作；IMDb bulk 的完整电影目录可以通过流式导入器写入 SQLite，避免把全量数据装入内存或反复重写 JSON。TMDB 完整 cast/profile 与 face bank 仍按影片任务由 worker 领取后的准备阶段或有界离线批次执行，头像只作为临时推理输入，成功后删除，演员向量按 IMDb/TMDB 人物 ID 全局复用。

第一版暂不做：

```text
无限制的全球演员全量 ANN 服务
IMDb HTML 全站抓取
动画人物识别
复杂 Face Track
多模型 Ensemble
人脸聚类
VLM 人物识别
跨影片 unknown clustering
完整 RBAC
```

---

## 5. 数据源策略

建议：

```text
IMDb ID + TMDB Metadata
```

IMDb ID 用作稳定外部标识。

影片元数据使用 IMDb 官方 bulk datasets：`title.basics`、`title.ratings`，可选 `title.akas`、`title.crew`；演员基础信息和影片演员关系分别来自 `name.basics`、`title.principals`。这些 TSV 支持 `.tsv.gz`，SQLite 导入器按行流式读取、以 IMDb ID 建唯一索引，并可用 IMDb ID、年份和数量限制缩小 TMDB enrichment 批次。

TMDB 用于：

```text
Movie metadata
Cast
Person
Profile images
```

默认视频任务入队后由 worker 调用与 `POST /v1/metadata/movies/prepare` 相同的准备服务检查本地 IMDb Movie；若 TMDB 完整 cast/profile 尚未准备，则按 IMDb ID 同步，并将同一 TMDB/IMDb 演员合并为一个 Person。每位演员默认最多选 8 张在 TMDB profile 顺序上分散的 `w500` 头像，随后由 Identity Service 检测“恰好一张脸”的图片并建立 512 维向量；头像下载失败、多人脸和低检测分数都会单独记录，不会污染可用 reference。任务提交不等待该阶段，失败原因写入任务状态；离线 `catalog-sync --tmdb` 仍可用于有界批次，不适合无上限全量运行。

MVP 不依赖 IMDb HTML scraping 获取演员头像。

### 5.1 可恢复采集命令

```bash
cd video-semantic-search
scripts/download_imdb_datasets.sh

# 本地库批次：每行一个 tconst，例如 tt0111161
set -a; source .env; set +a
printf 'tt0111161\n' > data/imdb/title-ids.txt

go build -o .build/catalog-sync ./cmd/catalog-sync
.build/catalog-sync \
  --imdb-basics data/imdb/title.basics.tsv.gz \
  --imdb-ratings data/imdb/title.ratings.tsv.gz \
  --imdb-principals data/imdb/title.principals.tsv.gz \
  --imdb-names data/imdb/name.basics.tsv.gz \
  --imdb-akas data/imdb/title.akas.tsv.gz \
  --imdb-crew data/imdb/title.crew.tsv.gz \
  --title-id-file data/imdb/title-ids.txt \
  --tmdb \
  --identity-db data/identity.db \
  --identity-file data/identity.json \
  --min-references 5 \
  --max-images-per-person 8 \
  --reference-root data/identity-faces
```

不传 `--title-id-file` 时按年份和 `--max-movies` 选择 IMDb 电影；不传 `--tmdb` 时可对完整 IMDb 电影、评分、别名、主创、角色和演员关系做 SQLite-only 导入。传入 `--tmdb` 后要求有界选择、`TMDB_API_KEY` 和已启动的 Identity Service；同一 TMDB/IMDb 演员跨影片复用 Person 与 vector，profile 请求默认 4 路有界并发且先按 IMDb actor/actress 名单过滤，头像默认使用 `w500`。IMDb 已完成导入后可加 `--skip-imdb-import`，重启同一命令会复用已保存图片和 face vector，失败项可以单独重试。

---

## 6. 数据模型

### movie

```text
id
imdb_id
tmdb_id
title
original_title
year
start_year
end_year
title_type
is_adult
runtime_minutes
genres
alternate_titles
directors
writers
average_rating
vote_count
overview
poster_url
metadata_json
```

### person

```text
id
imdb_id
tmdb_id
name
normalized_name
aliases
metadata_json
```

### movie_cast

```text
movie_id
person_id
character_name
billing_order
```

### person_image

```text
id
person_id
source
source_url
local_path
width
height
vote_average
quality_score
face_count
status
```

### scene_person

```text
scene_id
person_id
confidence
match_count
best_score
```

---

## 7. 向量库设计

### scene_vectors

Vector：

```text
WeMM
```

Payload：

```json
{
  "media_id": "...",
  "scene_id": "...",
  "tenant_id": "default",
  "person_ids": ["person_001"]
}
```

### person_face_vectors

Vector：

```text
512D face embedding
```

Payload：

```json
{
  "person_id": "person_001",
  "image_id": "image_001",
  "quality": 0.93,
  "model": "insightface_buffalo_l"
}
```

---

## 8. MVP 人脸模型

第一版推荐：

```text
InsightFace buffalo_l
```

原因：

```text
Face Detection
+
Face Alignment
+
512D Face Embedding
```

一套可以直接打通。

---

## 9. Python Identity Service

新增：

```text
Python Identity Service
```

与 WeMM 服务并列。

职责：

```text
Face Detection
Alignment
Embedding
```

Go 继续负责业务逻辑。

---

## 10. Identity API

### POST /v1/faces/embed

输入：

```text
image
```

输出：

```json
{
  "faces": [
    {
      "bbox": [10, 20, 100, 120],
      "det_score": 0.98,
      "embedding": [],
      "quality": 0.91
    }
  ]
}
```

---

## 11. Movie Resolver

视频处理开始前增加：

```text
Movie Resolver
```

例如：

```text
Inception.2010.1080p.BluRay.mkv
```

解析：

```text
title = Inception
year = 2010
```

再查询 Movie DB：

```text
title + year
→ movie_id
```

---

## 12. Metadata Ingestion Pipeline

```text
TMDB Movie
→ Movie
→ Credits
→ Cast
→ Person
→ Person Images
```

MVP 每部电影只需要：

```text
Top 20~40 cast
```

---

## 13. Reference Face Pipeline

```text
Person
→ 下载 5~20 张 profile images
→ Face Detection
→ 无效图片过滤
→ Face Alignment
→ 512D Embedding
→ person_face_vectors
```

丢弃：

```text
无人脸
多人脸
脸太小
严重模糊
极端侧脸
低检测分
```

---

## 14. 视频入库 Pipeline

现有：

```text
Scene
→ Representative Frame
→ WeMM
```

扩展为：

```text
Scene
├── Representative Frame → WeMM
└── Identity Frame(s)
    → Face Detection
    → Face Embedding
    → Cast-filtered Matching
    → Scene.person_ids
```

第一版可以直接复用现有 Scene Preview Frame。

后续再改为每个 Scene：

```text
25%
50%
75%
```

三帧识别。

---

## 15. Actor Matching

流程：

```text
query_face_embedding
→ 获取 movie cast person_ids
→ Qdrant search
→ filter person_id IN cast
→ TopK reference faces
→ 按 person_id 聚合
```

不要只取 Top1 reference。

推荐：

```text
person_score = mean(top 3 reference scores)
```

---

## 16. 阈值策略

配置项：

```text
FACE_MATCH_THRESHOLD
FACE_MARGIN_THRESHOLD
FACE_MIN_DET_SCORE
```

判定：

```text
if best_score < threshold:
    unknown

if best_score - second_score < margin:
    unknown

else:
    matched
```

原则：

```text
宁可 unknown
也不要错误演员标签
```

---

## 17. 搜索流程

用户：

```text
Leonardo DiCaprio 开车
```

Query Analyzer：

```json
{
  "person_id": "person_leo",
  "semantic_query": "开车"
}
```

执行：

```text
person_id = Leo
AND
WeMM ≈ "开车"
```

---

## 18. Go 服务职责

Go：

```text
Movie Resolver
Metadata Orchestration
Cast Lookup
Scene Processing
调用 Identity Service
Actor Matching Orchestration
Scene.person_ids 持久化
Search Filter
API
```

Python：

```text
Face Detection
Alignment
Embedding
```

---

## 19. API 规划

### Metadata

```text
POST /v1/metadata/movies/resolve
POST /v1/metadata/movies/{id}/sync
POST /v1/metadata/movies/prepare
GET  /v1/metadata/movies/{id}
GET  /v1/metadata/persons?q=<name>&limit=<n>
GET  /v1/metadata/persons/{id}
```

### Face Bank

```text
POST /v1/persons/{id}/faces/sync
POST /v1/persons/{id}/faces/rebuild
GET  /v1/persons/{id}/faces
```

### Scene Identity

```text
POST /v1/media/{id}/persons/rebuild
```

只重建人物标签，不重算 WeMM。

---

## 20. 实施阶段

### Phase 1：Metadata DB

已完成（当前实现）：

```text
Movie
Person
MovieCast
完整 IMDb catalog → SQLite
TMDB 完整 cast/profile sync
IMDb/TMDB Person 去重
Movie Resolver
worker 处理前 metadata/face-bank prepare gate（提交接口不等待）
```

目标：

```text
输入 title/year
→ movie_id
→ cast
```

### Phase 2：Actor Reference Face Bank

当前实现：

```text
Person Images
→ InsightFace
→ 512D
→ SQLite BLOB（可替换向量后端）

每个 Person 最多保留 8 个分散 profile reference；影片只有在其去重 cast 的每个人达到至少 5 个 ready vector 后才标记为 face-bank complete。
```

目标：

```text
至少 100 个演员
每人 5~20 reference
```

### Phase 3：单帧演员识别

使用现有 Scene Preview Frame。

目标：

```text
Scene.person_ids
```

闭环跑通。

### Phase 4：搜索过滤

实现：

```text
Actor Name
→ person_id
→ person filter
+
WeMM semantic search
```

### Phase 5：多帧 Scene Identity

每个 Scene：

```text
25%
50%
75%
```

识别后聚合。

---

## 21. MVP 数据规模

建议先做：

```text
100 Movies
500~1000 Actors
```

每演员：

```text
5~10 reference faces
```

总量大约：

```text
2500~10000 face vectors
```

足够验证。

---

## 22. Benchmark

建议人工建立：

```text
50 Movies
100 Actors
```

每演员：

```text
5 reference images
20 movie probe faces
```

约：

```text
2000 probe faces
```

指标：

```text
Top-1 Accuracy
Top-3 Accuracy
False Accept Rate
False Reject Rate
Unknown Rate
```

更重要的业务指标：

```text
Actor Search Precision@10
Actor Search Recall@10
```

---

## 23. 验收标准

MVP 必须满足：

### Metadata

```text
电影文件
→ 正确识别 movie
→ 得到 cast
```

### Face Bank

```text
主要演员
≥ 5 usable references
```

### Scene Tagging

```text
已知演员 Scene
→ Scene.person_ids
```

### Search

```text
演员名 + 场景语义
```

结果不能出现不包含该演员的 Scene。

---

## 24. 失败处理

以下情况允许：

```text
unknown
```

包括：

```text
脸太小
模糊
严重侧脸
遮挡
reference 不足
分数接近
```

系统不能为了“有标签”而强行识别。

---

## 25. 模型升级路线

第一版：

```text
InsightFace buffalo_l
```

第二步 benchmark：

```text
AdaFace R100
```

重点比较：

```text
低清
暗光
运动模糊
跨年龄
```

必须使用同一批 reference、probe 和 threshold calibration。

如果 AdaFace 在真实电影帧明显更好，再替换。

---

## 26. 后续增强

MVP 后按优先级：

```text
P1 Face Track
P2 Unknown Person Clustering
P3 更多 reference
P4 AdaFace / 其他模型 benchmark
P5 PostgreSQL + Qdrant 正式迁移
P6 多租户
```

---

## 27. 多租户预留

从第一版开始，face vector payload 建议保留：

```text
tenant_id
```

MVP：

```text
tenant_id = default
```

未来：

```text
tenant_id = A
AND
person_id IN [...]
```

即可。

---

## 28. License 注意事项

上线前需要单独确认：

```text
IMDb data license
TMDB API terms
actor image usage rights
InsightFace pretrained model license
```

MVP / Research 与商业化要分开评估。

不要把长期 scraping IMDb HTML 设计为正式数据供应链。

---

## 29. 推荐目录结构

```text
internal/
  metadata/
    movie.go
    person.go
    resolver.go
    tmdb.go

  identity/
    client.go
    matcher.go
    aggregator.go

  search/
    planner.go
    filters.go

python/
  identity_service/
    app.py
    insightface_backend.py
    schemas.py
```

---

## 30. 推荐配置

```text
IDENTITY_ENDPOINT=http://127.0.0.1:7003

FACE_MODEL=buffalo_l
FACE_MATCH_THRESHOLD=<benchmark calibration>
FACE_MARGIN_THRESHOLD=<benchmark calibration>
FACE_MIN_DET_SCORE=<benchmark calibration>

IDENTITY_MIN_REFERENCES=5
IDENTITY_REFERENCE_MAX_PER_PERSON=8
FACE_SCENE_FRAMES=1
```

后续：

```text
FACE_SCENE_FRAMES=3
```

---

## 31. 最终 MVP 架构

```text
                  Movie File
                      │
                      ▼
                Movie Resolver
                      │
                      ▼
                  Movie DB
                      │
                  get cast
                      │
                      ▼
                 Scene Frames
                      │
          ┌───────────┴───────────┐
          ▼                       ▼
        WeMM                 InsightFace
          │                       │
          ▼                       ▼
  Visual Embedding          Face Embedding
                                  │
                                  ▼
                         Cast-filtered Search
                                  │
                                  ▼
                              person_id
                                  │
                  ┌───────────────┘
                  ▼
                Scene
       ┌──────────┴──────────┐
       ▼                     ▼
visual_embedding          person_ids
       │                     │
       └──────────┬──────────┘
                  ▼
             Search Engine
```

---

## 32. MVP 成功标准

只要做到：

```text
100 部电影
→ metadata/cast 自动入库
→ 演员 reference face bank
→ Scene 自动打演员标签
→ 演员名 + 语义搜索
```

并且在人工 benchmark 中满足：

```text
演员过滤基本不串人
低质量脸宁可 unknown
搜索结果明显优于纯 WeMM
```

就说明这条路线成立。

之后再投入：

```text
AdaFace
Face Track
更多 reference
Unknown Clustering
PostgreSQL + Qdrant 扩容
多租户
```

是更合理的顺序。
