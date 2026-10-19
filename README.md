<a name="deutsch"></a>

<p align="center">
  <img src="poolpilot_relay/logo.png" alt="PoolPilot" width="112">
</p>

<h1 align="center">PoolPilot Relay</h1>

<p align="center">
  <b>🇩🇪 Deutsch</b> · <a href="#english">🇬🇧 English version below</a>
</p>

Ein kleines Programm für zu Hause, das deine Poolsteuerung mit der
[PoolPilot](https://poolpilot.eu)-App verbindet — auch von unterwegs.

## Was es für deinen Pool tut

| Was du davon hast | ProCon.IP | BADU Blue | VIOLET |
| --- | --- | --- | --- |
| Pool zu Hause im WLAN steuern | geht ohne Relay | geht ohne Relay | geht ohne Relay |
| Pool von unterwegs steuern | ✓ mit Relay | kann sie schon | kann sie schon |
| Push aufs Handy, wenn etwas nicht stimmt | ✓ mit Relay | ✓ mit Relay | kann sie schon\* |
| Pool mit der Familie teilen — inkl. Pro-Funktionen, bis zu 10 Geräte, iPhone und Android gemischt | ✓ mit Relay | ✓ mit Relay | ✓ mit Relay |

\* Kostenlos in PoolPilot — das Relay kann es optional übernehmen.

**Kurz gesagt:**

- **ProCon.IP:** Das Relay macht sie fit für unterwegs.
- **BADU Blue:** Das Relay bringt Push in PoolPilot.
- **VIOLET:** Das Relay brauchst du nur zum Teilen.

**Was brauchst du dafür?** Ein Gerät, das bei dir zu Hause immer läuft (z. B. ein
Raspberry Pi oder dein Home Assistant) und PoolPilot Pro.

## Installation

**Auf einem Raspberry Pi oder einem anderen Linux-Rechner:**

```bash
curl -fsSL https://get.poolpilot.eu/install.sh | bash
```

Danach öffnest du die PoolPilot-App im selben WLAN und koppelst das Relay. Lieber
erst lesen, was du ausführst? Der Installer liegt offen in diesem Repo:
[`deploy/relay/install.sh`](deploy/relay/install.sh).

**Mit Home Assistant:** Füge dieses Repo unter **Einstellungen → Add-ons →
Add-on-Store → ⋮ → Repositories** hinzu und installiere **PoolPilot Relay**:

```
https://github.com/ylabonte/poolpilot-relay
```

Danach aktualisiert sich das Relay [von selbst](docs/de/self-update.md).

## Dokumentation

| Seite | Inhalt |
| --- | --- |
| [Installation & Kopplung](docs/de/installation.md) | Voraussetzungen, der Installer, erste Kopplung |
| [Home-Assistant-App](docs/de/home-assistant.md) | Das Relay als Home-Assistant-App statt auf einem Pi betreiben |
| [Befehle](docs/de/commands.md) | Die Programme, die das Relay installiert, und wie du sie aufrufst |
| [Konfiguration](docs/de/configuration.md) | Die Datei `/etc/poolpilot-relay/config` |
| [Selbst-Update](docs/de/self-update.md) | Wie automatische Updates funktionieren und wie du sie steuerst |
| [Fehlerbehebung](docs/de/troubleshooting.md) | Logs, neu koppeln, Wiederherstellung, neu installieren, entfernen |

Fragen oder ein Problem? [Issue anlegen](https://github.com/ylabonte/poolpilot-relay/issues)
· Website: [poolpilot.eu](https://poolpilot.eu)

## Für Entwickler

Open Source (MIT), in Go geschrieben. Die Binaries erscheinen als signierte
[GitHub Releases](https://github.com/ylabonte/poolpilot-relay/releases), das
Home-Assistant-Image auf `ghcr.io`. Bauen und testen mit `go build ./...`,
`go test ./...` und `go vet ./...` — mehr dazu im
[englischen Teil](#for-developers).

---

<a name="english"></a>

<p align="center">
  <a href="#deutsch">🇩🇪 Deutsche Version oben</a> · <b>🇬🇧 English</b>
</p>

A small program for your home that connects your pool controller to the
[PoolPilot](https://poolpilot.eu) app — even while you're away.

## What it does for your pool

| What you get | ProCon.IP | BADU Blue | VIOLET |
| --- | --- | --- | --- |
| Control your pool at home over Wi‑Fi | works without the relay | works without the relay | works without the relay |
| Control your pool while you're away | ✓ with the relay | built in | built in |
| Push to your phone when something's off | ✓ with the relay | ✓ with the relay | built in\* |
| Share your pool with your family — Pro features included, up to 10 devices, iPhone and Android mixed | ✓ with the relay | ✓ with the relay | ✓ with the relay |

\* Free in PoolPilot — the relay can optionally take it over.

**In short:**

- **ProCon.IP:** the relay gets it ready for when you're away.
- **BADU Blue:** the relay brings push to PoolPilot.
- **VIOLET:** you only need the relay for sharing.

**What do you need?** A device that's always on at home (e.g. a Raspberry Pi or
your Home Assistant) and PoolPilot Pro.

## Install

**On a Raspberry Pi or any other Linux machine:**

```bash
curl -fsSL https://get.poolpilot.eu/install.sh | bash
```

Then open the PoolPilot app on the same Wi‑Fi and pair the relay. Prefer to read
what you run first? The installer is right here in this repo:
[`deploy/relay/install.sh`](deploy/relay/install.sh).

**With Home Assistant:** add this repo under **Settings → Add-ons → Add-on store
→ ⋮ → Repositories** and install **PoolPilot Relay**:

```
https://github.com/ylabonte/poolpilot-relay
```

From then on the relay [updates itself](docs/en/self-update.md).

## Documentation

| Page | What it covers |
| --- | --- |
| [Installation & pairing](docs/en/installation.md) | Requirements, the installer, first pairing |
| [Home Assistant app](docs/en/home-assistant.md) | Running the relay as a Home Assistant app instead of on a Pi |
| [Commands](docs/en/commands.md) | The programs the relay installs and how to run them |
| [Configuration](docs/en/configuration.md) | The `/etc/poolpilot-relay/config` file |
| [Self-update](docs/en/self-update.md) | How automatic updates work and how to control them |
| [Troubleshooting](docs/en/troubleshooting.md) | Logs, re-pairing, recovery, reinstalling, removing |

Questions or a problem? [Open an issue](https://github.com/ylabonte/poolpilot-relay/issues)
· Website: [poolpilot.eu](https://poolpilot.eu)

## For developers

The edge agent is one Go module at the repo root. It talks to the controller on
the local network and reaches the PoolPilot cloud through an outbound tunnel
when you're away.

```bash
go build ./...     # compile every package
go test ./...      # unit + wire/measurement parity tests
go vet ./...       # static checks
```

- **Releases:** binaries are published as signed
  [GitHub Releases](https://github.com/ylabonte/poolpilot-relay/releases). The
  installer checks each download's SHA-256 checksum, plus a `minisign` signature
  when `minisign` is installed. The PoolPilot control plane decides which version
  installs.
- **Home Assistant app:** lives in [`poolpilot_relay/`](poolpilot_relay/). Its
  multi-arch image is published to `ghcr.io` on every release tag by
  [`addon-release.yml`](.github/workflows/addon-release.yml).
- **Self-update testing:** a manual end-to-end harness per architecture (QEMU or
  cloud-init VMs on amd64/386/armv7/riscv64) lives in
  [test/self-update-e2e/](test/self-update-e2e/README.md).

License: [MIT](LICENSE) · see also [NOTICE](NOTICE).
