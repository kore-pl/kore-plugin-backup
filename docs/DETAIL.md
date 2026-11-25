> 状态: draft · 归属: wave1 · 更新: 2026-11-25 · 里程碑: M5

# kore-plugin-backup

本仓实现 kore 的备份插件（id `backup`）。打包 / 恢复 / 校验全部使用 Go 标准库 `archive/tar` + `compress/gzip`，避免 shell 注入与不同发行版 tar 版本差异。异地存储由外部同步工具搬运，本插件只留可下载引用与宿主路径。

语言：[简体中文](DETAIL.md) · [English](DETAIL.en.md) · [Español](DETAIL.es.md) · [Français](DETAIL.fr.md) · [Deutsch](DETAIL.de.md) · [Русский](DETAIL.ru.md) · [Português](DETAIL.pt.md)

## 它做什么

- **协议面**：`kore.plugin.v1`，`lifecycle=lazy`，`depth_level=production`，能力声明 `system:admin`。
- **方法**：`backup.create` / `list` / `restore` / `delete` / `verify` / `retention` / `stats` / `download` / `jobs` / `schedule.set` / `schedule.get` 等；业务方法同时接受裸动词。
- **完整性**：校验 `exists` / `size` / `sha256` / `readable` / `entries` 逐项给出，损坏包明确标记，不静默放过。
- **本地存储**：备份包落本地目录（非 S3），由外部同步工具搬运；`download` 支持分块读取。
- **最小权限**：`info.json.capabilities` 即全部授权面；写操作 fail-closed，越出声明范围即拒绝。

## 快速开始

```bash
make check    # gofmt → vet → test
make build    # 构建插件二进制
make dist     # 产出发布 tarball
```

插件由 kore 主壳按需拉起；`info.json` 声明能力与 `requires_kore` 区间。

## 安全与边界

漏洞披露走 [SECURITY.md](../SECURITY.md)

## 文档导航

[SECURITY.md](../SECURITY.md) — 漏洞披露流程。
[CONTRIBUTING.md](../CONTRIBUTING.md) — 提交与发版规范。
[CHANGELOG.md](../CHANGELOG.md) — 版本变更记录。
[info.json](../info.json) — 能力与版本声明。

## 常见问题

**问：需要注册账号才能用吗？**
不需要。没有账号、无联网校验、无隐藏上报；配置明文，看得懂也改得动。

**问：插件住在哪？**
插件住在各自 `kore-plugin-<id>` 仓，只依赖 `kore/pkg/pluginapi`。

**问：我该先读什么？**
先看 README 的协议面与方法表，再看 `info.json` 的能力声明。
