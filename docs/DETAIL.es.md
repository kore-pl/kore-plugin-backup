> 状态: borrador (草稿) · 归属: wave1 · 更新: 2026-11-25 · 里程碑: M5

# kore-plugin-backup

Este repositorio implementa el plugin de copias de seguridad de kore (id `backup`). Empaquetar/restaurar/verificar usan solo `archive/tar` + `compress/gzip` de Go, evitando inyección shell y diferencias de tar entre distribuciones. La transferencia externa la hacen herramientas de sincronización; el plugin solo guarda referencias descargables y rutas del host.

语言：[简体中文](DETAIL.md) · [English](DETAIL.en.md) · [Español](DETAIL.es.md) · [Français](DETAIL.fr.md) · [Deutsch](DETAIL.de.md) · [Русский](DETAIL.ru.md) · [Português](DETAIL.pt.md)

## Qué hace

- **Protocolo**: `kore.plugin.v1`, `lifecycle=lazy`, `depth_level=production`, capacidad `system:admin`.
- **Métodos**: `backup.create` / `list` / `restore` / `delete` / `verify` / `retention` / `stats` / `download` / `jobs` / `schedule.set` / `schedule.get`; también se aceptan verbos simples.
- **Integridad**: comprobaciones `exists` / `size` / `sha256` / `readable` / `entries` se informan una por una; los paquetes dañados se marcan, nunca se ignoran.
- **Almacenamiento local**: los paquetes quedan en un directorio local (no S3) y los mueven herramientas externas; `download` admite lectura por bloques.
- **Privilegio mínimo**: `info.json.capabilities` es toda la autorización; las escrituras fallan cerradas.

## Inicio rápido

```bash
make check    # gofmt → vet → test
make build    # compilar el binario
make dist     # generar el tarball
```

El plugin lo inicia el shell de kore bajo demanda; `info.json` declara capacidades y `requires_kore`.

## Seguridad y límites

Informa de vulnerabilidades por [SECURITY.md](../SECURITY.md)

## Mapa de documentación

[SECURITY.md](../SECURITY.md) — proceso de divulgación.
[CONTRIBUTING.md](../CONTRIBUTING.md) — convenciones de commit y versión.
[CHANGELOG.md](../CHANGELOG.md) — historial.
[info.json](../info.json) — capacidades y versión.

## Preguntas frecuentes

**P: ¿Necesito una cuenta?**
No. Sin cuentas, verificación en línea ni telemetría oculta; configuración en texto plano.

**P: ¿Dónde están los plugins?**
Cada plugin vive en su propio repositorio `kore-plugin-<id>` y solo depende de `kore/pkg/pluginapi`.

**P: ¿Por dónde empezar?**
Lee el protocolo y la tabla de métodos del README y luego las capacidades en `info.json`.
