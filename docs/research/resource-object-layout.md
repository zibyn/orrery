# Resource 的 S3 对象布局：按需读取的只读 FUSE 用哪种布局

调研日期 **2026-11-13**。问题：Resource 改为经只读 FUSE 层按需挂载以后（读时才去取，Node 上以 4 MiB 为单位缓存），对象存储上该用哪种对象布局。比较五种布局：A 现状一文件一对象；B 在 A 的基础上给 manifest 加分块哈希；C 固定 4 MiB 分块，每块一个对象；D 内容定义分块（CDC）加打包；E 只打包小文件。另外核对 S3 的分段校验和能不能在不改键的前提下让范围读可校验，以及 RustFS、Garage、SeaweedFS 是否支持。

只用一手来源：官方文档、API 模型、设计文档、源码。英文原文照引，每条结论附链接。没能核实的标「未确认」。

仓库内的现状只作背景，不重新论证：ADR-0012、ADR-0021（CAS 一文件一对象，键 `cas/<owner_id>/sha256/<hex>`，按 owner 去重，预签名 PUT 带 `x-amz-checksum-sha256`，publish 时用 checksum mode 的 `HeadObject` 校验，manifest 最多 1,000,000 条，GC 看数据库引用再过宽限期清扫），`internal/manifest/manifest.go`（`MaxFileSize = 5 << 30`），`internal/objectstore/objectstore.go`，`internal/agent/resources.go`（现在整对象下载，校验后放入缓存），`docs/spec/schema.md`（`resource_version_objects`、`cas_objects`）。JuiceFS 的事实沿用 `workspace-shared-storage-s3.md` §2，厂商的小文件建议沿用 `code-vs-training-data-vendor-practice.md`。

**来源说明**：本环境连不上 `docs.aws.amazon.com` 和 `aws.amazon.com`，TLS 被断开。AWS 用户指南改从 AWS 中国区的官方镜像 `docs.amazonaws.cn/en_us/...`（同路径、同内容的 `.md` 版本）读取，下文链接仍给 `docs.aws.amazon.com` 的规范路径。API 层的事实来自 AWS 官方 Smithy 模型 [`aws/api-models-aws` s3-2006-03-01.json](https://github.com/aws/api-models-aws/blob/main/models/s3/service/2006-03-01/s3-2006-03-01.json)。S3 价格页打不开，价格标「未确认」。三个对象存储读的源码版本：Garage tag `v2.4.1`（与 ADR-0021 实测的版本相同）；SeaweedFS `master@a576398`；RustFS `master@4a1d542`（最新 release 是 1.0.5，ADR-0021 测的是 1.0.2）。后两个比实测版本新，所以在已部署的版本上是否同样成立，**未确认**。

---

## 0. 先读这几条

1. **现状 A 放不下 10–100 GB 的模型权重，也校验不了它们。** S3 单次 PUT 上限 5 GB（"With a single `PUT` operation, you can upload a single object up to 5 GB in size"，[upload-objects](https://docs.aws.amazon.com/AmazonS3/latest/userguide/upload-objects.html)），orrery 的 `MaxFileSize` 也是 5 GiB。更大的文件只能走 multipart，可 multipart 下 SHA-256 **只能是 composite**（"SHA-256 (SHA256) | No | Yes"，"Full object checksums in multipart uploads are only available for CRC-based checksums"，[checking-object-integrity-upload](https://docs.aws.amazon.com/AmazonS3/latest/userguide/checking-object-integrity-upload.html)）。也就是说，对象存储再也替 Control Plane 校验不了整文件的 SHA-256。要保住「Control Plane 不读内容、也不信任未经校验的客户端摘要」，大文件的地址必须是按块计算的。
2. **整对象上的范围读校验不了。** S3 只有在范围恰好对齐**一个 multipart 分段**时才返回该段的校验和（"specifying a part number or byte range that aligns with a single part"，同上页）。Garage v2.4.1 对 `Range` 和 `partNumber` 读都关掉了校验和（源码 TODO）。SeaweedFS 和 RustFS 只在整对象读时返回校验和。所以 FUSE 按需读的 4 MiB 单元想要能校验，靠的只能是**每个单元自己就是一个已被对象存储校验过的对象**（C），或者一张 Control Plane 能间接验证的分段摘要表（B 的 composite 变体）。
3. **小文件按对象计数，成本主要不在钱，在请求次数。** 1,000,000 个 300 KB 文件，A、B、C 都是 100 万次 PUT、100 万个预签名、publish 时 100 万次 HEAD、冷读 100 万次 GET；AWS 的数字是 "smaller requests (for example, less than 512 KB), where median latencies are often in the tens of milliseconds"（[design patterns](https://docs.aws.amazon.com/AmazonS3/latest/userguide/optimizing-performance-design-patterns.html)）。打包（D、E）能把这些降到约 1.8 万次（16 MiB 的包），代价是包内偏移只能由客户端声明、要另建位置索引、GC 要 repack。restic、kopia、borg 三家都是这样处理的。

**推荐**：**C，并且对 ≤ 4 MiB 的文件退化为现状**。文件按固定 4 MiB 切块，每块是一个 CAS 对象 `cas/<owner_id>/sha256/<块哈希>`；≤ 4 MiB 的文件只有一块，块哈希就是文件哈希，键与今天完全相同。小文件打包（E）先不做，§5 列出了改做 E 的触发条件。D 不做。

---

## 1. 共同事实

### 1.1 S3（AWS）

| 事实 | 原文 | 来源 |
|---|---|---|
| 单次 PUT 上限 | "With a single `PUT` operation, you can upload a single object up to 5 GB in size." | [upload-objects](https://docs.aws.amazon.com/AmazonS3/latest/userguide/upload-objects.html) |
| multipart 限制 | "Maximum number of parts per upload \| 10,000"；"Part size \| 5 MiB to 5 GiB. There is no minimum size limit on the last part" | [qfacts](https://docs.aws.amazon.com/AmazonS3/latest/userguide/qfacts.html) |
| 校验和类型与算法 | full object 只支持 CRC64NVME、CRC32、CRC32C；composite 支持 CRC32、CRC32C、SHA-1、SHA-256；"SHA-256 (SHA256) \| No \| Yes" | [checking-object-integrity-upload](https://docs.aws.amazon.com/AmazonS3/latest/userguide/checking-object-integrity-upload.html) |
| composite 的定义 | "A composite checksum is calculated based on the individual checksums of each part … aggregates the part-level checksums (from the first part to the last)" | 同上 |
| 不先声明算法就不校验 | "if the algorithm is not specified in `CreateMultipartUpload`, any checksum header provided in `CompleteMultipartUpload` is currently accepted but not validated or stored" | 同上 |
| 分段校验和怎么取回 | "For completed uploads, you can get an individual part's checksum by using the `GetObject` or `HeadObject` operations and specifying a part number or byte range that aligns with a single part." | 同上 |
| AWS 自己的 SHA-256 示例 | 整文件 SHA-256 存进 tag（"you could add it as a tag after completion"），下载时按 `partNumber` 逐段校验，再校验 "checksum of checksums" | 同上，Java 示例 |
| 复制会改校验和 | "If the object was originally uploaded using a multipart upload, the checksum value changes even though the data doesn't." | 同上 |
| `GetObject` `partNumber` | "Effectively performs a 'ranged' GET request for the part specified." | [API 模型](https://github.com/aws/api-models-aws/blob/main/models/s3/service/2006-03-01/s3-2006-03-01.json) `GetObjectRequest.PartNumber` |
| 一个 GET 只能取一个范围 | "Amazon S3 doesn't support retrieving multiple ranges of data per GET request." | 同上 `GetObjectRequest.Range` |
| `GetObjectAttributes` | "combines the functionality of HeadObject and ListParts"；`ObjectPart.ChecksumSHA256` 是 "The Base64 encoded, 256-bit SHA256 checksum of the part" | 同上 |
| 每个前缀的请求速率 | "at least 3,500 PUT/COPY/POST/DELETE or 5,500 GET/HEAD requests per second per partitioned Amazon S3 prefix. There are no limits to the number of prefixes"；扩容 "happens gradually"，期间 "you may see some 503 (Slow Down) errors" | [optimizing-performance](https://docs.aws.amazon.com/AmazonS3/latest/userguide/optimizing-performance.html) |
| 小请求延迟 | "smaller requests (for example, less than 512 KB), where median latencies are often in the tens of milliseconds range"；小对象延迟 "roughly 100–200 milliseconds"（指延迟敏感型应用能达到的一致延迟） | [design patterns](https://docs.aws.amazon.com/AmazonS3/latest/userguide/optimizing-performance-design-patterns.html)、[optimizing-performance](https://docs.aws.amazon.com/AmazonS3/latest/userguide/optimizing-performance.html) |
| 范围读按分段对齐 | "For objects that were uploaded using multipart upload, we recommend downloading them using the same part sizes or aligning requests to the original part boundaries for best performance." | [design patterns](https://docs.aws.amazon.com/AmazonS3/latest/userguide/optimizing-performance-design-patterns.html) |
| 请求价格 | S3 Standard，us-east-1：PUT/COPY/POST/LIST 每 1,000 次 $0.005，GET/SELECT 每 1,000 次 $0.0004，DELETE 免费。**未确认**：官方价格页打不开，数字来自第三方转述 | <https://aws.amazon.com/s3/pricing/> |

### 1.2 三个自托管对象存储

ADR-0021 已经实测：三家都校验预签名 PUT 的 `x-amz-checksum-sha256`，并在 checksum mode 的 `HeadObject` 里返回。下表只补本题需要的能力。

| 能力 | RustFS `master@4a1d542` | Garage `v2.4.1` | SeaweedFS `master@a576398` |
|---|---|---|---|
| `Range` GET | 支持 | 支持（`ACCEPT_RANGES: bytes`，`handle_get_range`） | 支持 |
| `GetObject`/`HeadObject` 带 `partNumber` | 支持 | 支持（[get.rs](https://github.com/deuxfleurs-org/garage/blob/v2.4.1/src/api/s3/get.rs#L358-L389)） | 支持，"turn the request into a ranged read of that part"（[s3api_object_handlers.go](https://github.com/seaweedfs/seaweedfs/blob/a5763986628fa629cb87b58f1b885426cf664d88/weed/s3api/s3api_object_handlers.go#L1121-L1129)） |
| UploadPart 校验 `x-amz-checksum-sha256` | 支持 | 支持，Complete 时还比对 "Invalid checksum for part"（[multipart.rs](https://github.com/deuxfleurs-org/garage/blob/v2.4.1/src/api/s3/multipart.rs#L360-L375)） | 支持，Create 时记下算法和 COMPOSITE/FULL_OBJECT（[filer_multipart.go](https://github.com/seaweedfs/seaweedfs/blob/a5763986628fa629cb87b58f1b885426cf664d88/weed/s3api/filer_multipart.go#L76-L131)） |
| SHA-256 composite 对象校验和 | 支持 | 支持，`CompositeSha256` 对各段摘要再做 SHA-256（[multipart.rs](https://github.com/deuxfleurs-org/garage/blob/v2.4.1/src/api/s3/multipart.rs#L755-L790)） | 支持，值形如 "base64-N (COMPOSITE)"（[filer_multipart.go](https://github.com/seaweedfs/seaweedfs/blob/a5763986628fa629cb87b58f1b885426cf664d88/weed/s3api/filer_multipart.go#L239-L241)） |
| 按 `partNumber` 读时返回该段校验和 | 支持，`decrypt_checksums(part_number…)`；带 `Range` 时不返回（`rs.is_none()`）（[get.rs](https://github.com/rustfs/rustfs/blob/4a1d542f2df910c3423784f4a4f611eb880d982e/rustfs/src/app/object/get.rs#L3506-L3525)） | **不支持**："TODO: for multipart uploads, checksums of each part should be stored so that we can return the corresponding checksum here"，`enabled: false`；`Range` 同样关掉（[get.rs](https://github.com/deuxfleurs-org/garage/blob/v2.4.1/src/api/s3/get.rs#L362-L389)） | **不支持**：只在 "The response covers the full object (no Range header, no partNumber)" 时返回（[s3api_object_handlers.go](https://github.com/seaweedfs/seaweedfs/blob/a5763986628fa629cb87b58f1b885426cf664d88/weed/s3api/s3api_object_handlers.go#L2512-L2517)） |
| `GetObjectAttributes` | 支持，`ObjectParts` 带每段校验和（[get.rs](https://github.com/rustfs/rustfs/blob/4a1d542f2df910c3423784f4a4f611eb880d982e/rustfs/src/app/object/get.rs#L4864-L4910)） | **未实现**：路由表里没有，[兼容性文档](https://garagehq.deuxfleurs.fr/documentation/reference-manual/s3-compatibility/)也没列 | 实现了，但 `ObjectParts` 只有 `PartNumber` 和 `Size`，**没有每段校验和**（[s3api_object_handlers_attributes.go](https://github.com/seaweedfs/seaweedfs/blob/a5763986628fa629cb87b58f1b885426cf664d88/weed/s3api/s3api_object_handlers_attributes.go#L311-L365)） |

结论：如果靠「按 `partNumber` 读时由存储返回该段校验和」或者「`GetObjectAttributes` 列出每段校验和」，三家里只有 RustFS 能用。三家都能用的只有两样：单次 PUT 时校验 `x-amz-checksum-sha256`（现在已经在用），以及 composite 的对象级校验和（经 `HeadObject` 取回；SeaweedFS 和 RustFS 是否按 AWS 的同一公式计算、带不带 `-N` 后缀，**未确认，要实测**）。

### 1.3 备份工具与 JuiceFS 的参数

| 系统 | 分块 | 打包 | 包内定位与校验 | 回收 | 来源 |
|---|---|---|---|---|---|
| restic | Rabin CDC。"Files smaller than 512 KiB are not split, Blobs are of 512 KiB to 8 MiB in size. The implementation aims for 1 MiB Blob size on average." | "Restic currently defaults to a 16 MiB pack size"；调大是为了 "reduce the number of files in the repository" | pack 尾部有 header；index 记录 "plaintext hashes, types, offsets and lengths"；"Placing the header at the end of a file allows writing the blobs in a continuous stream" | prune："A partially used file is either kept or marked for repacking"；"for repacking, restic must download the file from the repository storage and re-upload the needed data"；`--max-unused` 默认 5% | [design.rst](https://github.com/restic/restic/blob/master/doc/design.rst)、[047_tuning_parameters.rst](https://github.com/restic/restic/blob/master/doc/047_tuning_parameters.rst)、[060_forget.rst](https://github.com/restic/restic/blob/master/doc/060_forget.rst) |
| kopia | 默认 `DYNAMIC-4M-BUZHASH`；另有 `FIXED-4M` | "multiple smaller blocks are combined into larger Packs of 20-40MB each"；源码默认 `MaxPackSize` 为 `20<<20` | "CABS maintains an index that maps a block ID to the blob file name, offset within the file and length"，并且 "a local index is also stored at the end of each pack" | Full Maintenance "is also responsible for compaction of data pack blobs (p) after contents stored in them have been deleted"，每 24 小时一次；只允许一个 Maintenance Owner | [architecture](https://kopia.io/docs/advanced/architecture/)、[maintenance](https://kopia.io/docs/advanced/maintenance/)、[splitter.go](https://github.com/kopia/kopia/blob/master/repo/splitter/splitter.go)、[initialize.go](https://github.com/kopia/kopia/blob/master/repo/initialize.go) |
| borg（2.x 开发分支） | 默认 fastcdc，`19,23,21`："maximum chunk size = 2^23 B = 8 MiB"，"target chunk size ~= 2^21 B = 2 MiB" | "many of them are batched into a **pack** file and that pack file is stored as a single store object" | chunks index：chunk ID → `pack_id`、`obj_offset` | `borg compact`："a single object can not be removed from a pack file, only a whole pack file can be deleted or rewritten" | [data-structures.rst](https://github.com/borgbackup/borg/blob/master/docs/internals/data-structures.rst) |
| casync | buzhash CDC，默认平均 64 KiB（最小 16K、最大 256K） | **不打包**："store these chunks in individual, compressed files in some directory, each file named after a strong hash value"；"remove file boundaries before chunking"，所以小文件和相邻文件拼在一起切 | chunk index 是 "hash values plus their respective chunk sizes in a simple linear array" | — | [README](https://github.com/systemd/casync/blob/main/README.md)、[cachunker.h](https://github.com/systemd/casync/blob/main/src/cachunker.h) |
| JuiceFS | 64 MiB Chunk → Slice → 最大 4 MiB 的 Block，每个 Block 一个对象 | **不合并**："Irrespective of the file size, JuiceFS avoids storage merging to prevent read amplification" | 对象键 `chunks/<hash>/<sliceId>_<index>_<size>` | 按 slice 引用延迟删除 | [architecture](https://juicefs.com/docs/community/architecture)、[internals](https://juicefs.com/docs/community/internals) |

这些工具都是为备份或分发设计的，读的模式是顺序还原；只有 JuiceFS 和 casync（有 `casync mount`）是为随机读做的。两个做随机读的系统，一个不合并小对象，一个不打包。

---

## 2. 逐个布局

### A. 现状：一文件一对象，按需读用 Range GET

- **读粒度**：任意；FUSE 每次取 4 MiB 范围，小文件一次取整个对象。读放大就是 4 MiB 对齐带来的那部分。
- **部分读的校验**：做不到。范围读拿不到可比对的摘要（§1.1、§1.2），整对象读完才能比对文件 SHA-256。要么第一次访问时把整文件下完（等于没有按需读），要么在未校验的数据上运行。
- **大文件**：超过 5 GiB 的文件 PUT 不进去（§0.1）。换成 multipart 以后，对象存储只能校验 CRC 类的 full-object 校验和或 SHA-256 composite，整文件 SHA-256 变成客户端声明、无人校验的值。
- **其余**：去重粒度是整文件；GC、按 owner 去重、迁移都是现状。

### B. A + manifest 里记分块哈希，对象不变

分两种：

- **B1：分块哈希由客户端声明**（对象仍用单次 PUT）。Node 读到一个 4 MiB 单元，可以先拿 manifest 里的块哈希比对，错了就报错。但块哈希是客户端声明的，**Control Plane 无法校验**，违反「不信任未经校验的客户端摘要」。影响只限于这个 owner 自己的版本（键在 owner 命名空间内，ADR-0021），但版本是否「可读」要等到读的时候才知道。对超过 5 GiB 的文件没有帮助。
- **B2：块 = multipart 分段，表由 composite 间接校验。** 大文件用固定段长 P 做 multipart，每个 UploadPart 都带 `x-amz-checksum-sha256`，三家存储都会校验（§1.2）。publish 时用一次 checksum mode 的 `HeadObject` 取回 composite 值，Control Plane 用 manifest 里的分段摘要表自己算 SHA-256(d₁‖…‖dₙ)，两者一致，就说明表里每一项都是对象存储校验过的段摘要（依赖 SHA-256 的抗碰撞性）。不需要 `GetObjectAttributes`，也不需要存储在读分段时返回校验和，所以三家都能用（composite 公式是否与 AWS 一致，**要实测**）。代价：
  - 段长 ≥ 5 MiB，而且要 ≤ 10,000 段：100 GiB 至少要 10.24 MiB 一段，实际取 16 MiB（最大 156 GiB），或 64 MiB（最大 625 GiB）。**FUSE 的校验单位就是段**，也就是 16 MiB，而不是 4 MiB；读放大最多是 C 的 4 倍。
  - 文件地址必须改成「段长 + 分段摘要表」的哈希。整文件 SHA-256 没人校验，不能拿来做去重键。
  - Control Plane 要新增 CreateMultipartUpload、预签名 UploadPart、CompleteMultipartUpload 和未完成上传的清理；现在只有单次 PUT。
  - 去重粒度仍是整文件；对象数少，100 GiB 只有 1 个对象。

### C. 固定 4 MiB 分块，每块一个内容寻址对象

- 文件按偏移切成 4 MiB 的块，最后一块可以更短；每块是 `cas/<owner_id>/sha256/<块哈希>`，用现有的预签名 PUT 带 `x-amz-checksum-sha256` 上传，publish 时逐块 `HeadObject` 校验。这条链路已在三家存储上实测过（ADR-0021）。
- **≤ 4 MiB 的文件只有一块，块哈希 = 文件 SHA-256，键与今天完全一样**。manifest 只需要给 > 4 MiB 的文件加一个 `blocks: [sha256…]` 列表。
- **读粒度**：恰好是 FUSE 的 4 MiB 单元，一次 GET 取一个对象，取回后立即按块哈希校验。读放大与 A 相同，但每个单元都可校验。
- **大文件**：一个块远小于 5 GB，不需要 multipart；一个文件有多少块就是多少次单次 PUT。
- **文件地址**：> 4 MiB 的文件，manifest 用块列表表示；Control Plane 从已校验的块摘要算出一个「块列表哈希」作为文件身份。客户端算的整文件 SHA-256 可以留作信息字段，但标为未校验，不作去重键。
- **去重**：粒度是块，同一 owner 的不同文件和不同版本之间都能共享。追加写只影响最后一个不满的块和新块。在中间插入会让后面的块全部错位（固定分块的通病，CDC 就是为此而来）。
- **GC**：不变。块就是 CAS 对象，`resource_version_objects` 和 `cas_objects` 多出一些行，`NOT EXISTS` 清扫照旧，不存在「半死的包」。
- **代价**：每 GiB 256 个对象；100 GiB 文件就是 25,600 个对象，以及同样数量的 PUT、HEAD 和预签名。manifest 中这个文件的块列表约 1.8 MB（25,600 × 约 70 字节 JSON）。

### D. 内容定义分块 + 打包（restic / kopia / borg）

- **读粒度**：chunk 大小不定（restic 0.5–8 MiB，平均 1 MiB；kopia 平均约 4 MiB；borg 平均约 2 MiB），与 FUSE 的 4 MiB 单元不对齐。一个 4 MiB 单元可能跨几个 chunk，好在同一文件的 chunk 通常在包里连续存放，一次 Range GET 就能取到（取决于写入顺序）。
- **部分读的校验**：可以做到。每个 chunk 有自己的哈希，取回后自校验。
- **上传与信任**：包是单次 PUT，带 `x-amz-checksum-sha256`，包的摘要可以由对象存储校验；但**「chunk H 位于包 P 的偏移 O」这件事是客户端声明的**，Control Plane 不读内容就无法验证。更糟的是去重依据的正是这张索引：「这个 owner 已经有 H」只是一个声明，一个写坏的索引会污染这个 owner 之后所有复用 H 的版本。restic、kopia、borg 的客户端本身就是信任根，没有这个问题；orrery 的 Control Plane 不是。
- **另需一层位置索引**：manifest 是按内容寻址的，不能写入包位置，否则 repack 会改变版本地址。这就是 restic 的 tree / index 分离、kopia 的 CABS index，等于在 PG 或 S3 上再造一个按 owner 划分的 chunk 索引。
- **GC**：包里只要有一部分已死，就要下载再上传重写（restic prune、kopia full maintenance、borg compact）。orrery 的 Control Plane 不读内容，repack 只能交给 Node 上的系统任务。
- **去重**：最好，插入和删除也能识别。但数据集和模型权重多数是不可变文件或整体重写的产物，这种优势能用上多少，**未确认**（属于判断，没有一手数据）。
- **迁移**：全部重写。

### E. 只打包小文件，大文件按 A/B/C

- 低于阈值的文件按路径顺序拼进包（比如 16–64 MiB），manifest 或位置索引记录 `(pack, offset, length, sha256)`；大文件按 C 处理。
- **读粒度**：一个小文件可以只取它自己的范围（放大为零），也可以取所在包的一个 4 MiB 单元，顺带预取同目录的相邻文件。顺序扫描整个数据集（训练的一个 epoch）的 GET 次数降一到两个数量级。
- **部分读的校验**：每个文件都有自己的 SHA-256，取回后自校验，现在的 manifest 已经有这个字段。
- **信任**：与 D 相同。包的摘要可由对象存储校验，偏移由客户端声明。可以这样补：读时校验，不一致就按 ADR-0012「暂时读不到 → unavailable，修复而不删」处理；去重只以包为单位，或者只复用已被 Node 读过并校验的位置。不论哪种，「publish 时一切都已校验」都会降级为「读时校验」，需要在 ADR 里写明。
- **GC**：位置不能进入按内容寻址的 manifest（否则 repack 会改地址），所以要另建位置索引，或者干脆不 repack，接受死空间（restic 默认也容忍 5% 未用）。
- **按 owner 去重**：包放在 `cas/<owner_id>/…` 下，Control Plane 只给同一 owner 签 URL，ADR-0021 的规则不受影响。
- **迁移**：旧版本保持单对象；新上传开始打包，于是新旧之间的小文件去重要断一次（全部重传一次）。

### 关于「不改键，只用 S3 分段校验和」

做得到的只有 B2：用 composite 校验分段摘要表，读的时候由 Node 自己比对。依赖存储在读分段时返回该段校验和的方案（`GetObject partNumber` + checksum mode）和依赖 `GetObjectAttributes ObjectParts` 的方案，在 Garage v2.4.1 上都不可用，SeaweedFS 也不返回每段校验和（§1.2）。而且这条路改的只是「大文件怎么上传」，键其实也变了：一样要换成 multipart，地址也要从整文件 SHA-256 换成分段表的哈希。对 ≤ 5 GiB 的现有对象，要想有可校验的范围，只能重传成 multipart。

---

## 3. 对比表

| 标准 | A 现状 | B1 客户端声明的块哈希 | B2 multipart + composite | C 固定 4 MiB 块对象 | D CDC + 打包 | E 小文件打包 + 大文件 C |
|---|---|---|---|---|---|---|
| 读粒度 / 放大 | 4 MiB 范围；小文件取整个 | 同 A | 校验单位 = 段（≥ 16 MiB 才能放下 100 GiB） | 4 MiB，恰好对齐 | chunk 0.5–8 MiB，不对齐 | 小文件取自身范围或包内 4 MiB；大文件同 C |
| 部分读能否校验 | 否 | 能，但所用的哈希没人校验过 | 能，分段表经 composite 校验 | 能，块经存储校验 | 能（chunk 自校验） | 能（文件自校验） |
| > 5 GiB 文件 | 不支持；换 multipart 后整文件 SHA-256 无法校验 | 不支持 | 支持 | 支持 | 支持 | 支持 |
| Control Plane 不读内容、也不信任客户端摘要 | 保持（≤ 5 GiB 时） | **破坏**（块哈希） | 保持（composite 一次 HEAD 校验整张表） | 保持（每块一次 HEAD） | **破坏**（索引偏移、去重依据） | 包保持，**偏移破坏**，降级为读时校验 |
| 去重：改一个文件 | 只重传这个文件 | 同 A | 同 A | 只重传变了的块 | 只重传变了的 chunk | 小文件进新包；大文件同 C |
| 去重：文件追加 | 整文件重传 | 整文件重传 | 整文件重传 | 只传最后一块和新块 | 只传新 chunk | 同 C |
| GC | 现状 | 现状 | 现状 + 清理未完成的 multipart | 现状，行数变多 | 位置索引 + repack（要在 Node 上跑） | 位置索引 + repack，或接受死空间 |
| 迁移 | — | manifest 加字段 | 大文件改上传路径，地址变 | ≤ 4 MiB 零迁移；> 4 MiB 的旧对象保持整对象读 | 全部重写 | 小文件去重断一次 |
| 按 owner 去重 | 保持 | 保持 | 保持 | 保持 | 保持（包在 owner 前缀下），但索引可被自己写坏 | 同 D |
| 小对象请求量（工作负载 i） | 10⁶ 级 | 10⁶ | 10⁶ | 10⁶ | ~1.8×10⁴ | ~1.8×10⁴ |
| 新增的 S3 能力依赖 | 无 | 无 | multipart + SHA-256 composite（三家都有，公式待测） | 无 | 无 | 无 |

---

## 4. 两种工作负载的对象数与请求数

假设：(i) 300 GB = 3×10¹¹ 字节，1,000,000 个文件，平均 300 KB，全部 ≤ 4 MiB（只给了平均值，分布**未确认**；如果有 > 4 MiB 的长尾，C 和 E 的大文件部分按 (ii) 的算法另算）。(ii) 一个 100 GiB 的文件，10 GiB 除以 10。每列都按冷启动、无去重计算。「预签名」指 Control Plane 签发的 URL 数；「HEAD」指 publish 时的校验请求。价格按 §1.1 里**未确认**的 $0.005/千次写、$0.0004/千次读（HEAD 是否按 GET 档计费，**未确认**）。

### (i) 1,000,000 × 300 KB

| 布局 | 对象数 | 上传 PUT | 预签名 | publish HEAD | 全量冷读 GET | 单个文件冷读 |
|---|---|---|---|---|---|---|
| A / B1 / B2 / C | 1,000,000 | 1,000,000（≈ $5） | 1,000,000 | 1,000,000（≈ $0.4） | 1,000,000（≈ $0.4） | 1 次 GET，300 KB，可校验 |
| D（restic：< 512 KiB 不切；16 MiB 包） | ≈ 17,900 包 + 索引 | ≈ 17,900（≈ $0.09） | ≈ 17,900 | ≈ 17,900 | 按 4 MiB 单元 ≈ 71,500；整包 ≈ 17,900 | 1 次 Range GET（或取 4 MiB 单元） |
| E（16 MiB 包） | ≈ 17,900 | ≈ 17,900 | ≈ 17,900 | ≈ 17,900 | ≈ 71,500（4 MiB 单元） | 同 D |
| E（64 MiB 包） | ≈ 4,500 | ≈ 4,500 | ≈ 4,500 | ≈ 4,500 | ≈ 71,500 | 同 D |

算式：3×10¹¹ / 16 MiB（16,777,216）≈ 17,881；3×10¹¹ / 64 MiB ≈ 4,470；3×10¹¹ / 4 MiB ≈ 71,526。

量级判断：AWS 上 100 万次 PUT 约 $5，单团队规模下钱不是问题。问题在时间：按 AWS 说的「小请求中位延迟几十毫秒」，并发 64、每次 30 ms 估算，100 万次请求约 470 秒；publish 要先 PUT 再 HEAD，至少两轮，还没算 100 万个预签名经过 Control Plane 的开销。冷读一个 epoch 也是 100 万次 GET，同样的估算约 470 秒；同样条件下 300 GB 在 1 GB/s 带宽下要 300 秒。两者同一量级，所以打包对冷读的收益取决于实际延迟和并发，**要实测**。单个前缀 3,500 PUT/s 的下限决定了 100 万次 PUT 至少约 286 秒（键是哈希，前缀分散以后能扩容，但扩容 "gradual"，期间有 503）。

### (ii) 一个 100 GiB 文件（10 GiB 按 1/10 计）

| 布局 | 对象数 | 写请求 | 预签名 | publish 校验 | 全量冷读 GET（4 MiB 单元） | 读校验单位 |
|---|---|---|---|---|---|---|
| A | 放不进：单次 PUT 上限 5 GB，`MaxFileSize` 5 GiB | — | — | — | — | — |
| A 改 multipart（16 MiB 段） | 1 | 6,402（Create + 6,400 UploadPart + Complete） | 6,400 | 1 HEAD，**只能校验 composite，整文件 SHA-256 无法校验** | 25,600 | 无 |
| B2（16 MiB 段） | 1 | 6,402 | 6,400 | 1 HEAD（composite 校验整张分段表） | 6,400 次 16 MiB，或 25,600 次 4 MiB（每段读完才能校验） | 16 MiB |
| C（4 MiB 块） | 25,600 | 25,600 | 25,600 | 25,600 HEAD | 25,600 | 4 MiB |
| D（restic，平均 1 MiB chunk，16 MiB 包） | ≈ 6,400 包 | ≈ 6,400 | ≈ 6,400 | ≈ 6,400 HEAD | ≈ 25,600 | chunk |
| D（kopia，平均 4 MiB，20 MiB 包） | ≈ 5,120 包 | ≈ 5,120 | ≈ 5,120 | ≈ 5,120 | ≈ 25,600 | chunk |
| E | 同 C | 同 C | 同 C | 同 C | 同 C | 4 MiB |

算式：100 GiB / 4 MiB = 25,600；100 GiB / 16 MiB = 6,400（≤ 10,000）；8 MiB 段会得到 12,800 段，超过 10,000，不行。

C 的 25,600 次单 PUT 与 B2 的 6,400 次 UploadPart 处在同一量级，单个 100 GiB 文件上的差距不大。真正的差别在：C 不需要 multipart，校验单位是 4 MiB，并且能做块级去重；B2 对象少，publish 校验只要一次 HEAD。

---

## 5. 推荐

**采用 C：固定 4 MiB 分块，每块一个 CAS 对象；≤ 4 MiB 的文件就是今天的一文件一对象。** E（小文件打包）留作下一步，等触发条件出现再做。D 不做，A 和 B1 不能满足需求。

理由，按分量排：

1. **只有 C 和 B2 能在「Control Plane 不读内容、也不信任客户端摘要」的前提下承载 > 5 GiB 的文件。** S3 单次 PUT 最多 5 GB；multipart 下 SHA-256 只能是 composite（§1.1）。C 把问题化解成「每块一次单次 PUT」，用的就是 ADR-0021 已经在三家存储上实测过的那条链路，不新增任何 S3 能力。B2 要新做 multipart 签发与清理，三家的 composite 公式还没实测，校验单位也被 5 MiB 的段长下限和 10,000 段上限推到 16 MiB 以上，和 FUSE 的 4 MiB 缓存单元对不上。
2. **按需读要可校验，校验单位就必须是一个被存储校验过的对象，或者一张被 composite 证明过的表。** 范围读在 Garage v2.4.1 上不返回任何校验和，SeaweedFS 也不返回；`GetObjectAttributes` 里的每段校验和只有 RustFS 有（§1.2）。C 的块就是 FUSE 的单元，取回即可校验，这与 JuiceFS 用 4 MiB Block、并且 "avoids storage merging to prevent read amplification" 的取舍一致。
3. **C 对现状是严格的超集，迁移几乎为零。** ≤ 4 MiB 的文件块哈希就是文件哈希，键、`cas_objects` 行、按 owner 去重、`NOT EXISTS` GC 都不变；只有 > 4 MiB 的文件在 manifest 里多一个块列表。现有 > 4 MiB 的整对象可以继续按旧方式整对象下载并校验，下次 publish 再自然切块。
4. **E 的收益是真实的，但代价落在 orrery 最在意的地方。** 打包能把工作负载 (i) 的请求数降 50–200 倍，可是包内偏移只能由客户端声明；位置不能进按内容寻址的 manifest，于是要建 restic 式的位置索引；repack 要下载再上传，而 Control Plane 不读内容，只能放到 Node 上做（§2 E）。在没有测到瓶颈之前，这些成本不值得付。
5. **D 的长处（插入和删除也能去重）与 Resource 的用法错位。** Resource 是不可变版本，数据集和权重文件多半是新增或整体重写。D 却要付出和 E 一样的信任与 GC 代价，还要全量迁移。casync 证明 CDC 并不一定要打包（chunk 可以各自一个对象），如果将来真的需要 CDC，可以只对大文件做「CDC + 每 chunk 一个对象」，信任模型与 C 相同。

### 落地要点（不在本文范围，只列出与布局相关的部分）

- manifest：`file` 条目在 `size > 4 MiB` 时带 `blocks`（有序 SHA-256 列表），最后一块可以更短；块长作为常量写进格式，改块长等于改格式。≤ 4 MiB 的文件保持现在的 `sha256`。
- 上传：客户端或 Node 对每块计算 SHA-256，Control Plane 只给本 owner 缺失的块签 PUT；publish 时逐块 `HeadObject`（checksum mode），与现在逐文件 HEAD 是同一段代码。
- 文件身份：> 4 MiB 的文件由块列表哈希标识，由 Control Plane 从已校验的块摘要计算；客户端给的整文件 SHA-256 不作去重键。
- `MaxFileSize` 的 5 GiB 限制可以去掉或调大；manifest 的大小上限要把块列表算进去（100 GiB 约 1.8 MB）。
- 读：FUSE 缺页时按块取对象，校验块哈希后放入 Node 的内容寻址缓存；缓存键就是块哈希，跨版本、跨 Resource 共享。

### 什么情况下改变推荐

| 观察到的情况 | 改成 |
|---|---|
| 百万小文件的数据集上，publish（PUT + HEAD + 预签名）或冷读一个 epoch 实测主要耗在逐对象请求上，比带宽下限慢一个数量级以上 | 加 E：小文件按路径顺序打包；位置索引按 owner 存在 PG；接受读时校验，并为此写 ADR |
| 部署的对象存储对对象数敏感（元数据表膨胀、LIST/GC 太慢），或 HEAD 量成为 Control Plane 的瓶颈 | 大文件改用 B2（16 MiB 段，composite 一次 HEAD）；前提是三家的 SHA-256 composite 公式实测一致 |
| FUSE 缓存单元改到 ≥ 16 MiB | B2 的校验单位问题消失，B2 与 C 基本打平，可以按对象数来选 |
| 出现大量「在中间编辑的大文件」版本（例如原地修改的大型 JSONL 或 tar） | 只对大文件做 CDC，每个 chunk 一个对象（casync 式，不打包） |
| 需要跨 owner 去重 | 不改布局；ADR-0021 的理由依然成立 |

---

## 6. 未确认项

- S3 请求价格（官方价格页打不开）；HEAD 是否按 GET 档计费。
- SeaweedFS 和 RustFS 的 SHA-256 composite 是否与 AWS / Garage 同一公式（对各段原始摘要拼接后再做 SHA-256，是否带 `-N` 后缀），只有 B2 需要这一点。
- RustFS `master@4a1d542` 和 SeaweedFS `master@a576398` 的行为在已部署的 RustFS 1.0.2、SeaweedFS 4.48 上是否相同。
- 工作负载 (i) 的文件大小分布（只给了平均值）。
- D 对数据集和模型权重实际能多去重多少。
- 各对象存储在百万级小对象下的元数据开销和 LIST/GC 性能，没有找到一手的基准。
