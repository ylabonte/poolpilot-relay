# Konfiguration

*[🇬🇧 English](../en/configuration.md) · 🇩🇪 Deutsch*

Das Relay wird über eine einzige Datei konfiguriert:

```
/etc/poolpilot-relay/config
```

Es ist eine einfache `KEY=value`-Datei (ein systemd-*EnvironmentFile*). Der
Installer schreibt eine sinnvolle Standardkonfiguration, und **die meisten müssen
sie nie anfassen** — die spannenden Einstellungen liegen in der PoolPilot-App.
Ändere sie nur, wenn du einen konkreten Grund hast.

Nach jeder Änderung den Dienst neu starten:

```bash
sudo systemctl restart poolpilot-relay
```

## Einstellungen, die du nutzen könntest

| Einstellung | Standard | Was sie bewirkt |
| --- | --- | --- |
| `CLOUD_BASE_URL` | `https://api.poolpilot.eu` | Die PoolPilot-Cloud, mit der das Relay spricht. Wird vom Installer geschrieben — lass sie so, außer der Support sagt etwas anderes. |
| `LAN_LISTEN` | `:8443` | Adresse/Port, unter dem das Relay die App über dein LAN bedient. Ändere den Port nur, wenn `8443` mit etwas anderem auf dem Gerät kollidiert. |
| `POLL_INTERVAL` | `60s` | Wie oft das Relay deine Steuerung ausliest (eine Go-Dauer, z. B. `30s`, `2m`). Kürzer heißt frischere Werte und etwas mehr Last. |
| `UPDATE_DISABLED` | *(nicht gesetzt)* | Auf `1` setzen, um das **automatische Selbst-Update auf diesem Gerät vollständig abzuschalten**. Siehe [Selbst-Update](self-update.md), was das bedeutet (und warum der Schalter in der App meist die bessere Wahl ist). |

Die Standardkonfiguration, die der Installer schreibt, sieht so aus:

```bash
# PoolPilot Relay agent configuration (systemd EnvironmentFile).
CLOUD_BASE_URL=https://api.poolpilot.eu
# LAN_LISTEN=:8443
# TUNNEL_LISTEN=127.0.0.1:8480
# POLL_INTERVAL=60s
# Self-update runs automatically at night; manage it from the PoolPilot app, or
# set UPDATE_DISABLED=1 here to opt this device out entirely.
# UPDATE_DISABLED=1
```

Zeilen, die mit `#` beginnen, sind Kommentare (inaktiv). Um eine zu aktivieren,
entferne das `#` und setze den Wert.

## Erweiterte Einstellungen

Ein paar weitere Variablen existieren vor allem für Entwicklung, Tests und
Support. Auf einer normalen Installation zu Hause brauchst du sie nicht, und sie
zu ändern kann das Relay lahmlegen — setze sie nur, wenn du weißt, warum:

`TUNNEL_LISTEN`, `CTRL_FILTER_LISTEN`, `STATE_PATH`, `MDNS_DISABLED`,
`FRPS_AUTH_TOKEN`, `PAIR_URL_BASE`, `REPO_DL_BASE`.
