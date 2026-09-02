# 电影关键帧获取与语义索引中间层设计

## 1. 背景

目标是构建一个只面向**电影**的视频语义搜索系统。

上层希望统一得到：

```text
Movie
  ↓
timestamp + representative frame
  ↓
WeMM-Embedding
  ↓
Scene / Shot Semantic Index
```

问题在于：各视频网站虽然经常在播放器中提供拖动预览图、storyboard、sprite 或 WebVTT，但实现方式并不统一，也并不是所有站点都能稳定直接获取。

因此不能把“网站已有关键帧”当成唯一数据来源。

推荐做一个独立的：

> **Movie Frame Acquisition Service**

负责填平以下数据源差异：

```text
Storyboard
WebVTT
Sprite
Thumbnail API
HLS
DASH
MP4
Local File
BT / External Asset
```

并统一输出关键帧。

## 2. 核心结论

对于电影场景，推荐使用“双通道”：

```text
                    Movie Source
                         │
                         ▼
                    Source Adapter
                         │
             ┌───────────┴───────────┐
             ▼                       ▼
       Preview Available       Stream Available
             │                       │
             ▼                       ▼
      Storyboard Parser        Sparse Sampler
             │                       │
             │                Adaptive Sampling
             │                       │
             └───────────┬───────────┘
                         ▼
                  Frame Processing
                         │
               dedup / blur / black
                         │
                         ▼
                       WeMM
```

对于热门、高价值或需要高召回率的电影，再升级为：

```text
Full Stream
   ↓
TransNetV2
   ↓
Shot Boundary
   ↓
Representative Frames
   ↓
WeMM
```

电影网站提供的 storyboard 应当看成低成本优化路径，而不是系统唯一依赖。

## 3. 为什么不能只依赖视频网站现成关键帧

不同视频网站实现拖动预览的方式差异很大。常见模式包括：

```text
preview_001.jpg
preview_002.jpg
preview_003.jpg
```

或者：

```text
sprite.jpg
+
WebVTT
```

例如：

```text
00:00 --> 00:10
sprite.jpg#xywh=0,0,160,90
```

也有专用 Storyboard API、播放器 Session API、临时 Token、DRM 或登录状态绑定。

因此不存在一个成熟项目可以长期可靠地统一支持所有电影网站并直接获得关键帧。从系统架构角度，必须有 fallback。

## 4. 推荐开源组件

### 4.1 yt-dlp

推荐定位：**Source Adapter Framework**，而不是 Keyframe Detector。

输入 URL 后，统一解析：

```text
title
duration
thumbnail
subtitles
formats
manifest
HLS URL
DASH URL
MP4 URL
metadata
```

对于部分站点还可以发现 storyboard / thumbnail tracks。

推荐链路：

```text
Movie URL
   ↓
yt-dlp
   ↓
SourceInfo
```

### 4.2 TransNetV2

推荐定位：**电影 Shot Boundary Detector**。

电影不适合固定每 N 秒抽帧。120 分钟电影如果每 10 秒抽一帧只有 720 帧，而实际可能包含 1000~3000 个 shots，很多重要镜头仅 1~5 秒。

推荐：

```text
Video
 ↓
Continuous Frames
 ↓
TransNetV2
 ↓
Shot Boundary
 ↓
Representative Frame
```

默认可取 middle frame；长 Shot 可取 25% / 50% / 75%。

### 4.3 PySceneDetect

推荐定位：**CPU / MVP / Simple Fallback**。

优点是简单、成熟、易和 FFmpeg 配合。MVP 可直接：

```text
movie
 ↓
PySceneDetect
 ↓
scene list
 ↓
save-images
 ↓
WeMM
```

长期面向电影时，建议：

```text
TransNetV2 > PySceneDetect
```

### 4.4 frameko

值得借鉴其 pipeline：

```text
Scene Detection
+
Representative Frame Extraction
+
FFmpeg
+
pHash Dedup
+
Blur Filtering
+
Metadata
```

尤其适合参考 pHash、模糊过滤和 metadata 组织方式。

### 4.5 clip-video-scene-search

它证明了以下工程链条可直接跑通：

```text
Scene Detection
 ↓
Keyframe
 ↓
Embedding
 ↓
FAISS
 ↓
Text-to-Scene Search
```

其 OpenCLIP 可以替换为 WeMM，本地视频入口可替换成 yt-dlp + Remote Stream / Preview。

## 5. 推荐统一接口

```python
class MovieFrameExtractor:
    async def probe(self, source) -> MovieInfo:
        ...

    async def extract_storyboard(self, source):
        ...

    async def sparse_sample(self, source):
        ...

    async def adaptive_sample(self, source):
        ...

    async def detect_shots(self, source):
        ...
```

上层不关心底层来源。

## 6. Frame 统一输出

```json
{
  "movie_id": "...",
  "timestamp": 123.4,
  "source": "storyboard",
  "frame_url": "...",
  "local_path": "...",
  "width": 320,
  "height": 180,
  "quality_score": 0.92
}
```

source 可取：

```text
storyboard
sprite
thumbnail
sparse_sample
adaptive_sample
transnet
pyscenedetect
```

## 7. Storyboard 优先策略

优先级建议：

```text
1. Storyboard
2. WebVTT + Sprite
3. Thumbnail Track
4. Preview API
5. Sparse Stream Sampling
6. Full Shot Detection
```

Storyboard 成本最低，因为通常已经由视频网站预计算完成，无需完整下载视频和 decode 全片。

## 8. Sparse Sampling

当没有 Preview 时，不应第一时间下载整部电影。

推荐：

```text
Remote Stream
 ↓
Sparse Sampling
```

例如每 30 秒取一帧。120 分钟电影只需要约 240 帧。

对于 WeMM，通常优先选 240p / 360p 低码率流即可显著降低网络、decode、GPU 和存储成本。

## 9. Adaptive Sampling

固定 30 秒采样会漏掉多个 Shot，因此推荐：

```text
Coarse Sampling
      ↓
WeMM Embedding
      ↓
Neighbor Similarity
      ↓
Detect Large Semantic Change
      ↓
Fine Sampling
```

当相邻粗采样向量相似度明显下降时，在对应时间窗口追加采样。

例如 30 秒窗口可增加：

```text
+5
+10
+15
+20
+25
```

最大优势是不需要连续 decode 整部电影，非常适合海量远程资源。

## 10. HLS Sampling

如果源是 m3u8，可解析 segment。

例如：

```text
6 sec / segment
```

2 小时电影约 1200 个 segment，但无需全部下载，可只选择 200~400 个，并优先最低码率流。

## 11. MP4 Range Sampling

如果服务器支持 HTTP Range，可以配合 FFmpeg 做远程 Seek 与按需读取，但需考虑：

```text
MP4 moov atom
keyframe location
server range support
codec GOP structure
```

当前 Go MVP 已落地两层适配：

1. 对 progressive MP4/MOV，使用 `github.com/Eyevinn/mp4ff` 的 lazy `mdat` 模式读取 `moov`、视频轨道 sample table、同步样本（I 帧）的位置、时间戳和编码信息。读取索引时不会读取媒体 payload；索引不完整、fragmented MP4 或文件损坏时回退到 FFmpeg。
2. 对远程 URL，Go 侧使用按 4 MiB 对齐的 HTTP Range 缓存，并通过仅监听 `127.0.0.1` 的临时 HTTP adapter 提供给命令行 FFmpeg。多个抽帧进程共享这个缓存，避免同一 GOP 被重复拉取。

progressive MP4 的抽帧会将目标时间先定位到前一个 I 帧，再让 FFmpeg 从该位置解码到目标画面。这样不需要下载原视频，但仍可能读取目标 GOP 内的若干非 I 帧；单独下载一个 H.264/H.265 I 帧样本通常缺少完整容器和 codec configuration，不能可靠地直接解码成 JPG。

如果远程服务器不支持 Range，或者容器不是当前 MP4 索引器覆盖的类型，通用 FFmpeg adapter 负责兼容性；它可能因为容器/编码器需要而读取较大范围，不能承诺始终只产生稀疏流量。远程处理结果会记录 `remote_range` 统计和 `remote_mp4_index` 摘要，便于判断实际流量。

## 12. 三种采集模式

| 模式 | 用途 | 流量 | 完整度 |
|---|---|---:|---:|
| Storyboard | 站点已有 preview | 极低 | 中~高 |
| Sparse Adaptive | 远程视频 | 低 | 中 |
| Full TransNetV2 | 高价值电影 | 高 | 很高 |

推荐根据电影热度做 Progressive Indexing：

```text
Cold Movie
→ Storyboard / Sparse

Warm Movie
→ Adaptive Sampling

Hot Movie
→ Full TransNetV2
```

## 13. Frame 过滤

在发送给 WeMM 前，应过滤：

```text
black frame
white frame
blur
transition frame
duplicate frame
credits
logo screen
```

推荐手段：

- 黑帧：mean luminance
- 模糊：Laplacian variance
- Cheap dedup：pHash + Hamming Distance
- Semantic dedup：WeMM cosine similarity

## 14. 与 WeMM 的衔接

最终 pipeline：

```text
Frame Acquisition
       ↓
Frame Filter
       ↓
Frame Dedup
       ↓
WeMM Image Embedding
       ↓
Scene Vector
```

统一输出：

```json
{
  "movie_id": "...",
  "timestamp": 123,
  "embedding": [],
  "source": "adaptive_sample"
}
```

## 15. 推荐整体架构

```text
                   Movie URL / Asset
                          │
                          ▼
                       yt-dlp
                          │
                          ▼
                    SourceInfo
                          │
          ┌───────────────┼─────────────────┐
          │               │                 │
          ▼               ▼                 ▼
      Storyboard         HLS               MP4
          │               │                 │
          ▼               └────────┬────────┘
   Storyboard Parser               │
          │                        ▼
          │                  Sparse Sampler
          │                        │
          │                  Adaptive Sampler
          │                        │
          └──────────────┬─────────┘
                         ▼
                  Frame Processor
                         │
                ┌────────┼────────┐
                ▼        ▼        ▼
              pHash     Blur     Black
                │        │        │
                └────────┼────────┘
                         ▼
                       WeMM
                         │
                         ▼
                   Scene Index
```

高价值电影旁路：

```text
Hot Movie
   ↓
Full Stream
   ↓
TransNetV2
   ↓
Shots
   ↓
Representative Frames
   ↓
WeMM
```

## 16. 推荐模块划分

### source-adapter

基础：yt-dlp

负责：

```text
URL
 ↓
Metadata / Stream / Preview
```

### storyboard-parser

负责：

```text
WebVTT
Sprite
Thumbnail Track
Storyboard
```

统一转成：

```text
timestamp + image
```

### sparse-sampler

负责 Remote HLS / Remote MP4 随机访问抽帧。

### adaptive-sampler

负责：

```text
Coarse Sample
 ↓
Difference Detection
 ↓
Refine
```

### shot-detector

后端：

```text
TransNetV2
PySceneDetect
```

### frame-filter

负责：

```text
black
blur
transition
duplicate
```

### embedding-worker

负责 WeMM。

## 17. 推荐实施阶段

### Phase 1

```text
yt-dlp
+
FFmpeg
+
PySceneDetect
+
pHash
+
WeMM
```

先完成：

```text
URL
 ↓
SourceInfo
 ↓
Frame
 ↓
Embedding
```

建议规模：1K~10K movies。

### Phase 2

加入：

```text
Storyboard Parser
HLS Segment Sampling
Adaptive Sampling
```

目标是显著降低下载流量。

### Phase 3

加入 TransNetV2，对热门电影生成完整 Shot Index。

### Phase 4

实现 Progressive Indexing，根据：

```text
query count
CTR
movie popularity
index quality
```

动态升级：

```text
Sparse
→ Adaptive
→ Full Shot
```

## 18. 最终推荐

不建议寻找一个“支持所有视频网站并直接获得关键帧”的单一开源项目，这类方案长期不可控。

更合适的是自己抽象一个：

> **Movie Frame Acquisition Service**

核心依赖：

```text
yt-dlp
+
FFmpeg
+
TransNetV2
+
PySceneDetect
+
pHash
+
WeMM
```

其中：

- yt-dlp：解决网站差异
- Storyboard Parser：解决最低成本 Preview 获取
- Sparse / Adaptive Sampler：解决无 storyboard 时的低成本 fallback
- TransNetV2：解决热门电影完整 Shot Index
- WeMM：解决统一语义表示

最终上层永远只看到：

```text
Movie
 ↓
timestamp
 ↓
representative frame
 ↓
embedding
```

这样未来增加视频网站、电影资源库、BT Asset 或授权视频源，都只需要增加新的 Source Adapter，而不需要修改搜索核心。
