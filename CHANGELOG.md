# Changelog

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
