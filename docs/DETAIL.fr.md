> 状态: brouillon (草稿) · 归属: wave1 · 更新: 2026-11-25 · 里程碑: M5

# kore-plugin-backup

Ce dépôt implémente le plugin de sauvegarde de kore (id `backup`). Archiver/restaurer/vérifier utilise uniquement `archive/tar` + `compress/gzip` de Go, évitant l’injection shell et les différences de tar entre distributions. Le transfert hors site est confié à des outils de synchronisation externes ; le plugin ne conserve que des références téléchargeables et des chemins hôtes.

语言：[简体中文](DETAIL.md) · [English](DETAIL.en.md) · [Español](DETAIL.es.md) · [Français](DETAIL.fr.md) · [Deutsch](DETAIL.de.md) · [Русский](DETAIL.ru.md) · [Português](DETAIL.pt.md)

## Ce que fait le projet

- **Protocole** : `kore.plugin.v1`, `lifecycle=lazy`, `depth_level=production`, capacité `system:admin`.
- **Méthodes** : `backup.create` / `list` / `restore` / `delete` / `verify` / `retention` / `stats` / `download` / `jobs` / `schedule.set` / `schedule.get` ; verbes nus acceptés.
- **Intégrité** : les contrôles `exists` / `size` / `sha256` / `readable` / `entries` sont rapportés un par un ; les archives corrompues sont signalées, jamais ignorées.
- **Stockage local** : les archives restent dans un répertoire local (pas S3) et sont déplacées par des outils externes ; `download` prend en charge la lecture par blocs.
- **Moindre privilège** : `info.json.capabilities` est la surface d’autorisation ; les écritures échouent fermées.

## Démarrage rapide

```bash
make check    # gofmt → vet → test
make build    # compiler le binaire
make dist     # produire le tarball
```

Le plugin est lancé à la demande par le shell kore ; `info.json` déclare les capacités et `requires_kore`.

## Sécurité et limites

Signalez les vulnérabilités via [SECURITY.md](../SECURITY.md)

## Plan de la documentation

[SECURITY.md](../SECURITY.md) — procédure de divulgation.
[CONTRIBUTING.md](../CONTRIBUTING.md) — conventions de commit et version.
[CHANGELOG.md](../CHANGELOG.md) — historique.
[info.json](../info.json) — capacités et version.

## Questions fréquentes

**Q : Faut-il un compte ?**
Non. Pas de compte, de vérification en ligne ni de télémétrie cachée ; configuration en texte brut.

**Q : Où sont les plugins ?**
Chaque plugin a son propre dépôt `kore-plugin-<id>` et ne dépend que de `kore/pkg/pluginapi`.

**Q : Par où commencer ?**
Lisez le protocole et le tableau des méthodes du README, puis les capacités dans `info.json`.
