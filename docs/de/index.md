# PoolPilot Relay

*[🇬🇧 English](../en/index.md) · 🇩🇪 Deutsch*

Das PoolPilot Relay ist ein kleines Hintergrundprogramm (der **Edge-Agent**), das
auf einem Gerät bei dir zu Hause läuft — typischerweise auf einem Raspberry Pi
oder einem ähnlichen kleinen Computer. Es spricht über dein lokales Netzwerk mit
deiner **ProCon.IP**- oder **VIOLET**-Poolsteuerung und verbindet sie mit den
PoolPilot-Apps:

- **zu Hause** erreicht die App das Relay direkt über dein WLAN/LAN, und
- **unterwegs** über einen sicheren Tunnel zur PoolPilot-Cloud.

Einmal installiert und gekoppelt, kümmert sich das Relay um sich selbst: Es hält
deine Steuerung verbunden, verschickt die in der App eingestellten Alarme und
**aktualisiert sich automatisch**. Am Gerät selbst gibt es normalerweise nichts
zu tun.

## Installation

```bash
curl -fsSL https://get.poolpilot.eu/install.sh | bash
```

Öffne danach die PoolPilot-App im selben Netzwerk, um zu koppeln. Siehe
**[Installation & Kopplung](installation.md)** für Voraussetzungen, was der
Installer macht und wie du den Kopplungscode von Hand anzeigst.

## Dokumentation

| Seite | Inhalt |
| --- | --- |
| [Installation & Kopplung](installation.md) | Voraussetzungen, der Installer, erste Kopplung |
| [Befehle](commands.md) | Die Programme, die das Relay installiert, und wie du sie aufrufst |
| [Konfiguration](configuration.md) | Die Datei `/etc/poolpilot-relay/config` |
| [Selbst-Update](self-update.md) | Wie automatische Updates funktionieren und wie du sie steuerst |
| [Fehlerbehebung](troubleshooting.md) | Logs, neu koppeln, Wiederherstellung, neu installieren, entfernen |
| [Home-Assistant-App](home-assistant.md) | Das Relay als Home-Assistant-App statt auf einem Pi betreiben |

> 🇬🇧 This documentation is also available **[in English](../en/index.md)**.

## Hilfe

- Problem melden: <https://github.com/ylabonte/poolpilot-relay/issues>
- Website: <https://poolpilot.eu>
