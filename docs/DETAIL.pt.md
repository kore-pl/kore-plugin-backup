> 状态: rascunho (草稿) · 归属: wave1 · 更新: 2026-11-25 · 里程碑: M5

# kore-plugin-backup

Este repositório implementa o plugin de backup do kore (id `backup`). Empacotar/restaurar/verificar usa apenas `archive/tar` + `compress/gzip` do Go, evitando injeção de shell e diferenças de tar entre distribuições. A transferência externa fica com ferramentas de sincronização; o plugin guarda apenas referências para download e caminhos do host.

语言：[简体中文](DETAIL.md) · [English](DETAIL.en.md) · [Español](DETAIL.es.md) · [Français](DETAIL.fr.md) · [Deutsch](DETAIL.de.md) · [Русский](DETAIL.ru.md) · [Português](DETAIL.pt.md)

## O que faz

- **Protocolo**: `kore.plugin.v1`, `lifecycle=lazy`, `depth_level=production`, capacidade `system:admin`.
- **Métodos**: `backup.create` / `list` / `restore` / `delete` / `verify` / `retention` / `stats` / `download` / `jobs` / `schedule.set` / `schedule.get`; verbos simples também aceitos.
- **Integridade**: verificações `exists` / `size` / `sha256` / `readable` / `entries` relatadas item a item; arquivos corrompidos são marcados, nunca ignorados.
- **Armazenamento local**: arquivos ficam em diretório local (não S3) e são movidos por ferramentas externas; `download` suporta leitura em blocos.
- **Menor privilégio**: `info.json.capabilities` é toda a superfície de autorização; gravações fail-closed.

## Início rápido

```bash
make check    # gofmt → vet → test
make build    # compilar o binário
make dist     # gerar o tarball
```

O plugin é iniciado sob demanda pelo shell do kore; `info.json` declara capacidades e `requires_kore`.

## Segurança e limites

Relate vulnerabilidades por [SECURITY.md](../SECURITY.md)

## Mapa da documentação

[SECURITY.md](../SECURITY.md) — processo de divulgação.
[CONTRIBUTING.md](../CONTRIBUTING.md) — convenções de commit e versão.
[CHANGELOG.md](../CHANGELOG.md) — histórico.
[info.json](../info.json) — capacidades e versão.

## Perguntas frequentes

**P: Preciso de uma conta?**
Não. Sem contas, verificação online ou telemetria oculta; configuração em texto simples.

**P: Onde ficam os plugins?**
Cada plugin fica em `kore-plugin-<id>` e depende apenas de `kore/pkg/pluginapi`.

**P: Por onde começar?**
Leia o protocolo e a tabela de métodos no README e depois as capacidades em `info.json`.
