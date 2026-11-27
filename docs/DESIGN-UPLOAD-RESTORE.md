> 状态: proposal（doc-first，待用户审阅） · 归属: upload-restore-design · 更新: 2026-11-27 · 里程碑: R29
> 关联: task-241（本轮）/ task-238（round 28 只读核实）· 插件版本 v0.5.3 → 方案目标 v0.6.0
> 写域: 本仓 `docs/DESIGN-UPLOAD-RESTORE.md`；不碰代码、不落实现、不改 backlog、不联网业务

# DESIGN：上传本地备份还原（doc-first 方案）

## 0. 结论先行

BT 官方「备份还原」原文含**上传本地备份还原**；我方 `kore-plugin-backup` v0.5.3 的 15 个方法中无 `upload`（`grep upload` 零命中），只能把**已在本地备份目录**的包恢复到目标目录。这是竞品一手确证的**真实功能层差距**，不是 G-11「明确不做」，也不是此前被证伪的「S3 后端已备、前端未接」文档漂移（`kore/docs/FEATURE-GAP-BACKLOG.md` C3 条 round 28 订正）。按用户「门槛自主裁决」决定补齐；因涉及**文件上传 + 恢复执行**，安全敏感且跨多文件，先出本方案供审阅，不直接写代码。

本方案的核心裁决：

1. **增量边界 = 接收上传文件落入备份目录，绝不自动恢复。** 上传只做「导入一份外部 tar/tar.gz 包」；真正恢复仍走既有 `backup.restore`，复用它的白名单解析、空目录检查、两级确认与安全解包门，不重建第二套恢复逻辑。
2. **入口 = 一个新方法 `backup.upload`（`system:admin`）+ 分块 base64 入站**，不新增内核 HTTP 端点、不改内核代码、不引入任何出站连接。
3. **不触 G-11、不触 ADR-0023 零侵入清单**，因此**不新增 ADR**；但把「跨面板格式解析 / 云存储」明确排除在本期之外（见 §3）。

---

## 1. 现状与增量边界（读码实证）

### 1.1 现状事实

| 面 | 事实（代码证据） |
| --- | --- |
| 方法面 | `info.json` 的 `methods` / `method_capabilities` 共 15 个：`create / list / restore / delete / verify / retention / stats / download / jobs / schedule.set / schedule.get / config.get / config.set / presets / sources.detect`；写方法 `system:admin`、读方法 `system:read`（`method_capabilities_test.go:31-42`）。`Call` 的 verb 分派见 `internal/backupop/call.go:26-69`。 |
| 下载分块 | `backup.download` 用 `offset/limit` 分块，单块 ≤4 MiB（`DefaultDownloadChunk` / `MaxDownloadChunk`），base64 后 ≤5.34 MiB，低于协议单帧 8 MiB（`download.go:15-23,73-83,140-148`）；结果含 `path` 供宿主文件域取走。入站侧对称：`/api/v1/invoke` 请求体上限同样是 8 MiB（`kore internal/core/httpapi/handlers_invoke.go:18-19,89`）。 |
| 恢复安全门 | `callRestore` 先 `ValidateID` → `ResolveTargetDir`（绝对路径、白名单、祖先符号链接解析后二次校验）→ `NormalizeRestoreFilter`（`restore.go:29-49`、`validate.go:41-60,139-182,184-208`）；执行面 `Restore` 用「同文件系统临时目录完整解包 → rename」的全有或全无提交，目标不存在或存在且空，否则 `E_CONFLICT`（`restore.go:62-128,130-150`）；解包逐头 `validateRestoreHeader` + `withinPath(staging,dest)` 双重防越界（`restore.go:167-204,292-353`）。 |
| 前端确认 | `web/backups.js` 已是全方法调用（1398 行）；恢复/删除/清理走**页面内二次确认面板**（`panel-head` 标「二次确认 · 页面内」），不用 `window.confirm`（`backups.js:8,968-1003`）：点「恢复」→ 面板显示目标目录约束 → 填 `/tmp/...` 类绝对路径 → 点「确认恢复」才 `invoke('backup.restore', ...)`（`:1004-1021`）。这就是任务卡所说「两级确认」的落点（页面一级 + 插件白名单/空目录一级）。 |
| G-28 基线 | 备份页双端截图与自评已落 `kore/.agents/verify/backup-redis-ui.md`（桌面 1440×1935 / 手机 390×2354），其中明确「下载按钮为行内动作、0 份备份故不可见，下载链路未实机验证属证据缺口」——本次不借它宣称下载已验证。 |

### 1.2 增量边界裁决：只导入，不恢复

**选择 B（接收上传文件落入备份目录）**，理由：

- **职责分离**：上传是「把外部产物纳入本地备份视图」，恢复是「把备份内容写到目标目录」的破坏性动作；合并会让一次 HTTP 调用同时具备「接收不可信字节」与「覆盖目标路径」两种能力，放大误操作与权限提升面。
- **复用而非重建**：`backup.restore` 已具备空目录保护、白名单、Zip-Slip 防护、全有或全无提交；上传后由用户在既有列表点「恢复」即可走完全相同的门（§4.2）。新增「上传即恢复」等于复制一套恢复路径，违反 G-06 与「不重建恢复逻辑」的任务约束。
- **可验证性**：导入结果可被既有 `backup.list` / `backup.verify` / `backup.download` 立刻检查，分期判据清晰（§6）。

**明确不做**：`upload` 方法体内不调用 `exec.Restore`，不接受 `target_dir` 参数，不写任何备份目录以外的业务路径。

---

## 2. 方案设计：`backup.upload`（新增第 16 个方法）

### 2.1 协议形状（显式字段，符合 G-24）

新方法限定名 `backup.upload`，能力 `system:admin`（写类），四阶段共用一个方法，避免方法数量膨胀带来的 `method_capabilities` / 索引 / presets 涟漪：

```text
phase=begin   params { phase, upload_id, filename, size_bytes, sha256? }
              → { upload_id, chunk_bytes, expires_at, hint }
phase=chunk   params { phase, upload_id, offset, data_base64 }
              → { upload_id, offset, next_offset, received_bytes }
phase=commit  params { phase, upload_id }
              → { id, filename, size_bytes, sha256, entries, compression, managed:false, duration_ms, hint }
phase=abort   params { phase, upload_id }
              → { upload_id, aborted:true }
```

- `upload_id` 复用 `ValidateID`：仅字母/数字/`.`/`_`/`-`、≤160 字符、不得以 `.` 开头，天然拒绝 `/`、`..` 与路径分隔符。
- `filename` 只取 basename，必须匹配既有 `archiveExt`（`.tar.gz` / `.tar`），禁止 NUL、反斜杠与路径分隔符；最终 id 由插件按 `<name>-<UTC时间戳Z>` 生成（`ValidateName` 约束 name），**不接受调用方指定最终 id**；冲突走 `E_CONFLICT`（`archive.go:56-61` 的 `O_EXCL` 口径）。
- `size_bytes` 是调用方声明的总长，`data_base64` 严格 base64 解码；`chunk_bytes` 固定 4 MiB（与 `MaxDownloadChunk` 对齐），JSON 体远小于 8 MiB 上限。
- 所有字段显式声明（`Phase/UploadID/Filename/SizeBytes/SHA256/Offset/DataBase64` 均为具体类型），不使用 `any` / `map[string]any` / `object`。

### 2.2 落盘与生命周期（隔离暂存 → 校验 → 提交）

1. `begin`：在备份目录内建 `<dir>/.upload-<upload_id>/`（`0700`，`O_EXCL`），写 `.upload.json`（显式字段：声明长度、可选 sha256、已收字节、创建时间）。`assertInsideBackupDir` 校验暂存**根目录**（`download.go:152-178`），入口文件再用 `withinPath(staging, file)` 二次确认。
2. `chunk`：`os.OpenFile(O_WRONLY|O_CREATE, 0600)` + `WriteAt(offset)`，只接受 `offset == received_bytes`（顺序写），拒绝重叠/回退（`E_CONFLICT`）；更新 `.upload.json` 的 `received_bytes`（原子写，复用 `archive.go:427-464` 的 `atomicWrite`）。`serve` 循环是串行读帧→分发→写响应（`serve.go:14-41`），单实例无并发写竞态；跨进程同目录再加 `.lock` 文件 `O_EXCL` 兜底。
3. `commit`：先验 `received_bytes == size_bytes`、可选 sha256 匹配 → **只读安全扫描**（§3 的 S4/S5，复用 `validateRestoreHeader` + 条目/字节上限，不落盘任何一个条目）→ 通过后才 `os.Rename` 到 `<dir>/<generated-id><ext>`（`O_EXCL` 语义，目标已存在拒绝），写**仅** `.sha256` sidecar（对实际收到字节计算，原子写）。暂存目录无论成功失败都清理。
4. `abort` / 超时 / 失败：删除整个暂存目录。每次 `begin` 与插件启动时扫描 `.upload-*`，清理超过 30 分钟未提交的暂存（防止半包长期占用）。

**Managed 口径**：上传包**不写 `.meta.json`**。`backup.list` 的 `Managed = metaErr==nil && managedMeta(...)`（`metadata.go:63-74,80-88`），因此导入包显示已有 UI 的「外部包」标记，`backup.retention` 不会静默删除用户导入的包（`types.go:180-182`）；`backup.verify` 因有 `.sha256` 而获得 sha 校验（`verify.go:99-120`）。这是诚实的：我们只能为「接收字节的完整性」背书，不能为第三方产物的内部一致性背书。

---

## 3. 安全门清单（逐条可验收）

| # | 门 | 设计与证据 |
| --- | --- | --- |
| S1 | **鉴权** | `info.json.method_capabilities["backup.upload"]="system:admin"`；内核 `authorizeInvoke` 对写方法校验能力，未登录 401 / 不足 403，并统一写 `capability.denied` 审计（`invoke_capability.go:105-145`）。HTTP 层沿用 CSRF 双提交（`web/backups.js:67-72`）。单管理员模式下内核已诚实披露能力检查降级，多用户注入 resolver 后自动生效——本方案不新增鉴权旁路。 |
| S2 | **类型 / 大小上限（防 DoS）** | 仅 `.tar.gz`/`.tar`（复用 `archiveExt`）；声明总长 ≤ `MaxUploadBytes`（新常量，建议 512 MiB，硬顶 ≤ `DefaultMaxBytes` 2 GiB）；单块 ≤4 MiB 原始字节；条目数 ≤ `DefaultMaxEntries`（100 万）、单条目 ≤ `MaxEntryBytes`（2 GiB）、路径 ≤ `MaxEntryNameLen`（4096）、未压缩累计 ≤ `DefaultMaxBytes`；超限整包拒绝，不进备份目录（`types.go:11-22`、`restore.go:324-328`）。 |
| S3 | **路径遍历 / 符号链接** | `upload_id` 经 `ValidateID` 白名单字符集；暂存根用 `assertInsideBackupDir`（含 `EvalSymlinks` 二次校验，`download.go:152-178`）；`.upload-<id>` 以 `O_EXCL|0700` 创建，已存在（含符号链接）即拒绝；入口文件 `withinPath(staging, dest)`（`validate.go:230-240`）；最终路径同样过 `assertInsideBackupDir`。 |
| S4 | **解包防 Zip-Slip（成员必须落目标内）** | commit 的只读扫描与 restore 的解包共用 `validateRestoreHeader`：拒绝对路径、`..` 段、NUL/反斜杠、超长路径、字符/块设备/FIFO/硬链接等不支持类型（`restore.go:292-335`）；成员目标 `path.Clean(path.Join(...))` 不得逃出根，符号链接目标另经 `validateSymlinkTarget` 拒绝绝对或逃逸（`restore.go:337-353`）；实际写盘处仍有 `withinPath(staging, dest)` 兜底（`restore.go:196-199`）。 |
| S5 | **恶意归档** | gzip/tar 头无法解析 → `E_INVALID`；符号链接逃逸、设备文件、绝对路径、超长链接整包拒绝；setuid/setgid 位在 restore 落盘时被丢弃（`restore.go:265-267`）；扫描时统计未压缩字节与条目数，超上限立即中止（防解压炸弹）；空归档拒绝（`restore.go:105-110`）。 |
| S6 | **隔离暂存 → 校验通过才入备份目录** | 暂存目录隐藏（`.` 前缀），`backup.list` 跳过目录条目（`metadata.go:37-39`），`retention` 只认合法扩展名文件；只有 commit 成功才 rename 到备份目录并生成 `.sha256`；失败/超时清理，启动 sweeping 半包（§2.2）。 |
| S7 | **审计** | 复用既有出口：拒绝走 `capability.denied`（`auditDenied`，`middleware.go:274`），成功调用由 `invoke.*` 写审计（`needAdmin=true`）；插件侧只在方法结果里返回结构化错误，**不新增审计表、不新增端点、不绕开现有出口**。 |
| S8 | **不违 G-10（无出站）** | 上传是入站 `POST /api/v1/invoke/backup/backup.upload`；不新增 HTTP 客户端、不接对象存储、不发遥测、不跨网校验；`make` 的 G10 grep 面不变。 |
| S9 | **不引入第二条破坏性路径** | `upload` 不接收 `target_dir`、不调用 `Restore`；用户须显式点「恢复」→ 页面内二次确认 → `backup.restore` 的白名单/空目录门。上传本身不覆盖任何已有备份（同 id `E_CONFLICT`）。 |

---

## 4. 是否需新 ADR：**不需要**（但设触发条件）

| 判据 | 核实结论 |
| --- | --- |
| G-11「6 项明确不做」 | 六项为多用户 RBAC / 集群多机 / 模板市场 / 厂商云存储深度集成 / 拖拽建站 / 多语言全量框架（另容器编排、多租户 SaaS、移动 App、重型主题）。**本地文件上传不在其中**；插件 README:6 的「S3 等异地存储明确不做」指对象存储客户端，本方案不引入任何对象存储。 |
| ADR-0023 零侵入 §3.5 | 写入只在 `<state>/backups`（`config.go:312-326` 的 `defaultBackupDir`）及其内隐藏暂存目录，属 ADR §2 圈 A「核心状态（安装目录内自包含）」；不新增 `/etc`、`/var/lib`、`/run` 路径，不新增系统用户、unit 或 `changes.jsonl` 条目 → **清单外写入为零**。 |
| 协议兼容（G-14） | 新增方法是向后兼容的增量；旧内核对未知方法返回 `E_NOTFOUND`，前端按插件版本/`methods` 列表降级隐藏上传入口即可，不需要内核主版本递增。 |
| backlog C3 的「补齐前提需解除 G-11 并先立 ADR」 | 该前提针对 C3 中**异地/S3 存储与跨面板迁移**；round 28 已将「上传还原」单列为真差距且非 G-11，本轮用户裁决补齐此项，不等于解除其余部分的 G-11。 |

**触发条件（写进本方案，不静默开工）**：若未来要把上传扩展为①解析 1Panel/AMH 等他面板备份格式（跨面板迁移），或②接云端/异地对象存储，或③让上传直接覆盖运行中的站点目录，则分别触 G-11 有意偏离 / ADR-0023 §3.5 / 破坏性路径，**必须先写 ADR proposal 并入方案、经用户裁决后才可开工**。

---

## 5. 前端入口与涟漪

### 5.1 前端增量（`kore/web/backups.js`，UI 减法）

- **只加 1 个常驻控件**：页头（`build()` 的 `view-head-split`）在「⚙ 设置」旁加「上传备份」按钮；**不加行内按钮**（上传不是针对某一备份的操作），**不加一级菜单**（A6 不变），**不加新弹窗组件**。
- **复用现有 confirm 面板**：扩展 `openConfirm/paintConfirm` 的 `kind==='upload'` 分支，复用 `S.confirmHost` 与 `.bak-confirm` 样式——文件选择（`<input type="file" accept=".tar,.tar.gz">`，仅提示、后端仍校验）、总大小/可选 sha256 展示、「取消 / 确认导入」。导入成功后不自动跳恢复；结果经既有 `paintResult()`（新增 `resultTitle.upload='导入结果'`）展示 `id/entries/size/sha256`。
- **复用现有进度条**：`ensureProgress()` 的 `progHost/progState` 用于分块上传的真实已传字节/总字节（每块返回 `received_bytes`），不另造进度组件；失败保留「重试/取消」文字提示，给「怎么办」（A3：错误文案 ≥8 字含引导词，A4：不出现 capability 等内部术语）。
- **主流程 ≤3 点击**（A7）：点「上传备份」→ 选文件 → 点「确认导入」。旧插件不认识 `backup.upload` 时隐藏按钮并给出升级提示（沿用 `verify.supported` 的降级模式，`:1115-1117`）。
- **G-28 双端截图义务（验收硬项）**：隔离实例（独立 config/state/run/log，非 8787）下出 PC 1440×900 full-page + 手机 390×844 full-page 截图存 `kore/.agents/verify/screenshots/upload-*.png`，`file`/`identify` 读真实宽高，并写三问自评（解决什么问题 / 与宝塔 1Panel 差距 / 可用还是占位）到 `kore/.agents/verify/upload-ui-selfeval.md`；缺任一不得声明完成。

### 5.2 版本、索引与门禁涟漪（实施期执行，本方案不执行）

| 项 | 动作 |
| --- | --- |
| 版本 | feat → **minor**：`0.5.3 → 0.6.0`；`info.json` + `manifest.json` 同步 bump，`CHANGELOG.md` 加 0.6.0 中文条目（含安全门与本方案链接）。 |
| 包 | `make dist` 生成 `backup-0.6.0.tar.gz` + `.sha256`；`sha256sum -c` + `tar -tzf` + `stat -c%s` 校验，`size_bytes` 按 Makefile 的 ±2 收敛口径回填（`Makefile:35-92`）。 |
| 索引仓 | `kore-plugins/index/plugins.json`（version/sha256/size_bytes/download_url 用 dist 实测）、`index/versions.lock.json` 追加 0.6.0 账本、`presets/full.json` 与 `presets/db-only.json` 的 backup pin 更新、`build-market.sh` 重建 `market/plugins.js`、重签索引；`validate-index.sh` + `build-market.sh --check` exit 0。 |
| 测试替身 | `method_capabilities_test.go` 的读/写动词表与 16 条一致性断言、`methods_test.go`/`methods_new_test.go` 的分派用例、`tests/e2e/backup_e2e_test.go` 增补上传→导入→复用 restore 的正反例、`tests/boundary/` 的恶意归档/超大/越权用例；`internal/backupop/` 新增 `upload.go` + `upload_test.go`（含 Zip-Slip/设备文件/绝对路径/超长链接/截断/abort/TTL 清理）。 |
| 门禁 | backup 仓 `make check`（gofmt → vet → test → race）、`make dist`；kore 仓 `make docs-check`（既有 11 份行数 WARN 如实标注、不阻断）、`make frontend`、`make store-check`、`make presets-check`、`make market-check`、索引 `validate-index.sh`。 |
| 依赖 | 全部走标准库 `archive/tar` + `compress/gzip` + `crypto/sha256`，**预计无新 go.mod 依赖**；若确需新增，按已授权 goproxy 口径精确披露版本与来源后再动。 |
| 真机验证 | 隔离实例：①自造合法 tar.gz 分块上传 → `list` 见「外部包」→ `verify` sha_match=true → 点恢复+二次确认到空目录成功；②恶意归档（含 `../` / 绝对路径 / 设备文件 / 逃逸符号链接）在 commit 被拒、备份目录无新文件、无暂存残留；③超大包与越权（无 `system:admin`）被拒且审计有 `capability.denied`；④abort/超时清理 + `retention` dry_run 确认导入包被跳过；⑤清理隔离实例后回读主实例未变（G-29）。 |

---

## 6. 分期与可验证判据

| 期 | 范围 | 可验证判据（未达不得进下一期） |
| --- | --- | --- |
| P1 后端导入 | 新增 `backup.upload` 四阶段、隔离暂存、安全扫描、索引/能力声明、单测 + boundary | `make check` exit 0；`method_capabilities_test.go` 16 条断言全绿；恶意/超大/截断/越权用例均返回预期错误码且备份目录零落地、暂存零残留；合法包 commit 后 `list/verify` 可读。 |
| P2 前端入口 | `web/backups.js` 一个按钮 + 复用 confirm/进度 + 降级，G-28 双端截图 | `node --check` + `make frontend` exit 0；usability-check 无新增 FAIL；PC/手机截图真实宽高 + 自评三问落盘（§5.1）。 |
| P3 真机复验与发布 | 隔离实例端到端（§5.2 真机验证五项）+ 版本/dist/索引/重签 | 五项真机脚本逐项 PASS；`validate-index.sh` + `build-market.sh --check` exit 0；签名 tag / 推送 / Release 由用户逐项授权，本方案不执行。 |

### 诚实边界

- **本期只做「上传本地备份还原」的导入半程 + 复用既有恢复**：用户仍需显式点「恢复」并走两级确认；不做「上传即恢复」。
- **BT 的「从 1Panel / AMH 迁移」仍属 G-11 有意偏离**：不做他面板备份格式解析、不做跨面板语义映射；本期导入只接受本插件自身格式的 `.tar`/`.tar.gz`。
- **异地/S3 明确不做**（README:6 / backlog C3）；外部搬运仍由用户自己的同步工具负责。
- **未验证项如实标注**：本方案只经读码与文档核实，未写码、未跑真机上传；G-28 截图、真机五项、索引重签均在实施期产生，届时按证据贴命令+退出码，未跑不得写「通过」。
- **evidence 缺口沿用**：`backup.download` 的下载链路在 G-28 时未实机验证，本期不借上传方案替它背书（可另卡补验）。
