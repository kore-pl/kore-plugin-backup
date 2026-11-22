# Changelog

## Unreleased

- feat: 统一插件配置契约（`backup.config.get` / `backup.config.set`）：界面可配备份目录与
  `allow_roots` 白名单，保存即生效、无需重启
- feat: 配置存储 `<KORE_STATE_DIR>/config.json`（0600，原子写），优先级
  `config.json` > `KORE_BACKUP_DIR` / `KORE_BACKUP_ALLOW_ROOTS` > 默认值；默认白名单收敛为
  `/var/www`、`/etc/kore`（原 `/home` 需在界面显式添加）
- feat: 通用 `internal/plugincfg`（schema 校验 / 0600 原子存储 / sensitive `***` 掩码）与
  dir、allow_roots 的写盘前校验；失败不写半份文件、旧配置继续可用

## 0.2.0 (2026-11-22)

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
