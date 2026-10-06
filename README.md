# oci-layer-blob-store-go

用本地文件系统模拟 OCI 镜像仓库的 blob 与 manifest 存储，提供内容寻址写入、
多架构 index 解析、按平台引用解析、**本地名字/标签登记**、以登记名字为根的
垃圾回收，以及**跨本地仓库的流式镜像搬运**。
完全离线，不依赖外部服务、网络或真实凭据（仅用 Go 标准库）。

## 存储布局

```
<root>/
├── blobs/
│   └── sha256/
│       └── <64 位十六进制摘要>     # 内容寻址 blob，文件名即 sha256 摘要
├── refs/
│   ├── tags.json                   # 名字/标签登记表（原子整体重写）
│   └── .lock                       # flock 跨进程锁
└── tmp/
    └── put-* / refs-*              # 写入中的临时文件，校验/落盘后原子 rename
```

- 同一摘要的内容在磁盘上只有一份（跨镜像共享的层天然去重）。
- blob 写入先落到 `tmp/`，通过大小与摘要双重校验后才 `rename` 到
  `blobs/sha256/`。`rename` 是原子操作，并发写入不会产生部分可见文件；
  任何校验失败都会删除临时文件，不留半成品。

## 写入校验与失败原因

`Store.Put(expectedDigest, declaredSize, reader)` 对每次写入做三类判定，
错误可通过 `errors.Is` 区分：

| 情形 | 错误哨兵 |
| --- | --- |
| 实际字节数 < 声明大小（内容被截断） | `ErrTruncated` |
| 实际字节数 > 声明大小 | `ErrSizeMismatch` |
| 字节数一致但 sha256 与声明摘要不符 | `ErrDigestMismatch` |
| 摘要格式非法 / 读取不存在内容 | `ErrInvalidDigest` / `ErrNotFound` |

即使声明摘要的内容**在库中已经存在**，`Put` 也会读完这次的全部输入并重新做
大小与摘要校验：声明与实际对不上照旧拒绝，不会因为“库里已有一份”就放过
（见 `TestPutExistingContentStillVerifies`）。相同内容的重复写入则幂等命中，
库里始终只保留一份。

## 名字 / 标签登记

仓库自己“记得住有哪些镜像”，登记持久化在 `refs/tags.json`，重开仓库、换
进程都读得到。

- `Store.Tag(name, tag, digest)`：登记或改指向。`tag` 传空串时使用默认标签
  `latest`。
- `Store.Lookup(name, tag)`：按名字/标签查回摘要；未登记返回 `ErrNameNotFound`。
- `Store.Untag(name, tag)`：删除一条登记；名字下最后一个标签删掉后名字消失。
- `Store.ListTags()`：列出全部登记，按名字、标签排序。

**名字规则**：由 `/` 分隔的若干段，每段为小写字母数字、内部可含 `.`/`_`/`-`
（如 `registry.io/team/app`），非法返回 `ErrInvalidName`。
**标签规则**：字母数字开头，只含字母数字、`.`、`_`、`-`，长度 1–128，非法
返回 `ErrInvalidTag`。登记的摘要必须格式合法（`ErrInvalidDigest`）且**内容本地
确实存在**（`ErrNotFound`），否则拒绝登记。

**并发与持久化**：每次登记/改/删都在 `refs/.lock` 上取排他 `flock`，读取共享
锁；读入磁盘现状 → 在内存修改 → 整体原子重写 `tags.json`（tmp + rename）。
因此同一名字的并发登记/改指向被串行化为一次次完整的“读改写”，不会读到半截
文件，也不会两边互相覆盖丢数据。跨进程语义由 `flock(2)` 保证（Windows 下降
级为仅进程内一致）。

## manifest / index 解析与引用解析

- `ParseManifestOrIndex` 支持 OCI 与 Docker v2 的 manifest、manifest list；
  缺 `mediaType` 时按 `layers`/`manifests` 字段嗅探；无法识别返回
  `ErrUnsupportedMediaType`。
- `SelectPlatform` 按 `os`/`architecture`/`variant` 精确匹配 index 条目，
  无平台信息的条目不参与匹配；找不到平台返回 `ErrPlatformNotFound`，绝不任意
  挑选。
- `Store.Resolve(indexDigest, platform)` 从 index 解析到具体平台 manifest，
  并校验其 config 与全部 layers 均在本地存在；缺失返回 `ErrDanglingRef`。

## 以登记名字为根的回收

`Store.CollectRegistered(ctx, CollectOptions{DryRun: false})`：

1. 读取全部登记记录，取其指向的文档作为根（`Roots`）；
2. 从根沿多架构 index 一路展开到各平台 manifest，再展开到 config 与各层，
   得到全部递归可达内容（`Live`）；
3. 枚举磁盘 blob：可达 → 保留；不可达但被进行中读取钉住（pin）→ 保留
   （`Pinned`）；其余 → 删除（`Removed`）。

判定完全以**最新登记**为准：`Untag` 或 `Tag` 改指向后，原来可达、现在不可达
的内容在下一轮回收时会被清理。任何登记根悬空（指向缺失内容）或引用边损坏
（文档读不出/解析不了/引用缺失）都返回 `ErrDanglingRef` 并中止，**不删除任何
东西**，避免把图损坏误判成不可达。

- `CollectOptions{DryRun: true}` 只预演：给出 `Roots/Live/Pinned/Removed`
  判定结果但不删除，用于正式清理前确认。
- 回收幂等：对相同登记集合重复执行，第二轮 `Removed` 为空。
- 进行中的读取受 pin 保护，读取句柄关闭后再回收才会删除对应内容。

### 与旧回收 API 的兼容

上一轮的 `Store.Collect(ctx, []*Manifest)` 仍然保留、签名与行为不变，适合
“引用关系由调用方在外部维护”的旧用法。新代码推荐用
`CollectRegistered`，根由仓库自身的登记记录给出。两者的保留/删除/pin 规则一
致，`GCReport` 与 `CollectResult` 各自独立，旧用例无回归。

## 跨仓库搬运

`dst.CopyFrom(ctx, src, name, tag)` 把 `src` 中已登记的 `name:tag`（tag 为空按
源端 `latest`）连同其递归引用到的全部内容搬到 `dst`，并在 `dst` 以相同名字/
标签登记：

- 先在源端做整个引用闭包的**存在性预检**：源端缺 blob 返回 `ErrNotFound`，
  源文档损坏到无法解析返回 `ErrDigestMismatch`，错误原因可区分；
- 内容按 **32KiB 缓冲流式**经 `Put` 传输，大层/大镜像不会整份读进内存；
  传输时逐流做大小与 sha256 校验，源内容被截断/损坏分别返回
  `ErrTruncated`/`ErrDigestMismatch`/`ErrSizeMismatch`；
- **内容寻址去重**：目标已存在的 blob 不重复传输（`CopyReport.Present`），
  新传输的记在 `Copied`，相同内容目标只存一份；
- **不成半成品**：所有 blob 完整落盘并校验通过后才在目标登记名字，中途失败
  目标仓库仍自洽（可能多了些可独立存在的 blob，但绝不会有指向残缺内容的
  标签）；失败修复后**重试接着做**，已存在内容自动跳过、不重复堆内容。

## 本地验证

无需网络与外部依赖（标准库实现）：

```bash
go build ./...
go vet ./...
gofmt -l .            # 应无输出
go test ./... -race -count=1 -v
```

测试覆盖：

- 名字/标签：登记、查询、改指向、删除、列表、默认标签
  （`TestTagLookupRetagUntagList`）；
- 名字/标签/摘要非法与指向不存在内容的拒绝（`TestTagValidation`）；
- 登记在重开仓库与**独立子进程**后仍可读（`TestTagsPersistAcrossReopen`、
  `TestTagsPersistAcrossProcess`）；
- 多 goroutine 并发登记不同标签、并发改同一标签，收敛为完整结果、无丢失
  （`TestConcurrentTagNoLostWrites`，`-race` 下运行）；
- 以登记为根、经多架构 index 链的递归回收、预演不删除、改指向改变可达性、
  悬空根中止、幂等、pin 保护（`gc_registered_test.go` 中各例）；
- 跨仓库搬运：整图搬运后名字与逐字节内容一致、共享内容去重、源端缺内容与
  内容损坏分别报错、失败补齐后重试不重复、未知名字拒绝、20MiB 大层流式
  （`copy_test.go` 中各例）；
- 已存在内容再写一遍，声明/实际对不上仍按三类原因拒绝
  （`TestPutExistingContentStillVerifies`）；
- 上一轮的摘要校验、并发写、并发读回收、多架构解析与旧 GC 用例全部保留。

测试日志会打印引用关系（`manifest -> digest...`）、每轮 GC 的
`roots/live/pinned/removed` 判定依据，以及搬运的 `copied/present` 明细，
方便核对“凭什么保留/删除/跳过”。
