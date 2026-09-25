# oci-layer-blob-store-go

在**本地文件系统**上模拟 OCI 镜像仓库的 blob / manifest 存储，提供：

- 按内容摘要（`sha256` / `sha384` / `sha512`）写入与读取层，相同摘要全局只落盘一份；
- image manifest 与多架构 image index 的解析、按平台筛选与引用解析；
- 并发安全的访问，以及基于引用关系（tag → index → manifest → config/layers）的标记-清除回收；
- 全程离线、无外部服务、无网络、无真实凭据。

## 存储布局

```
<root>/
├── blobs/
│   └── <algorithm>/<hex>        # 所有内容寻址对象：层、config、manifest、index
├── tmp/
│   └── ingest-*                 # 写入过程中的临时文件，提交（rename）后即消失
└── refs/
    └── <url-path-escaped tag>   # 命名引用文件，内容为 JSON Descriptor
                                 # {"mediaType":..., "digest":..., "size":...}
```

要点：

- blob 文件名即摘要，路径为 `blobs/<algorithm>/<hex>`；内容写入后权限置为只读。
- 写入采用“写 `tmp/` 临时文件 → 完整读取并校验 → fsync → 原子 `rename` 到
  `blobs/...`”的提交方式。任何校验失败都会删除临时文件，**不留半成品，也不覆盖
  已有 blob**。
- 相同摘要的内容视为同一对象：并发或重复写入命中同一目标时丢弃临时副本，磁盘上始终只有一份。

## 写入校验与可区分的失败原因

写入（`PutBlob`）在流式读取的同时计算摘要与字节数，读完即校验，失败原因可通过
`errors.Is` 区分：

| 场景 | 错误 | 携带信息 |
| --- | --- | --- |
| 摘要字符串格式/算法/长度/十六进制非法 | `ErrInvalidDigest` | 原始字符串 |
| 实际摘要与声明摘要不一致 | `ErrDigestMismatch` | `Expected` / `Got` 摘要 |
| 实际字节数 **短于** 声明大小（截断 / 流中断） | `ErrTruncated` | `Declared` / `Actual` |
| 实际字节数 **长于** 声明大小 | `ErrSizeMismatch` | `Declared` / `Actual` |
| 按摘要读取不存在的 blob | `ErrNotFound` | 摘要 |
| tag 不存在 | `ErrRefNotFound` | tag 名 |

截断通常也会导致摘要不符；此时以**更直接的原因** `ErrTruncated` 为准（先判大小、
再判摘要），保证“截断”和“摘要不符”不会混淆。所有拒绝路径都不会留下 blob 或临时文件。

## 清单解析与按平台引用解析

支持的内容类型：

- `application/vnd.oci.image.manifest.v1+json`
- `application/vnd.oci.image.index.v1+json`（允许 index 嵌套指向 index）

`Resolve(root, platform)` 的 `root` 可以是 tag 名，也可以是 `sha256:...` 摘要。
解析严格按规则失败，**绝不任意挑选或返回空结果**：

| 场景 | 错误 |
| --- | --- |
| index 中没有匹配目标平台的条目 | `ErrPlatformNotFound`（携带 `Wanted` 与 `Available` 平台列表） |
| tag 指向的 blob 缺失 / index 选中条目内容缺失 | `ErrDanglingReference`（携带 `Location` 与 `Missing` 摘要） |
| 内容不是受支持的清单类型 | `ErrUnsupportedMediaType` |
| tag 不存在 | `ErrRefNotFound` |

平台匹配规则：`os` 与 `architecture` 必须相等；请求方显式给出 `variant` 时必须
精确相等（`v7` 不会降级命中 `v8`），请求方未给 `variant` 时接受该平台的任意 variant。

## 引用关系与回收（GC）

引用图：

```
refs/<tag>  ──►  manifest 或 index（根）
index        ──►  子 manifest / 子 index（按 descriptors）
manifest     ──►  config blob
manifest     ──►  layer blobs
```

`GC()` 执行标准标记-清除，返回 `GCReport`（根、可达集合、删除列表、悬空引用）：

1. **标记**：以 `refs/` 下全部命名引用为根，沿 index/manifest 的描述符递归标记
   可达 blob；层、config 等非清单内容作为叶子终止。
2. **清除**：仅删除标记结束时仍不可达的 blob。
3. **悬空引用**：根或描述符指向缺失内容时记录到 `GCReport.Dangling`（含位置与缺失
   摘要），不阻断其余回收。
4. **幂等**：重复执行第二次 `Removed` 为空、可达集合不变。

### 并发与“进行中的读取不被破坏”

Store 用一把 `gcMu`（`sync.RWMutex`）划定清晰边界：

- 所有 blob 读/写、tag 变更持有**读锁**；读取句柄在 `Close()` 时才释放读锁。
- GC 持有**写锁**完成“标记 + 清除”全过程。

因此：

- GC 开始前已在途的读取必然结束后才会删除任何内容，进行中的读取不会被破坏；
- GC 期间提交的写入与 tag 落在 GC 之后，其内容不会被本轮误删，下一轮 GC 再按最新
  引用关系判定；
- “写 manifest”和“打 tag”是两个步骤，若内容在打 tag 前恰好被 GC 回收，`Tag`
  返回 `ErrNotFound`，调用方重试即可（与真实 push 的竞态语义一致）。

## 本地验证

无需任何外部依赖，Go 1.25+：

```bash
go build ./...
go vet ./...
go test ./... -count=1                 # 功能与失败原因
go test ./... -race -count=3           # 并发读写 / GC 交错 + 竞争检测
go test ./... -v                       # 查看单测日志中的摘要、引用关系与判定依据
```

测试覆盖：

- 摘要格式非法、摘要不符、截断、超长、半截流，且失败后无 blob / 临时文件残留；
- 相同摘要并发 32 写仅落盘一份、8 读并发于持续写入期间无损坏（`-race`）；
- 多平台筛选（含 variant 精确匹配与 index 嵌套）、平台缺失、悬空 tag / 悬空条目、
  不支持内容类型；
- 跨镜像共享层保留、untag 后精确回收其 manifest/config/layer、悬空引用上报、
  GC 幂等、进行中读取不被破坏、GC 与写入交错后全部 tag 仍可解析。

## 主要 API

```go
s, _ := ocistore.Open(rootDir)
desc, err := s.PutBlob(ctx, r, expectedDigest, declaredSize) // expectedDigest 可为 ""
rc, desc, err := s.GetBlob(ctx, digest)                      // Close 前 GC 等待
md, err := s.PutManifest(ctx, mediaType, raw)
desc, err := s.Tag(ctx, "myimage:v1", manifestDigest)
res, err := s.Resolve(ctx, "myimage:v1", ocistore.Platform{OS: "linux", Architecture: "amd64"})
report, err := s.GC(ctx)
```
