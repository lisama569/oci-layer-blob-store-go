# oci-layer-blob-store-go

用本地文件系统模拟 OCI 镜像仓库的 blob 与 manifest 存储，提供内容寻址写入、
多架构 index 解析、按平台引用解析、**名字/标签引用登记**、以登记引用为根的
垃圾回收，以及跨本地仓库的镜像搬运。完全离线，不依赖外部服务、网络或真实凭据。

## 存储布局

```
<root>/
├── blobs/
│   └── sha256/
│       └── <64 位十六进制摘要>     # 内容寻址 blob，文件名即 sha256 摘要
├── refs.json                       # 名字/标签 -> 摘要 的登记表（原子落盘）
└── tmp/
    └── put-* / refs-*              # 写入中的临时文件，校验通过后原子 rename
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

**dedup 命中也校验**：内容已在库中时，`Put` 仍会把声明与库中实际内容比对
（声明大小偏大 → `ErrTruncated`，偏小 → `ErrSizeMismatch`；库中文件被篡改、
与摘要名不符 → `ErrDigestMismatch`），不会因为“库里已有一份”就放过对不上
的声明。

## 名字 / 标签引用登记

仓库自己记住“有哪些镜像”，引用规则如下：

- 引用形如 `name` 或 `name:tag`；省略标签时默认为 `latest`
  （`ParseReference("app/web")` 等价于 `app/web:latest`）。
- 名字须为小写字母数字开头、可含 `. _ - /` 分隔符；标签须以字母数字或 `_`
  开头，可含 `. _ -`，最长 128。非法时分别返回 `ErrInvalidName` /
  `ErrInvalidTag`。
- `Store.Tag(ref, digest)` 登记或改指向：摘要格式非法返回
  `ErrInvalidDigest`，摘要内容不在本地返回 `ErrNotFound`，非法登记一律不落盘。
- `Store.ResolveRef(ref)` 查回摘要、`Store.Untag(ref)` 删除登记，
  未登记均返回 `ErrRefNotFound`；`Store.ListRefs` / `Store.RefNames`
  列出当前全部登记。
- 登记表持久化在 `<root>/refs.json`：每次变更先写临时文件再原子 `rename`，
  进程内由互斥锁串行化。并发登记/改指向同一名字不会读到半截、也不会互相
  覆盖丢数据，最终总留下一个完整结果；重新打开仓库（或换个进程）读到的
  与之前一致。改指向后，谁还被引用以最新登记为准。

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

## 垃圾回收

### 新方式：以登记引用为根（`Store.Sweep`）

`Store.Sweep(ctx, SweepOptions{DryRun})` 从 `refs.json` 中全部登记名字出发，
沿 index -> manifest -> config/layers 递归展开引用闭包：

- 被任一登记引用递归可达 → 保留，理由记为 `referenced by <name:tag>`；
- 被进行中读取钉住（`Get` 返回的 `ReadCloser` 未 `Close`）→ 保留，
  理由记为 `pinned by in-flight read`；
- 其余 → 删除，理由记为 `unreferenced`。

`SweepReport` 逐 blob 记录保留/删除及依据，测试日志可直接看到每轮判定。
`DryRun: true` 时只判定不删除（`Removed` 表示“将会被删”），便于正式清理前
预演。回收幂等：重复执行不会误删，第二次 `Removed` 为空。

### 兼容范围

旧接口 `Store.Collect(ctx, liveManifests)` 保留可用：调用方自行传入存活
manifest 列表，按“config + layers 引用集合”判定，行为与上一版完全一致。
两者可混用，但新代码建议用 `Sweep`，让仓库自己从登记引用出发判断。

## 跨仓库搬运

`CopyImage(ctx, dst, src, ref)` 把源仓库中已登记的引用连同其递归引用的
全部内容搬到目标仓库，并在目标仓库登记同名引用：

- **流式**：逐 blob 边读边写（`Get` -> `Put`），不把整份内容读进内存，
  大镜像（层多、单层大）也适用；
- **去重**：目标仓库已有的内容只存一份（`CopyReport.Reused` 记录跳过的
  摘要，`Copied` 记录实际写入的）；
- **可区分失败**：源端内容缺失报 `ErrNotFound`，源端内容与声明摘要不符
  报 `ErrDigestMismatch`（目标侧写入校验同样生效）；
- **可重试**：全部内容落地后才在目标仓库登记名字，中途失败不会留下指向
  残缺内容的名字；重试时已搬运的内容自动去重，不重复堆内容。

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
- 内容已在库中再写一遍时声明仍被校验（`TestPutExistingStillValidatesDeclaration`）；
- 同一摘要多副本写入只落盘一份（`TestDedupSameDigestSingleFile`）；
- 32 个 goroutine 并发写同一内容，收敛为单个完整 blob
  （`TestConcurrentPutSameContent`）；
- 并发读取期间触发回收，进行中读取不被破坏（`TestConcurrentReadWhileGCNeverCorrupts`）；
- 多平台 index 精确筛选、未知媒体类型拒绝（`TestSelectPlatformMultiArch` 等）；
- 悬空 index 条目与悬空层引用被明确拒绝（`TestResolveDanglingRef`）；
- 旧接口回收：共享层保留、孤儿层回收、重复回收幂等（`TestGCSharingAndIdempotent`）；
- 名字/标签的登记、查询、改指向、删除、校验、跨进程持久化与并发更新
  （`TestTagResolveRepointUntag`、`TestTagValidation`、
  `TestRefsPersistAcrossReopen`、`TestConcurrentTagSameName`）；
- 以登记为根的递归回收：多架构 index 链整体存活、改指向后旧内容被清、
  预演不删、钉住内容保留、重复回收幂等（`TestSweep*`）；
- 跨仓库搬运：基本搬运、共享层去重、源端缺内容（`ErrNotFound`）与
  源端内容被篡改（`ErrDigestMismatch`）可区分、失败重试续搬、
  大镜像流式搬运（`TestCopyImage*`）。

测试日志会打印摘要、引用关系以及每轮回收逐 blob 的保留/删除依据。
