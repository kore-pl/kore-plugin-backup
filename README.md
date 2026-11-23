# kore-plugin-backup

备份插件（**生产级**）：真实打包、恢复与完整性校验，不依赖任何外部 `tar` / `gzip`
命令或第三方 Go 库，全部使用标准库 `archive/tar` + `compress/gzip`，避免 shell 注入
与不同发行版 tar 版本差异。S3 兼容后端不在本插件内实现，备份包落本地目录，可由
外部同步工具搬运。

## 协议面

- `kore.plugin.v1`，`lifecycle=lazy`，`depth_level=production`
- 能力声明：`system:admin`
- 方法：`backup.create` / `backup.list` / `backup.restore` / `backup.delete` / `backup.verify` /
  `backup.retention` / `backup.stats`（同时接受裸动词）
- 握手的 stdout 留给协议帧，日志一律写 stderr

## 七个方法

| 方法 | 参数 | 结果 |
| --- | --- | --- |
| `backup.create` | `name`、`paths[]`、`exclude?`、`compression=gzip\|none` | `id`、`path`、`size_bytes`、`entries`、`sha256`、`duration_ms`、`skipped[]`、`warnings[]` |
| `backup.list` | `{}` | `backups[]`（`id/name/created_at/size_bytes/sha256?/entries?/corrupted/managed`）、`count` |
| `backup.restore` | `id`、`target_dir`、`paths?` | `id`、`restored_entries`、`target_dir`、`failed[]`、`duration_ms` |
| `backup.delete` | `id` | `id`、`deleted`、`duration_ms` |
| `backup.verify` | `id` | `entries`、`size_bytes`、`uncompressed_bytes`、`sha256`、`sha_match`、`corrupted`、`duration_ms` |
| `backup.retention` | `keep`（1..100）、`dry_run?` | `kept`、`deleted[]`（`id/name/created_at/size_bytes`）、`freed_bytes`、`dry_run`、`protected[]`（`id/name/reason`） |
| `backup.stats` | `{}` | `total_count`、`total_bytes`、`oldest`、`newest`、`by_name[]`（`name/count/bytes`） |

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

### retention

- 按创建时间倒序保留最近 `keep` 份（1..100，必填），其余删除；`keep` 越界 `E_INVALID`
- **只删本插件创建的包**：需同时有 `.meta.json`（`id` 与文件名一致）与匹配的 `.sha256`；
  手工拷贝/外来的 tar.gz 一律跳过并记入 `protected[]`，`managed=false` 可在 `list` 里预先看到
- **最新一份永不删除**：`keep ≥ 1` 时排序后的第 0 份天然落在保留窗口内
- `dry_run=true` 只返回将删清单与预计 `freed_bytes`，不改磁盘；真删按同一清单执行
- 删除复用 `backup.delete` 的"包 + 两个 sidecar"语义；中途失败返回错误并注明已删除份数

### stats

- 汇总 `total_count` / `total_bytes` / `oldest` / `newest`（无备份时 `null`）与 `by_name[]` 分组
- 分组按数量倒序、同数量按 name 升序；创建时间无法解析的条目不参与最早/最晚判定

### verify

- 重算整个包 sha256 与 `.sha256` sidecar 比对
- 逐条目读取 tar 内容验证可解压，统计 `entries` 与 `uncompressed_bytes`
- 包损坏或 sidecar 缺失时仍返回结果：`corrupted` / `sha_match` 如实反映

## 安全边界

- **路径白名单**：`paths` 与 `target_dir` 必须在 `KORE_BACKUP_ALLOW_ROOTS` 之下，
  默认 `["/var/www","/etc/kore","/home"]`；支持逗号或系统路径分隔符
- **系统关键目录一律拒绝**：`/proc` `/sys` `/dev` `/run`，即使白名单误配
- 祖先目录符号链接会被解析并二次校验，防止经链接写穿白名单；末尾条目符号链接
  按链接记录，不读取目标内容
- 备份目录 `KORE_BACKUP_DIR`，缺省 `<KORE_STATE_DIR>/backups`（`KORE_STATE_DIR`
  缺省 `/var/lib/kore`）
- 日志不输出路径内容，只输出数量与大小

## 测试

```bash
gofmt -l . && go vet ./... && go test -timeout 200s ./...
go test -tags e2e -timeout 200s ./tests/e2e -v   # 真实插件二进制 + UDS 七步 E2E
make build && make dist
tar -tzf dist/backup-0.2.0.tar.gz | head -1      # info.json
```

单元测试覆盖真实目录树（子目录/空文件/大文件/中文名/符号链接）打包恢复、`exclude`
glob、确定性 sha256、路径穿越整包拒绝、白名单与 `/proc` 拒绝、篡改包
`sha_match=false`、删除不存在 `E_NOTFOUND`。`tests/fault/` 四类故障注入覆盖七个方法。

`dist` 使用确定性参数归档（显式条目 + `--sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner`
+ `gzip -9n` + chmod 归一），重复构建 sha256 不变；`size_bytes` 由打包脚本回填，
与实际包体偏差 ≤10%。
