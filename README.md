# oci-layer-blob-store-go

用本地文件系统模拟 OCI 镜像仓库的 blob 与 manifest 存储，提供内容寻址写入、
多架构 index 解析、按平台引用解析，以及基于引用关系的垃圾回收。
完全离线，不依赖外部服务、网络或真实凭据。

## 存储布局

```
<root>/
├── blobs/
│   └── sha256/
│       └── <64 位十六进制摘要>     # 内容寻址 blob，文件名即 sha256 摘要
└── tmp/
    └── put-*                       # 写入中的临时文件，校验通过后原子 rename
```

- 同一摘要的内容在磁盘上只有一份（跨镜像共享的层天然去重）。
- 写入先落到 `tmp/`，通过大小与摘要双重校验后才 `rename` 到
  `blobs/sha256/`。`rename` 是原子操作，因此并发写入不会产生部分可见的文件；
  任何校验失败都会删除临时文件，`blobs/` 中不会留下半成品。

## 写入校验与失败原因

`Store.Put(expectedDigest, declaredSize, reader)` 对每次写入做三类判定，
错误可通过 `errors.Is` 区分：

| 情形 | 错误哨兵 |
| --- | --- |
| 实际字节数 < 声明大小（内容被截断） | `ErrTruncated` |
| 实际字节数 > 声明大小 | `ErrSizeMismatch` |
| 字节数一致但 sha256 与声明摘要不符 | `ErrDigestMismatch` |
| 摘要格式非法 / 读取不存在内容 | `ErrInvalidDigest` / `ErrNotFound` |

## manifest / index 解析与引用解析

- `ParseManifestOrIndex` 支持 OCI 与 Docker v2 的 manifest、manifest list
  （`application/vnd.oci.image.manifest.v1+json`、
  `application/vnd.oci.image.index.v1+json`、
  `application/vnd.docker.distribution.manifest.v2+json`、
  `...manifest.list.v2+json`）；缺 `mediaType` 时按 `layers`/`manifests`
  字段嗅探；无法识别返回 `ErrUnsupportedMediaType`。
- `SelectPlatform` 按 `os`/`architecture`/`variant` 精确匹配 index 条目，
  无平台信息的条目不参与匹配；找不到平台返回 `ErrPlatformNotFound`，
  绝不任意挑选或返回空结果。
- `Store.Resolve(indexDigest, platform)` 从 index 解析到具体平台的 manifest，
  并校验其 config 与全部 layers 均在本地存在；引用的 blob 缺失时返回
  `ErrDanglingRef`，错误信息包含缺失摘要。

## 引用与回收规则

- 一个 manifest 的引用集合 = `config.digest` + 所有 `layers[].digest`。
- `Store.Collect(ctx, liveManifests)` 枚举全部 blob：
  - 出现在任一存活 manifest 引用集合中 → 保留（`GCReport.Kept`）；
  - 被进行中读取钉住（pin）→ 保留。`Get` 返回的 `ReadCloser` 在
    `Close` 前持有该摘要的 pin，回收器不会删除它（删除不存在的文件在
    POSIX 上也不影响已打开的句柄，pin 机制使该保证在进程内显式可测）；
  - 其余 → 删除（`GCReport.Removed`）。
- 回收是幂等的：对相同的存活 manifest 集合重复执行，第二次 `Removed` 为空；
  对已不存在的文件执行删除不报错。
- `GCReport` 按摘要排序输出 kept/removed，日志中可直接看到判定依据。

## 本地验证

无需网络与外部依赖（标准库实现）：

```bash
go build ./...
go vet ./...
go test ./... -race -count=1 -v
```

测试覆盖：

- 摘要不符 / 截断 / 大小不符三类失败的可区分原因，并断言失败后无半成品、
  无临时文件泄漏（`TestPutFailureReasons`）；
- 同一摘要多副本写入只落盘一份（`TestDedupSameDigestSingleFile`）；
- 32 个 goroutine 并发写同一内容，收敛为单个完整 blob
  （`TestConcurrentPutSameContent`）；
- 并发读取期间触发回收，进行中读取不被破坏（`TestConcurrentReadWhileGCNeverCorrupts`）；
- 多平台 index 精确筛选、未知媒体类型拒绝（`TestSelectPlatformMultiArch` 等）；
- 悬空 index 条目与悬空层引用被明确拒绝（`TestResolveDanglingRef`）；
- 跨镜像共享层在一个镜像删除后仍保留、孤儿层被回收、重复回收幂等
  （`TestGCSharingAndIdempotent`）。

测试日志会打印摘要、`manifest -> digest...` 引用关系以及每轮 GC 的
kept/removed 判定依据。
