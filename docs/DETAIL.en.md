> 状态: draft (草稿) · 归属: wave1 · 更新: 2026-11-25 · 里程碑: M5

# kore-plugin-backup

This repository implements kore’s backup plugin (id `backup`). Pack/restore/verify use only Go’s `archive/tar` + `compress/gzip`, avoiding shell injection and cross-distro tar differences. Off-site transfer is left to external sync tools; the plugin keeps only downloadable references and host paths.

语言：[简体中文](DETAIL.md) · [English](DETAIL.en.md) · [Español](DETAIL.es.md) · [Français](DETAIL.fr.md) · [Deutsch](DETAIL.de.md) · [Русский](DETAIL.ru.md) · [Português](DETAIL.pt.md)

## What it does

- **Protocol**: `kore.plugin.v1`, `lifecycle=lazy`, `depth_level=production`, capability `system:admin`.
- **Methods**: `backup.create` / `list` / `restore` / `delete` / `verify` / `retention` / `stats` / `download` / `jobs` / `schedule.set` / `schedule.get` and more; bare verbs are also accepted.
- **Integrity**: checks for `exists` / `size` / `sha256` / `readable` / `entries` are reported item by item; corrupted archives are flagged, never silently ignored.
- **Local storage**: archives stay in a local directory (not S3) and are moved by external sync tools; `download` supports chunked reads.
- **Least privilege**: `info.json.capabilities` is the full authorization surface; writes fail closed.

## Quick start

```bash
make check    # gofmt → vet → test
make build    # build the plugin binary
make dist     # produce the release tarball
```

The plugin is started on demand by the kore shell; `info.json` declares capabilities and `requires_kore`.

## Security and boundaries

Report vulnerabilities through [SECURITY.md](../SECURITY.md)

## Documentation map

[SECURITY.md](../SECURITY.md) — disclosure process.
[CONTRIBUTING.md](../CONTRIBUTING.md) — commit and release conventions.
[CHANGELOG.md](../CHANGELOG.md) — version history.
[info.json](../info.json) — capabilities and version declaration.

## FAQ

**Q: Do I need an account?**
No. There is no account, online verification or hidden telemetry; configuration is plain text.

**Q: Where do plugins live?**
Each plugin lives in its own `kore-plugin-<id>` repository, depending only on `kore/pkg/pluginapi`.

**Q: Where should I start?**
Read the protocol surface and method table in the README, then the capability declarations in `info.json`.
