# kore-plugin-backup

备份插件（**骨架**）：覆盖本地 + S3 兼容备份的方法面。本仓只交付协议骨架与参数校验，**不真写任何本地或 S3 存储、不内置 awscli/rclone 等任何工具二进制**（裁决 37 / 提案 §2.1 批 2）。S3 兼容统一以 `backend="s3"` 声明，具体端点/凭据由使用者自行提供。

## 协议面

- `kore.plugin.v1`，`lifecycle=lazy`，`depth_level=production`
- 能力声明：`system:admin`
- 方法：`backup.jobs` / `backup.create`（同时接受裸 `jobs` / `create`）
- 握手的 stdout 留给协议帧，日志一律写 stderr

## 参数校验（骨架口径）

- `backup.jobs`：`limit` 非负（0 表示不限制），返回作业列表的结构化结果
- `backup.create`：`target` 必须在配置根目录（`KORE_BACKUP_ROOT`，缺省 `/var/lib/kore/backup`）白名单内——拒绝绝对路径、空路径、`.` / `..` 段、反斜杠与 NUL；`backend` 仅 `local` / `s3`；`retention_days` 在 `1..3650`
- 根目录与后端仅用于逻辑校验，**不执行任何落盘、上传或对象存储调用**

非法参数返回 `E_INVALID`；未知方法返回 `E_NOTFOUND`。

## 四类故障注入

`tests/fault/` 四个文件分别注入 kill -9、超时、磁盘满、cgroup 超限，断言方法级返回结构化错误，且 `Error.Code` 与故障类别一一对应：

| 故障 | 文件 | 期望错误码 |
| --- | --- | --- |
| kill -9 | `kill_test.go` | `E_UPSTREAM` |
| 超时 | `timeout_test.go` | `E_UPSTREAM` |
| 磁盘满 | `disk_test.go` | `E_INTERNAL` |
| cgroup 超限 | `cgroup_test.go` | `E_UNSUPPORTED` |

每个用例经 `backupop.Call` 的执行面注入，覆盖 `backup.jobs` 与 `backup.create`，并配无故障 sanity 用例防止"永远失败"的假通过。

## 构建与验收

```bash
gofmt -l . && go vet ./... && go test -timeout 200s ./...
make build && make dist
tar -tzf dist/backup-0.1.0.tar.gz | head -1   # info.json
```

`dist` 使用确定性参数归档（显式条目 + `--sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner` + `gzip -9n` + chmod 归一），重复构建 sha256 不变；`size_bytes` 由打包脚本回填，与实际包体偏差 ≤10%。会话循环中每个 `kore.shutdown` 请求都配对读取响应帧，避免 net.Pipe 无缓冲导致的死锁。
