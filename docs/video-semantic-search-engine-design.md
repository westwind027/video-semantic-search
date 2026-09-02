# 多模态视频语义搜索引擎设计文档

## 1. 项目概述

### 1.1 项目目标

构建一个面向大规模视频内容的多模态语义搜索引擎，支持从多个公开数据源采集视频元数据、预览图、Storyboards、字幕以及 BitTorrent 元数据，并利用 WeMM-Embedding 建立统一的视觉/视频/文本语义索引。

最终用户可以通过自然语言搜索视频内容，而不再局限于标题、演员、标签等传统关键词。

典型查询示例：

- `夕阳下两个人骑摩托车`
- `一个男人站在雨中，后面有一辆红色跑车`
- `韩国电影，雪地里有人追逐`
- `有人从汽车里跳出来然后逃跑`
- `小时候看过一部电影，小孩躲在床底下，房间是蓝色的`
- `某个角色说“我们必须回去”`

系统目标不是存储完整视频，而是构建：

> **Media → Asset → Scene/Shot → Semantic Index**

统一索引层。

---

## 2. 核心设计原则

### 2.1 搜索引擎与资源存储分离

系统只需要保存：

- 视频元数据
- 来源信息
- Poster
- Preview Thumbnail
- Storyboard
- 字幕
- 视频指纹
- Scene/Shot 信息
- Embedding
- 可公开访问的 URL 或来源标识

无需保存完整视频。

```text
Video Search Engine
        ≠
Full Video Hosting
```

这样可以显著降低：

- 存储成本
- 带宽成本
- 视频解码成本
- 法律与版权风险

---

### 2.2 Scene/Shot 是核心搜索粒度

不建议：

```text
1 Video = 1 Vector
```

推荐：

```text
Video
 ├── Scene 001
 ├── Scene 002
 ├── Scene 003
 └── ...
```

搜索应该首先返回 Scene，再聚合到 Media。

例如：

```text
Interstellar
 ├── Scene 00:13:20
 ├── Scene 00:46:12
 └── Scene 01:32:04
```

用户最终不仅可以找到某部电影，还可以直接定位到具体时间点。

---

### 2.3 多路索引，而不是单一向量索引

系统应同时维护：

1. Metadata / BM25
2. Visual Embedding
3. Video Embedding
4. Subtitle/Text Embedding
5. Fingerprint / Dedup Index
6. Media Entity Index

整体搜索：

```text
                    Query
                      │
       ┌──────────────┼──────────────┐
       │              │              │
       ▼              ▼              ▼
     BM25          Visual         Subtitle
       │           Vector          Vector
       │              │              │
       └──────────────┼──────────────┘
                      ▼
                   Fusion
                      │
                      ▼
                   Rerank
                      │
                      ▼
                Scene Results
                      │
                      ▼
                Media Aggregation
```

---

# 3. 数据源设计

## 3.1 视频网站

可采集信息包括：

```text
title
description
tags
duration
uploader
publish_time
poster
thumbnail
preview thumbnails
storyboard
subtitle
chapter
source_url
```

很多视频网站为了播放器拖动预览已经提前生成：

```text
sprite.jpg
preview_001.jpg
preview_002.jpg
...
```

或者：

```text
WebVTT
00:00 --> 00:10
sprite.jpg#xywh=0,0,160,90
```

这部分是非常有价值的廉价视觉索引数据。

优势：

```text
无需下载原始视频
无需完整 decode
无需自行生成关键帧
```

可以直接：

```text
Storyboard
   ↓
Frame Extraction
   ↓
WeMM Image Embedding
   ↓
Scene Index
```

---

## 3.2 BitTorrent

推荐只把 BT 视为一个：

> Metadata / Asset Discovery Source

主要采集：

```text
infohash
torrent_name
file_list
file_size
file_path
tracker/source
publish_time
metadata
```

典型文件：

```text
Interstellar.2014.1080p.BluRay.x264.mkv
Interstellar.2014.REMUX.2160p.mkv
星际穿越.2014.蓝光.1080P.mkv
```

它们应归并为：

```text
Media
  Interstellar (2014)

Assets
 ├── Torrent A
 ├── Torrent B
 ├── Website A
 └── Other Source
```

BT 的 `infohash` 是非常好的 Asset Identifier。

---

## 3.3 网盘 / 公开分享目录

如果采集公开可索引资源，可获得：

```text
filename
filesize
directory
share metadata
preview
thumbnail
duration
resolution
source
```

不能直接使用 URL 作为内容 ID。

因为同一视频可能存在数千个不同 URL。

建议构建：

```text
content_fingerprint
```

用于去重和归一。

---

## 3.4 字幕来源

字幕价值非常高。

来源包括：

```text
website subtitle
external subtitle
embedded subtitle
ASR
```

字幕索引可以解决：

```text
“有人说某句话”
“某个角色讨论量子物理”
“电影里有人提到东京”
```

这类单纯视觉搜索无法完成的问题。

---

# 4. WeMM-Embedding 使用方案

## 4.1 Embedding 类型

统一设计：

```text
Media
 └── Scene
      ├── image_embedding
      ├── video_embedding
      ├── subtitle_embedding
      └── metadata_embedding
```

---

## 4.2 图片 Embedding

主要来源：

```text
poster
thumbnail
storyboard
preview frame
keyframe
```

作用：

```text
Text → Image Retrieval
Image → Image Retrieval
```

适合：

```text
“红色跑车”
“雪地”
“蓝色房间”
“夕阳”
“海边白色房子”
```

---

## 4.3 Video Embedding

图片无法很好表达动作关系。

例如：

```text
一个人从车里跳出来
两个人拥抱
飞机从楼顶掠过
一个人追赶另一个人
```

这种查询应使用短视频片段 Embedding。

推荐将视频分为：

```text
5~30 秒短 Clip
```

而不是整部电影。

---

## 4.4 Text Embedding

用于：

```text
title
description
subtitle
chapter
tags
generated caption
```

文本 Embedding 与 BM25 应共存。

不能用向量搜索完全替代关键词搜索。

---

# 5. 视频分段策略

## 5.1 MVP 阶段

如果只有 Storyboard：

```text
Storyboard Frame
    ↓
timestamp
    ↓
pseudo scene
```

例如：

```text
00:00
00:10
00:20
00:30
```

每张图建立一个 Scene。

---

## 5.2 Shot Detection 阶段

有原始视频时：

```text
Video
 ↓
Shot Boundary Detection
 ↓
Shot
 ↓
Representative Frames
```

可以使用：

```text
PySceneDetect
TransNetV2
FFmpeg
```

---

## 5.3 Scene 聚合

多个连续 Shot 可以进一步聚合成 Scene。

```text
Shot
Shot
Shot
 │
 ▼
Scene
```

Scene 可以依据：

- 时间连续性
- 视觉相似度
- 字幕语义相似度
- 人物一致性
- 场景一致性

---

# 6. 数据模型

## 6.1 Media

```json
{
  "media_id": "uuid",
  "type": "movie",
  "title": "Interstellar",
  "original_title": "Interstellar",
  "year": 2014,
  "duration": 10140,
  "language": ["en"],
  "imdb_id": "...",
  "tmdb_id": "...",
  "metadata": {}
}
```

---

## 6.2 Asset

Asset 表示一个具体资源。

```json
{
  "asset_id": "uuid",
  "media_id": "uuid",
  "source_type": "torrent",
  "source": "source-name",
  "url": "...",
  "infohash": "...",
  "filename": "...",
  "filesize": 123456789,
  "duration": 10140,
  "resolution": "1920x1080"
}
```

---

## 6.3 Scene

```json
{
  "scene_id": "uuid",
  "media_id": "uuid",
  "asset_id": "uuid",
  "start": 800.0,
  "end": 823.5,
  "preview": "...",
  "subtitle": "...",
  "caption": "a man standing in a corn field"
}
```

---

## 6.4 Vector

推荐逻辑上单独维护：

```text
scene_id
embedding_type
model
dimension
vector
```

Embedding 类型：

```text
image
video
subtitle
caption
metadata
```

---

# 7. Entity Resolution

这是整个系统大规模以后最重要的问题之一。

目标：

```text
多个 Asset
    ↓
同一个 Media
```

例如：

```text
Interstellar.2014.1080p...
Interstellar.2014.2160p...
星际穿越 2014 ...
```

都归并为：

```text
Media: Interstellar (2014)
```

---

## 7.1 Metadata Matching

匹配字段：

```text
normalized_title
year
duration
episode
season
filename
resolution
imdb_id
tmdb_id
```

---

## 7.2 Fingerprint Matching

进一步使用：

```text
video duration
frame perceptual hash
keyframe hash
audio fingerprint
scene hash
```

形成：

```text
content_signature
```

---

# 8. 去重体系

建议分三级。

## L1 Asset 去重

```text
infohash
source_id
exact filesize
```

---

## L2 Content 去重

```text
duration
resolution
keyframe pHash
audio fingerprint
```

---

## L3 Semantic 去重

```text
scene embedding similarity
```

用于识别：

- 转码版本
- 裁剪版本
- 带字幕版本
- 水印版本
- 分辨率不同版本

---

# 9. 搜索系统设计

## 9.1 Query Processing

用户输入：

```text
“雪地里两个人追逐的韩国电影”
```

Query Parser 提取：

```text
visual:
    snow
    two people
    chasing

metadata:
    country = Korea

type:
    movie
```

---

## 9.2 Retrieval

并行执行：

```text
BM25
Visual ANN
Subtitle ANN
Metadata ANN
```

---

## 9.3 Fusion

推荐初始版本使用：

```text
RRF
Reciprocal Rank Fusion
```

例如：

```text
score =
  0.30 * visual
+ 0.20 * subtitle
+ 0.20 * metadata
+ 0.15 * bm25
+ 0.15 * video
```

后期可训练 Learning-to-Rank。

---

# 10. 两级向量索引

为了控制成本，不建议直接：

```text
Query
 ↓
1 Billion Scene Vectors
```

推荐：

```text
Query
  ↓
Media Level ANN
  ↓
Top 10K Media
  ↓
Scene Level ANN
  ↓
Top 1K Scene
  ↓
Rerank
```

---

## 10.1 Hot Index

保存：

```text
media pooled vector
```

规模：

```text
10M ~ 100M vectors
```

---

## 10.2 Cold Scene Index

保存：

```text
scene vectors
```

规模可能达到：

```text
100M
1B
10B
```

通过 Media 候选过滤后再访问。

---

# 11. 向量维度与存储估算

假设：

```text
10M videos
20 scene vectors/video
```

总计：

```text
200M vectors
```

---

## 11.1 4096D FP16

单向量：

```text
4096 × 2 bytes
= 8192 bytes
= 8 KB
```

200M：

```text
约 1.64 TB
```

还未计算 ANN Index。

---

## 11.2 1024D FP16

```text
1024 × 2
= 2 KB/vector
```

200M：

```text
约 400 GB
```

---

## 11.3 256D FP16

```text
256 × 2
= 512 bytes/vector
```

200M：

```text
约 102 GB
```

---

## 11.4 推荐方案

```text
L1：
256D

L2：
1024D

Rerank：
4096D / 原始多模态模型
```

这样可以显著降低大规模 ANN 成本。

---

# 12. 向量压缩

规模进一步扩大时：

```text
FP16
 ↓
INT8 / SQ8
 ↓
PQ
 ↓
IVF-PQ
```

可以将存储进一步下降数倍到数十倍。

建议：

```text
Hot Vector:
FP16 / INT8

Cold Scene:
PQ

Top Candidates:
Full Precision Rerank
```

---

# 13. 技术选型

## 13.1 PostgreSQL

负责：

```text
Media
Asset
Source
Entity
Job
Crawler State
```

推荐：

```text
PostgreSQL 16+
```

---

## 13.2 OpenSearch

负责：

```text
BM25
metadata
filter
keyword
facet
```

例如：

```text
country
year
language
duration
source
genre
```

---

## 13.3 Milvus

适合：

```text
大规模 Scene Vector
```

优点：

- 分布式
- ANN
- Billion-scale
- Vector-native

---

## 13.4 Vespa

如果最终系统规模较大，Vespa 非常值得考虑。

它可以同时负责：

```text
BM25
vector retrieval
filter
ranking
tensor
rerank
```

所以后期可能演化为：

```text
PostgreSQL
+
Vespa
+
Object Storage
```

替代：

```text
PostgreSQL
+
OpenSearch
+
Milvus
```

---

## 13.5 MinIO / S3

保存：

```text
poster
thumbnail
storyboard
frame
small clip
subtitle
```

不建议长期保存完整视频，除非业务确有授权与必要性。

---

# 14. 推荐整体架构

```text
                    Sources
                       │
      ┌────────────────┼────────────────┐
      │                │                │
 Video Sites        Torrents       Public Catalogs
      │                │                │
      └────────────────┼────────────────┘
                       ▼
                   Crawler
                       │
                       ▼
                Raw Metadata
                       │
                       ▼
                 Normalizer
                       │
                       ▼
              Entity Resolution
                       │
             ┌─────────┴─────────┐
             ▼                   ▼
           Media                Asset
             │                   │
             └─────────┬─────────┘
                       ▼
                Scene Extractor
                       │
        ┌──────────────┼──────────────┐
        │              │              │
       Frame        Subtitle         Clip
        │              │              │
        ▼              ▼              ▼
      WeMM           Text           WeMM
      Image         Embedding       Video
        │              │              │
        └──────────────┼──────────────┘
                       ▼
                    Index
             ┌─────────┼─────────┐
             ▼         ▼         ▼
          BM25       Vector    Fingerprint
             │         │         │
             └─────────┼─────────┘
                       ▼
                  Search API
                       │
                       ▼
                  Reranking
                       │
                       ▼
                    Results
```

---

# 15. Offline Pipeline

推荐使用 Job Queue。

```text
Crawler
 ↓
Kafka / Redis Streams
 ↓
Metadata Worker
 ↓
Media Resolver
 ↓
Preview Extractor
 ↓
Embedding Worker
 ↓
Indexer
```

任务示例：

```text
crawl_source
normalize_asset
resolve_media
extract_storyboard
extract_keyframe
generate_image_embedding
generate_video_embedding
index_scene
```

---

# 16. 在线搜索 Pipeline

```text
HTTP Query
  ↓
Query Analyzer
  ↓
Embedding Service
  ↓
Parallel Retrieval
  ├── BM25
  ├── Visual ANN
  ├── Subtitle ANN
  └── Media ANN
  ↓
Fusion
  ↓
Top N
  ↓
Rerank
  ↓
Media Aggregation
  ↓
Response
```

目标：

```text
P50 < 300 ms
P95 < 800 ms
```

不包含重型 VLM Rerank 时。

---

# 17. API 设计

## Search

```http
POST /v1/search
```

Request：

```json
{
  "query": "雪地里两个人追逐的韩国电影",
  "type": "movie",
  "limit": 20
}
```

Response：

```json
{
  "results": [
    {
      "media_id": "...",
      "title": "...",
      "scene": {
        "start": 1234,
        "end": 1250,
        "preview": "..."
      },
      "score": 0.93
    }
  ]
}
```

---

## Similar Image

```http
POST /v1/search/image
```

输入图片：

```text
Image
 ↓
WeMM
 ↓
ANN
```

---

## Similar Video

```http
POST /v1/search/video
```

输入短视频：

```text
Clip
 ↓
WeMM Video
 ↓
Scene ANN
```

---

# 18. MVP 实施方案

## Phase 1

目标：

> 验证 Thumbnail / Storyboard + WeMM 是否可以完成实用的视频语义搜索。

规模：

```text
100K 视频
1~2M Preview Images
```

组件：

```text
Python Crawler
PostgreSQL
Qdrant / Milvus
WeMM
FastAPI
简单 Web UI
```

数据：

```text
title
description
preview
timestamp
url
```

暂时不处理：

```text
BT
复杂去重
video embedding
ASR
```

---

# 19. Phase 2

增加：

```text
字幕
BM25
Hybrid Search
Scene Grouping
Media Entity
```

规模：

```text
1M 视频
20M Scenes
```

---

# 20. Phase 3

增加：

```text
BitTorrent Metadata
网盘公开目录
多源 Asset
Entity Resolution
Fingerprint
```

规模：

```text
10M Videos
200M Scenes
```

---

# 21. Phase 4

增加：

```text
Video Embedding
Clip Retrieval
Reranker
VLM
Learning-to-Rank
```

搜索质量进入真正的多模态阶段。

---

# 22. Phase 5

目标：

```text
100M Media
1B+ Scenes
```

架构考虑：

```text
Kafka
Kubernetes
Vespa / Distributed Milvus
S3
PostgreSQL Cluster
Distributed Crawlers
GPU Embedding Workers
```

---

# 23. Embedding 推理架构

GPU 服务建议独立。

```text
               Embedding Gateway
                       │
         ┌─────────────┼─────────────┐
         ▼             ▼             ▼
      GPU #1         GPU #2         GPU #3
      WeMM           WeMM           WeMM
```

Worker 支持：

```text
batching
dynamic batching
backpressure
retry
priority
```

离线任务应优先 Batch。

例如：

```text
batch size = 32 / 64 / 128
```

根据显存动态调整。

---

# 24. GPU 成本控制

最大的计算成本主要来自：

```text
image embedding
video embedding
VLM rerank
```

所以推荐：

```text
所有视频
   ↓
Image Embedding
   ↓
Candidate
   ↓
少量 Video Embedding
   ↓
极少量 VLM Rerank
```

而不是：

```text
所有视频全部执行昂贵的视频理解
```

---

# 25. 缓存

建议缓存：

```text
query embedding
top query
media result
preview
rerank result
```

可使用：

```text
Redis
```

---

# 26. 搜索排序

初始可以：

```text
score =
w1 * BM25
+
w2 * ImageSimilarity
+
w3 * SubtitleSimilarity
+
w4 * VideoSimilarity
+
w5 * MetadataScore
```

后续可以训练：

```text
LambdaMART
LightGBM Ranker
Neural Reranker
```

---

# 27. VLM Rerank

最终 Top 50 可以进一步使用 VLM 判断：

```text
Query:
“一个男人站在雨里，后面有辆红色跑车”

Candidate:
Frame + Subtitle + Metadata
```

VLM 输出：

```text
relevance score
```

这样搜索准确率会显著提高。

但不能把 VLM 用在全部数据上。

---

# 28. 搜索结果聚合

Scene Search 可能返回：

```text
Movie A Scene 1
Movie A Scene 2
Movie A Scene 3
Movie B Scene 1
```

最终应进行：

```text
Scene
 ↓
Media Aggregation
```

例如：

```text
Movie A
 score 0.94
 best scene 00:23:10

Movie B
 score 0.89
 best scene 01:13:22
```

同时可以允许：

```text
展开查看全部匹配片段
```

---

# 29. 数据质量控制

每个 Scene 建议记录：

```text
quality_score
frame_quality
embedding_quality
source_quality
subtitle_quality
```

低质量数据可以降低排名。

---

# 30. 监控指标

Crawler：

```text
crawl_success_rate
assets_per_hour
error_rate
duplicate_rate
```

Embedding：

```text
images/sec
clips/sec
GPU utilization
queue length
latency
```

Search：

```text
P50
P95
P99
Recall@K
CTR
NDCG
Zero Result Rate
```

---

# 31. 推荐评估体系

建立人工 Benchmark。

例如：

```text
1000 queries
```

分类：

```text
object
scene
action
dialog
actor
movie metadata
abstract semantics
```

指标：

```text
Recall@10
MRR
NDCG@10
Precision@10
```

这是判断系统优化是否真实有效的重要基础。

---

# 32. 法律与合规边界

如果系统未来公开提供服务，建议明确：

```text
Index
≠
Unauthorized Distribution
```

原则：

1. 优先采集公开可访问的 metadata。
2. 不绕过 DRM。
3. 不绕过登录或访问控制。
4. 不存储未经授权的完整视频。
5. Source 与 Media Index 分离。
6. 对侵权内容提供下架机制。
7. 对不同国家/地区进行法律审查。
8. 对 robots.txt、站点条款和 API 使用规则进行单独评估。

BT 数据应主要作为：

```text
metadata discovery
```

而不是直接作为内容分发机制。

---

# 33. 最终推荐技术栈

## MVP

```text
Python
FastAPI
PostgreSQL
Qdrant
WeMM-Embedding
MinIO
Redis
```

优点：

```text
简单
开发快
适合验证
```

---

## Production

```text
Crawler:
Go / Python

Queue:
Kafka

Metadata:
PostgreSQL

Search:
Vespa

Object Storage:
S3 / MinIO

Embedding:
WeMM GPU Service

Cache:
Redis

Deploy:
Kubernetes
```

---

# 34. 最终推荐架构

长期建议：

```text
                Multi-source Crawler
                        │
                        ▼
                     Kafka
                        │
                        ▼
                  Asset Pipeline
                        │
                        ▼
                Entity Resolution
                        │
                        ▼
              Media / Asset Graph
                        │
                        ▼
                  Scene Pipeline
                        │
              ┌─────────┼─────────┐
              ▼         ▼         ▼
            Image      Text      Video
              │         │         │
              ▼         ▼         ▼
             WeMM      NLP       WeMM
              │         │         │
              └─────────┼─────────┘
                        ▼
                     Vespa
                        │
                        ▼
                 Hybrid Search
                        │
                        ▼
                    Reranker
                        │
                        ▼
                 Search Results
```

核心资产不是 crawler，也不是 BT 数据，而是：

```text
Media
  ↓
Asset
  ↓
Scene
  ↓
Semantic Representation
```

一旦这一层构建起来，后续无论新增：

```text
YouTube
Bilibili
Vimeo
电影库
电视剧
公开 Archive
授权网盘
BT Metadata
企业内部视频库
```

都只是新增一个 Data Connector。

---

# 35. 项目价值判断

这个项目真正有价值的地方，不是做另一个传统的视频资源搜索站，而是建立：

> **跨来源、多模态、Scene-Level 的统一视频语义索引。**

传统搜索引擎主要解决：

```text
我知道视频叫什么
```

这个系统可以进一步解决：

```text
我不知道叫什么，
但我记得里面发生了什么。
```

这属于非常明确且真实存在的搜索需求。

---

# 36. 推荐下一步

建议按以下顺序推进：

```text
Step 1
100K 视频 Storyboard 数据集

Step 2
WeMM Image Embedding

Step 3
Text → Scene Search

Step 4
加入 BM25 + Subtitle

Step 5
评估 Recall@10

Step 6
验证 Entity Resolution

Step 7
扩展到 1M 视频

Step 8
引入 BT Metadata

Step 9
Video Embedding

Step 10
Vespa + 分层 ANN
```

第一阶段最重要的验证指标不是数据规模，而是：

> 用户输入一个模糊的视频画面描述后，系统是否能在 Top 10 中找到正确视频或正确 Scene。

如果这个指标成立，再扩规模才有意义。

---

# 37. 结论

该方案在技术上具有较高可行性。

视频网站现成的 Storyboard / Preview Thumbnail 可以极大降低建立视觉索引的成本；WeMM-Embedding 可以作为统一的多模态向量基础；字幕、BM25、视频短 Clip、VLM Rerank 可以逐层提高搜索质量。

建议最终将系统定位为：

> **Universal Video Semantic Index**

而不是：

> BT Search Engine

长期核心壁垒将来自：

```text
数据规模
+
Entity Resolution
+
Scene-Level Index
+
Multi-modal Ranking
+
Dedup/Fingerprint
+
Search Relevance Data
```

这几层一旦建立起来，即使未来更换 Embedding 模型，整个数据与搜索基础设施仍然具备持续价值。
