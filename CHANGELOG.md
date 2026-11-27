# Changelog

## 0.6.0 (2026-11-27)

补齐 BT「备份还原」一手确证的真差距：**上传本地备份导入**，恢复仍走既有 `backup.restore` 两级确认，不做「上传即恢复」（方案见 `docs/DESIGN-UPLOAD-RESTORE.md`）。

- feat: 新增写方法 **`backup.upload`**（`system:admin`），四阶段 `begin / chunk / commit / abort` 共用一个方法；4 MiB base64 分块入站，对称 `backup.download`，不新增内核端点、不入任何出站（G-10）。
- feat: 隔离暂存 `0700` 隐藏目录 → 顺序写 → commit **只读安全扫描**（复用 `validateRestoreHeader` / `validateSymlinkTarget` / `withinPath`）→ create-only 移入备份目录；失败 / 30 分钟 TTL 清理半包。
- feat: 安全门 S1–S9：仅 `.tar` / `.tar.gz`、声明总长 ≤512 MiB（硬顶 2 GiB）、单块 ≤4 MiB、`ValidateID` + `assertInsideBackupDir`（含 `EvalSymlinks`）、拒绝对路径 / `..` / 设备 / FIFO / 硬链接 / 逃逸符号链接 / 超长链接 / 解压炸弹；审计复用内核 `capability.denied`。
- feat: 导入包**自算 `.sha256`、不写 `.meta.json`** ⇒ `Managed=false`，保留策略不会误删用户导入的包；`list` / `verify` 立即可读，既有 `restore` 可直接恢复。
- fix: 非 root 容器/沙箱里 `chown` 改他人属主返回 `EINVAL`（本机实测 errno 22）时，与既有 `EPERM` 同口径降级跳过属主恢复；此前会让 `restore` 误报 `E_INTERNAL`，属存量环境缺陷，非上传功能引入。
- test: `upload_test.go`（合法往返 / 11 类恶意归档 / 超大 / 乱序 / abort / TTL / sha 不符）、`tests/boundary/upload_test.go`（真实执行面恶意与超大）、`tests/e2e/upload_e2e_test.go`（真插件 UDS 上传→复用 restore）；`method_capabilities_test.go` / `methods_new_test.go` / fault 替身同步到 16 条方法。
- build: `info.json` / `manifest.json` 0.6.0，`methods` 与 `method_capabilities` 增 `backup.upload`；`make dist` 产出 `backup-0.6.0.tar.gz`。

## 0.5.3 (2026-11-26)

### 新增
- 补齐七语言（zh/en/es/fr/de/ru/pt）`DETAIL` 文档与 `locale/*.json`、README 语言切换行与 star 提示；无日语文件、无死链，JSON 校验通过。
- 新增六语言 `SECURITY` 译文、`.github/dependabot.yml`（gomod/npm/github-actions weekly）与 `.github/CODEOWNERS`。
- `CONTRIBUTING.md` 写明 Git Flow 分支模型（kebab-case 命名、release/hotfix 双向合流、`--no-ff`、SSH 签名 tag 约定）。

## 0.5.2 (2026-11-25)

补齐易用性原则②「自动发现、不让人填环境细节」在备份页的缺口
（`kore/docs/USABILITY.md` §5 记「备份源路径列表仍需手填」）：

- feat: 新增只读方法 **`backup.sources.detect`**（`system:read`）——只读探测本机常见备份源，
  返回 `{allow_roots, sources[], suggested_paths, checked, detected_at, hint?, note?}`：
  站点根（`/var/www`、`/srv/www` 及其真实子目录）、应用目录（`/opt` 子目录）、
  关键配置（`/etc/nginx`、`/etc/kore`）、数据库数据目录（`/var/lib/mysql` 等，只报告不预填）
- feat: **白名单内才预填**——只有「允许的根路径」之内的目录进 `suggested_paths`，
  白名单外的照样如实报告 `in_roots=false`，避免预填一个点「创建」必失败的路径；
  探不到时 `hint` 给可执行的三条下一步（先建站点 / 加入允许根路径 / 手填绝对路径）
- feat: 数据目录只报告不预填 + `note` 说明一致性导出做法（改用「数据库备份」预设做 mysqldump）
- fix: 只读、无 root 依赖、有界——`os.Lstat`/`os.ReadDir`，读不了就跳过；
  每容器最多 12 个子目录、总数 32，不递归遍历
- test: `internal/backupop/detect_test.go` 七条——站点目录枚举 / 白名单外不预填 /
  数据目录不预填 / 空容器退回自身 / 去重与父路径折叠 / 方法契约只读（探测前后目录不变）/ 未知方法拒绝

## 0.5.1 (2026-11-24)

修复 P3 错误消息缺陷（HANDOFF B.6 挂账 #9，披露于 `07e9ac5`）：

- **修复（权限失败不提怎么办）**：非 root 备份 root-only 源（如 `/etc/shadow`）时，源文件打开失败原被 `skip()` 吞成 warning，最终只报「没有可备份的条目」，用户不知道是权限问题。现在 `archiveBuilder` 记录权限跳过数，零条目消息点明「N 个源文件因权限不足被跳过（permission denied）；请检查属主与权限，或以 root 运行后重试」
- **修复（errno 上下文缺，同类）**：`writeFile` 的 `io.CopyN` 读失败消息补上底层 errno（EFBIG/ENOSPC 等）与「怎么办」；`errnoText` 剥掉 `fs.PathError` 的宿主绝对路径，不泄露目录布局
- **测试**：新增 `internal/backupop/archive_errors_test.go`——权限跳过零条目消息含 permission denied + 可操作建议、errnoText 剥路径两类断言；既有 boundary 断言（「没有可备份的条目」「读取文件内容失败」前缀）全部保持通过

## 0.5.0 (2026-11-24)

- feat: **备份预设（预设优先、自定义兜底）**——`backup.create` 新增 `preset` 参数：
  `site-full`（网站全量，默认）/ `config`（关键配置）/ `db`（数据库，需 `db.database`）/ `full`（整机关键数据）
- feat: **零参数一键**——`backup.create` 传 `{}` 时套默认预设：自动发现白名单内已存在的站点目录
  （`/var/www`、`/srv/www`、`/var/www/html`，全无则退回已存在的白名单根），名字取 `site-full`、压缩 gzip；
  不再报「备份 name 不能为空」，小白一次调用即成功
- feat: 新增只读方法 `backup.presets`（`system:read`）：返回预设清单、保留策略预设、本机真实展开结果
  （`expanded.paths`）与仍需补的字段，前端据此渲染一键按钮
- feat: 保留策略预设——`backup.retention` 支持 `preset=daily7|weekly4|monthly12`（keep 7/4/12 +
  推荐 cron + 依据），显式 `keep` 仍优先；结果回显 `preset` / `schedule`
- fix: 严格校验不放松——只要调用方传了任何业务字段且没给 `preset`，显式漏传 `paths` 仍是 `E_INVALID`；
  0.4.2 的 boundary 断言（`TestAbnormalInputEmptySitePaths` 等 7 类异常输入）全部保持通过
- test: `preset_test.go` 五条：零参数 create 成功且内容符合默认预设 / 三条预设展开正确 + 未知预设拒绝 +
  db 预设必须选库 / 保留策略预设展开 / 自定义参数与 `.tar` 兜底仍可用 / **反例自证**（去掉默认预设后零参数用例 FAIL）
- build: `info.json` / `manifest.json` 版本 0.5.0，`methods` 增 `backup.presets`，
  `method_capabilities` 增 `backup.presets: system:read`

## 0.4.2 (2026-11-23)

- feat: **`backup.verify` 完整化**（只读，`system:read`）——返回 `{ok, checks[], sha256, size_bytes, created_at}`，逐项检查 文件存在 / 大小 > 0 / sha256 与记录一致 / 大小与记录一致 / 压缩包可完整读取 / 条目数与记录一致
- fix: 失败文案按类别区分，能说清坏在哪：截断 → 「文件不完整（实际 X 字节 < 记录 Y 字节，疑似被截断）」/「压缩流在中途结束」；sha 不符 → 「内容被改动或写入不完整」；gzip/tar 坏 → 「压缩包损坏」；空文件 → 「文件为空（0 字节）」；旧备份无 sha → `legacy=true` + 仅检查可读性降级说明
- feat: 插件自带 `web/index.html` 备份列表「校验」按钮，按检查项显示 ✓/✗ + 原因（响应式；不碰主壳 web）
- test: `verify_test.go` 覆盖正常包 / 截断 / 篡改一字节 / 空文件 / 旧备份缺 sha 降级 / 不存在 id

## 0.4.1 (2026-11-23)

- feat(protocol): `info.json` 补 `method_capabilities` 13 条（11 业务 + `config.get`/`config.set`）——只读 7 个方法声明 `system:read`（`list`/`verify`/`stats`/`download`/`jobs`/`schedule.get`/`config.get`），写类 6 个声明 `system:admin`；未声明的方法会回退插件级 `system:admin`，导致"看个备份列表也进 admin 审计"（PLUGIN-MATURITY §3.5 #12）
- test: 声明一致性表驱动门禁（只读/写分类逐条断言 + 声明 ⊆ 实现反向断言 + 条数相等 + config 两条必含）

## 0.4.0 (2026-11-23)

- feat: `backup.schedule.set` / `backup.schedule.get`：对齐 `ssl.renew.set` 契约，输出内核
  scheduler 可消费的任务描述（`created=false`、`task_id=""`、`next_run_at`、
  `recommended_task{name,plugin,method,params,schedule,timeout_s}`），由前端 POST
  `/api/v1/scheduler/tasks` 生效；插件内**零 ticker / 零 AfterFunc / 零定时 goroutine**（G-19），
  `g19_test.go` 对源码做静态门禁
- feat: `backup.download`：返回可下载引用（`path` + `size_bytes` + `sha256` + `content_type`），
  `inline=true` 按 `offset`/`limit` 分块返回 base64（单块 ≤4 MiB，低于 8 MiB 帧上限）；
  只读本插件备份目录内常规文件，符号链接与越界路径拒绝
- feat: 作业进度：`create` 在 `<备份目录>/.jobs/` 落记录，状态机
  `pending → running → succeeded | failed`，`entries`/`bytes` 为真实计数，
  `stages[]` 记录 `db_dump`/`pack` 实测耗时；`backup.jobs` 可轮询，`create` 结果体带 `job`
- feat: `create` 新增 `dimensions=["site","db"]` 维度显式声明（缺省 `["site"]` 向后兼容）：
  db 维度经 `mysqldump` 导出到 `db/<database>.sql` 后打进同一归档；密码只经 `MYSQL_PWD`
  传给子进程（不进 argv/日志/结果），子进程环境白名单透传，30 分钟硬超时，staging 0600 后清理
- feat: 配置契约扩展 `db_host` / `db_port` / `db_user` / `db_password`（sensitive，0600 落盘、
  config.get 掩码、占位值不覆盖旧密码）/ `db_dump_bin`；0.3.0 旧配置自动补默认值，
  兼容 `KORE_BACKUP_DB_*` / `KORE_BACKUP_MYSQLDUMP_BIN` 环境变量
- test: schedule 契约/非法 cron/`next_run_at`、download 分块往返与越界拒绝、jobs 状态机与
  失败记录、db 维度 argv/环境变量/组合/失败映射、config 掩码与旧配置兼容；fault 四类注入
  覆盖扩到 11 个方法
- build: `info.json` / `manifest.json` 同步 `version=0.4.0` 与 `methods` 声明

## 0.3.0 (2026-11-23)

- feat: `backup.retention {keep, dry_run?}` 保留策略：按创建时间倒序保留最近 1..100 份，
  其余删除并返回 `{kept, deleted[], freed_bytes}`；`dry_run=true` 只返回将删清单
- feat: 保留策略保护边界——最新一份永不删除；只删带本插件 `.meta.json` + 匹配 `.sha256`
  的包，手工放入的外来包跳过并记入 `protected[]`；`backup.list` 新增 `managed` 字段
- feat: `backup.stats` 汇总 `{total_count, total_bytes, oldest, newest, by_name[]}`，
  分组按数量倒序、同数量按 name 升序
- test: retention 的 dry_run/真删/最新一份保护/外来包保护/keep 越界/中途失败上下文，
  stats 的空目录与分组汇总；故障注入覆盖扩到七个方法
- docs: README 七方法表与 retention/stats 语义

## 0.2.1 (2026-11-23)

- fix: 位级可复现打包（stage chmod + tar --mode 归一、size_bytes 收敛）；二进制改嵌 manifest.json，消除版本/大小回填导致的同版本 sha 漂移

## 0.2.0 (2026-11-22)

- feat: 统一插件配置契约（`backup.config.get` / `backup.config.set`）：界面可配备份目录与
  `allow_roots` 白名单，保存即生效、无需重启
- feat: 配置存储 `<KORE_STATE_DIR>/config.json`（0600，原子写），优先级
  `config.json` > `KORE_BACKUP_DIR` / `KORE_BACKUP_ALLOW_ROOTS` > 默认值；默认白名单收敛为
  `/var/www`、`/etc/kore`（原 `/home` 需在界面显式添加）
- feat: 通用 `internal/plugincfg`（schema 校验 / 0600 原子存储 / sensitive `***` 掩码）与
  dir、allow_roots 的写盘前校验；失败不写半份文件、旧配置继续可用
- feat: backup.create 真实打包：标准库 tar+gzip、边写边算 sha256、保留相对路径/模式/
  mtime/uid/gid、不跟随符号链接、exclude glob、2 GiB / 100 万条目上限与半成品清理
- feat: backup.list 扫描备份目录并解析 `.sha256` / `.meta.json` sidecar，读 tar 头失败标记
  `corrupted=true`
- feat: backup.restore 临时目录完整解包后 rename 的原子恢复，支持 paths 过滤、目标空目录
  校验、路径穿越整包拒绝、设备/FIFO/硬链接与超 2 GiB 条目拒绝、符号链接逃逸防护
- feat: backup.delete 删除备份包与两个 sidecar，不存在返回 E_NOTFOUND
- feat: backup.verify 重算 sha256 与 sidecar 比对 + 逐条目解压校验，报告
  entries/size/sha_match/corrupted
- feat: 路径白名单 `KORE_BACKUP_ALLOW_ROOTS`（默认 /var/www、/etc/kore、/home），
  `/proc` `/sys` `/dev` `/run` 无条件拒绝；备份目录 `KORE_BACKUP_DIR`（默认
  `<KORE_STATE_DIR>/backups`）；备份包与 sidecar 权限 0600
- test: 真实目录树往返、exclude、确定性 sha256、路径穿越/设备条目/超长路径/超限、
  白名单、篡改包 sha_match=false、delete E_NOTFOUND；四类故障注入覆盖五个方法
- test: 真实插件二进制 + UDS 的七步 E2E（create/list/verify/restore/diff/delete/恶意包拒绝）
- docs: README 更新五方法、安全边界、上限与验收命令

## 0.1.0 (2026-11-22)

- feat: 新建 backup 插件骨架，复用 kore.plugin.v1 握手与会话循环
- feat: backup.jobs / backup.create 方法级参数校验与结构化结果，目标路径白名单 + retention 天数校验，不真写本地或 S3 存储
- feat: 四类故障注入测试（kill -9 / 超时 / 磁盘满 / cgroup 超限），验证方法级错误返回
- build: 确定性打包（--sort=name --mtime=@0 --owner=0 --numeric-owner + chmod 归一 + gzip -9n）
