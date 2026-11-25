> 状态: Entwurf (草稿) · 归属: wave1 · 更新: 2026-11-25 · 里程碑: M5

# kore-plugin-backup

Dieses Repository implementiert das Backup-Plugin für Kore (id `backup`). Packen/Wiederherstellen/Prüfen nutzt nur Go-`archive/tar` + `compress/gzip`, um Shell-Injektion und distro-spezifische tar-Unterschiede zu vermeiden. Der Offsite-Transfer erfolgt durch externe Sync-Tools; das Plugin speichert nur herunterladbare Referenzen und Host-Pfade.

语言：[简体中文](DETAIL.md) · [English](DETAIL.en.md) · [Español](DETAIL.es.md) · [Français](DETAIL.fr.md) · [Deutsch](DETAIL.de.md) · [Русский](DETAIL.ru.md) · [Português](DETAIL.pt.md)

## Was es tut

- **Protokoll**: `kore.plugin.v1`, `lifecycle=lazy`, `depth_level=production`, Capability `system:admin`.
- **Methoden**: `backup.create` / `list` / `restore` / `delete` / `verify` / `retention` / `stats` / `download` / `jobs` / `schedule.set` / `schedule.get`; nackte Verben ebenfalls erlaubt.
- **Integrität**: Prüfungen `exists` / `size` / `sha256` / `readable` / `entries` werden einzeln gemeldet; beschädigte Archive werden markiert, nie ignoriert.
- **Lokaler Speicher**: Archive bleiben lokal (kein S3) und werden von externen Tools verschoben; `download` unterstützt blockweise Lesung.
- **Least Privilege**: `info.json.capabilities` ist die gesamte Autoritätsfläche; Schreibzugriffe fail-closed.

## Schnellstart

```bash
make check    # gofmt → vet → test
make build    # Plugin-Binary bauen
make dist     # Release-Tarball erzeugen
```

Das Plugin wird von der Kore-Shell bei Bedarf gestartet; `info.json` deklariert Capabilities und `requires_kore`.

## Sicherheit und Grenzen

Melde Schwachstellen über [SECURITY.md](../SECURITY.md)

## Dokumentationsübersicht

[SECURITY.md](../SECURITY.md) — Offenlegungsprozess.
[CONTRIBUTING.md](../CONTRIBUTING.md) — Commit- und Release-Konventionen.
[CHANGELOG.md](../CHANGELOG.md) — Versionshistorie.
[info.json](../info.json) — Capabilities und Version.

## Häufige Fragen

**F: Brauche ich ein Konto?**
Nein. Keine Konten, keine Online-Prüfung, keine Telemetrie; Konfiguration im Klartext.

**F: Wo liegen die Plugins?**
Jedes Plugin liegt in `kore-plugin-<id>` und hängt nur von `kore/pkg/pluginapi` ab.

**F: Wo anfangen?**
Lies Protokoll und Methodentabelle in der README, dann die Capabilities in `info.json`.
