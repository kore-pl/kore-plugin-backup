# kore-plugin-backup

备份插件（**生产级**）：真实打包、恢复与完整性校验，不依赖任何外部 `tar` / `gzip`
命令或第三方 Go 库，全部使用标准库 `archive/tar` + `compress/gzip`，避免 shell 注入
与不同发行版 tar 版本差异。S3 等异地存储在 G-11 里**明确不做**：备份包落本地目录，
由外部同步工具搬运（本插件只留可下载引用与宿主路径，不内置任何对象存储客户端）。

## 协议面

- `kore.plugin.v1`，`lifecycle=lazy`，`depth_level=production`
- 能力声明：`system:admin`
- 方法：`backup.create` / `backup.list` / `backup.restore` / `backup.delete` / `backup.verify` /
  `backup.retention` / `backup.stats` / `backup.download` / `backup.jobs` /
  `backup.schedule.set` / `backup.schedule.get`（另 `backup.config.get` / `backup.config.set`；
  业务方法同时接受裸动词）
- 握手的 stdout 留给协议帧，日志一律写 stderr

## 方法一览

| 方法 | 参数 | 结果 |
| --- | --- | --- |
| `backup.create` | `name`、`dimensions?`、`paths?`、`db?`、`exclude?`、`compression=gzip\|none` | `id`、`path`、`size_bytes`、`uncompressed_bytes`、`entries`、`sha256`、`duration_ms`、`dimensions`、`stages[]`、`job`、`skipped[]`、`warnings[]` |
| `backup.list` | `{}` | `backups[]`（`id/name/created_at/size_bytes/sha256?/entries?/corrupted/managed`）、`count` |
| `backup.restore` | `id`、`target_dir`、`paths?` | `id`、`restored_entries`、`target_dir`、`failed[]`、`duration_ms` |
| `backup.delete` | `id` | `id`、`deleted`、`duration_ms` |
| `backup.verify` | `id` | `entries`、`size_bytes`、`uncompressed_bytes`、`sha256`、`sha_match`、`corrupted`、`duration_ms` |
| `backup.retention` | `keep`（1..100）、`dry_run?` | `kept`、`deleted[]`、`freed_bytes`、`dry_run`、`protected[]` |
| `backup.stats` | `{}` | `total_count`、`total_bytes`、`oldest`、`newest`、`by_name[]` |
| `backup.download` | `id`、`inline?`、`offset?`、`limit?` | `id`、`filename`、`path`、`size_bytes`、`sha256?`、`content_type`、`inline`、`offset`、`next_offset`、`eof`、`chunk_bytes`、`max_chunk_bytes`、`data_base64?` |
| `backup.jobs` | `id?`、`limit?`（1..50，缺省 20） | `jobs[]`（`id/name/status/dimensions/started_at/finished_at?/entries/bytes/archive_bytes?/archive_id?/sha256?/error?/stages[]`）、`count`、`hint` |
| `backup.schedule.set` | `name`、`schedule?`、`timeout_s?`、`dimensions?`、`paths?`、`exclude?`、`compression?`、`database?` | `created=false`、`task_id=""`、`saved`、`next_run_at`、`recommended_task`、`hint` |
| `backup.schedule.get` | `name?` | `records[]`（`name/created_at/updated_at/next_run_at/recommended_task/hint`）、`count`、`hint` |

### create

- 遍历每个 `path`（文件或目录）写入 tar；相对路径以 `path` 的父目录为基准
  （`/home/u/src` → 归档内 `src/...`）
- 保留文件模式、mtime、uid/gid；**不跟随符号链接**，按 `TypeSymlink` 记录链接自身
- `exclude` 是 glob 列表（`filepath.Match`），同时匹配完整相对路径与 basename；
  目录命中时跳过整棵子树
- 边写边算压缩后文件的 sha256；文件名 `<name>-<UTC时间戳>.tar.gz`（`none` 为 `.tar`）
- 备份包与 sidecar 权限均为 `0600`；sidecar 为 `<包>.sha256` 与 `<包>.meta.json`
- 单次上限：未压缩内容 ≤ 2 GiB、条目 ≤ 100 万；超限中止并删除半成品
- 源内设备/FIFO/socket 跳过并写入 `skipped[]` / `warnings[]`

### 备份维度（site / db / 组合）

- `dimensions` 显式声明，缺省 `["site"]`（只传 `paths` 的老调用方保持兼容）
- `site`：`paths[]` 必须在 allow_roots 白名单内，祖先符号链接解析后二次校验；系统关键
  目录 `/proc` `/sys` `/dev` `/run` 无条件拒绝
- `db`：`db.database` 走标识符白名单（字母/数字/下划线/美元/连字符，≤64，不以 `-` 开头），
  只导出显式声明的一个库；连接参数来自插件配置（`db_host/db_port/db_user/db_password/db_dump_bin`）
- `db` 维度先 `mysqldump --single-transaction --quick --skip-lock-tables --no-tablespaces`
  导出到 `<备份目录>/.stage-<id>/db/<database>.sql`，再以 `db/<database>.sql` 打进同一归档；
  导出完成（无论成败）都删除 staging
- **密码只经 `MYSQL_PWD` 环境变量传给子进程**，绝不进 argv、日志、方法结果或配置回显；
  子进程环境只透传 PATH/HOME/TMPDIR/LANG/TZ 等白名单变量
- 单库导出硬超时 30 分钟（与内核单任务硬上限同口径）；找不到 `mysqldump` 返回
  `E_UNSUPPORTED` 并给出安装/配置提示，不静默降级

### 定时备份（交给内核 scheduler）

- `backup.schedule.set` **只返回任务描述，不创建内核任务**：`created=false`、`task_id=""`，
  与 `ssl.renew.set` 契约对齐（`next_run_at` + `recommended_task{name,plugin,method,params,schedule,timeout_s}`）
- 把 `recommended_task` 原样 `POST /api/v1/scheduler/tasks` 才会真正生效；调度、错过补跑、
  并发互斥都由内核 scheduler 负责
- **插件内零 ticker / 零 AfterFunc / 零定时 goroutine**（G-19），lazy 插件空闲能真正退出；
  `g19_test.go` 对源码做静态门禁
- `schedule` 是 5 段表达式（分 时 日 月 周），支持 `*`、`n`、`a-b`、`a,b`、`*/n`、`a-b/n`、`a/n`；
  `next_run_at` 由插件按本地时区计算（仅用于展示，最终以内核任务为准）
- `timeout_s` 缺省 1800，范围 1–3600（与 scheduler 上限一致）
- `backup.schedule.get` 只读插件本地保存的任务描述（`.schedules.json`，0600），
  不代表内核任务状态；内核任务请查 `GET /api/v1/scheduler/tasks`

### 进度（可轮询的作业状态）

- 每次 `create` 都在 `<备份目录>/.jobs/` 落一条作业记录：状态机
  `pending → running → succeeded | failed`
- `entries` / `bytes` 是真实计数（running 期间由执行面按 ≥1s 间隔刷新，成功后 `archive_bytes`
  为归档包字节数）；`stages[]` 是真实阶段 `db_dump` / `pack` 的实测耗时，**没有伪百分比**
- `create` 结果体直接带 `job` 字段；也可调 `backup.jobs`（按 id 或最近 N 条）轮询
- 作业记录只保留最近 200 条（按 mtime 淘汰）；记账失败不影响备份本身

### 下载

- `backup.download` 只解析本插件备份目录内的包（id 白名单 + `Lstat` 常规文件 + 目录归属
  二次校验 + 符号链接解析后仍须在同一目录），**不接受任意路径**
- `inline=false`（缺省）返回可下载引用：`path` + `size_bytes` + `sha256` + `content_type`
- `inline=true` 按 `offset` / `limit` 分块返回 base64：单块缺省且上限 4 MiB（base64 后
  ≤5.34 MiB，低于 8 MiB 帧上限），用 `next_offset` 续取直到 `eof=true`

### restore

- **先解到目标同文件系统的临时目录**，全部条目成功解出后才 `rename` 到 `target_dir`；
  任一条目失败整包中止，临时目录删除，目标不落地任何文件
- 目标已存在时必须为空目录，否则 `E_CONFLICT`；成功时原子替换空目录
- `paths` 为归档内相对路径，精确命中或目录前缀命中；未命中的过滤项进入 `failed[]`
- 路径穿越防护：条目含绝对路径、`..` 段、反斜杠、NUL、超长（>4096）路径 → 整包 `E_INVALID`
- 不信任 tar 头：拒绝字符/块设备、FIFO、硬链接；拒绝单条目 >2 GiB；
  符号链接目标不得为绝对路径或逃出恢复根
- 恢复文件模式、mtime、uid/gid；非 root 无法恢复他人属主时降级跳过属主
- 常规文件丢弃 setuid/setgid 位

### retention / stats / verify

- `retention`：按创建时间倒序保留最近 `keep` 份（1..100），只删带本插件 `.meta.json` 与
  匹配 `.sha256` 的包；最新一份永不删除，外来包跳过记入 `protected[]`；`dry_run` 只出清单
- `stats`：汇总总数/总字节/最早/最晚与按 name 分组
- `verify`：重算整包 sha256 与 sidecar 比对，再逐条目解压校验；损坏仍如实返回

## 配置契约

`backup.config.get` / `backup.config.set`，落盘 `<KORE_STATE_DIR>/config.json`（0600，原子写），
优先级 `config.json` > 环境变量 > 默认值：

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `dir` | `<KORE_STATE_DIR>/backups` | 备份包目录（绝对路径、Clean） |
| `allow_roots` | `["/var/www","/etc/kore"]` | 备份/恢复路径白名单 |
| `db_host` | `127.0.0.1` | db 维度 MySQL 主机 |
| `db_port` | `3306` | db 维度 MySQL 端口 |
| `db_user` | `root` | db 维度 MySQL 用户 |
| `db_password` | 空 | 敏感字段：只写不回显，`config.get` 返回 `***`；占位值不覆盖旧密码 |
| `db_dump_bin` | `mysqldump` | mysqldump 可执行文件路径或命令名 |

环境变量兼容：`KORE_BACKUP_DIR`、`KORE_BACKUP_ALLOW_ROOTS`、`KORE_BACKUP_DB_HOST`、
`KORE_BACKUP_DB_PORT`、`KORE_BACKUP_DB_USER`、`KORE_BACKUP_DB_PASSWORD`、
`KORE_BACKUP_MYSQLDUMP_BIN`。

## 安全边界

- **路径白名单**：`paths` 与 `target_dir` 必须在 allow_roots 之下；支持逗号或系统路径分隔符
- **系统关键目录一律拒绝**：`/proc` `/sys` `/dev` `/run`，即使白名单误配
- 祖先目录符号链接会被解析并二次校验；末尾条目符号链接按链接记录，不读目标内容
- 备份目录缺省 `<KORE_STATE_DIR>/backups`；备份包与 sidecar 权限 0600
- 日志不输出路径内容与凭据，只输出数量与大小；`db_password` 不进 argv、不进错误消息

## 测试

```bash
gofmt -l . && go vet ./... && go test -timeout 200s ./...
go test -tags e2e -timeout 200s ./tests/e2e -v   # 真实插件二进制 + UDS 端到端
make build && make dist
tar -tzf dist/backup-0.4.0.tar.gz | head -1      # info.json
```

单元测试覆盖真实目录树打包恢复、`exclude`、确定性 sha256、路径穿越整包拒绝、白名单与
`/proc` 拒绝、篡改包 `sha_match=false`、删除不存在 `E_NOTFOUND`；新增覆盖
schedule 契约/非法 cron/next_run_at、download 分块往返与符号链接越界、jobs 状态机与失败
记录、db 维度 argv/环境变量/组合/失败映射。`tests/fault/` 四类故障注入覆盖 11 个方法。

`dist` 使用确定性参数归档（显式条目 + `--sort=name --mtime=@0 --owner=0 --group=0
--numeric-owner` + `gzip -9n` + chmod 归一），重复构建 sha256 不变；`size_bytes` 由打包
脚本回填，与实际包体偏差 ≤2 字节。

<!-- oss:begin -->
## 开源与贡献

[![CI](https://github.com/kore-pl/kore-plugin-backup/actions/workflows/ci.yml/badge.svg)](https://github.com/kore-pl/kore-plugin-backup/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go 1.26](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](go.mod)

本仓库是 **kore** 的 `备份插件`（id `backup`），独立仓库、独立版本线、独立 CI（少触发：push main / tag / PR / 手动）。内核与协议见 [kore](https://github.com/kore-pl/kore)。

- **许可**：MIT（inbound = outbound，不要求 CLA）；提交用 `git commit -s` 签 DCO。
- **贡献**：[CONTRIBUTING.md](CONTRIBUTING.md)（构建、门禁、发版、边界）。
- **安全**：[SECURITY.md](SECURITY.md)——漏洞不要开公开 issue。
- **行为准则**：[CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md)。
- **兼容性**：`protocol: kore.plugin.v1` · `requires_kore: >=0.1.0 <0.2.0` · `lifecycle: lazy` · 深度等级 `production`。
- **验证方式**：本地 `make check` 优先（CI 只在 push main / tag / PR / 手动跑；发布前 `make dist` 连跑两次核对 sha 一致）。
- **发布产物**：`make dist` → `dist/backup-0.4.0.tar.gz` + `.sha256`（位级可复现，连跑两次 sha 相同）。
- **相关仓库**：[索引 kore-plugins](https://github.com/kore-pl/kore-plugins) · [协议文档](https://github.com/kore-pl/kore/blob/main/docs/PLUGIN-PROTOCOL.md) · [SDK 指南](https://github.com/kore-pl/kore/blob/main/docs/PLUGIN-SDK.md)

<!-- oss:end -->
