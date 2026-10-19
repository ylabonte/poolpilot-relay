# PoolPilot Relay

*[🇬🇧 English](../en/index.md) · 🇩🇪 Deutsch*

Das PoolPilot Relay ist ein kleines Programm für zu Hause, das deine Poolsteuerung
mit der PoolPilot-App verbindet — auch von unterwegs. Was es dir bringt, hängt
von deiner Steuerung ab:

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
Raspberry Pi oder dein [Home Assistant](home-assistant.md)) und PoolPilot Pro.

## So funktioniert's

Das Relay ist ein kleines Hintergrundprogramm (der **Edge-Agent**), das
auf einem Gerät bei dir zu Hause läuft — typischerweise auf einem Raspberry Pi
oder einem ähnlichen kleinen Computer. Es spricht über dein lokales Netzwerk mit
deiner **ProCon.IP**-, **VIOLET**- oder **BADU-Blue**-Poolsteuerung und
verbindet sie mit den PoolPilot-Apps:

- **zu Hause** über dein WLAN und
- **unterwegs** über den sicheren Tunnel zur PoolPilot-Cloud.

Einmal installiert und gekoppelt, kümmert sich das Relay um sich selbst: Es hält
deine Steuerung verbunden, verschickt die in der App eingestellten Alarme und
**aktualisiert sich automatisch**. Am Gerät selbst gibt es normalerweise nichts
zu tun. Die einzige Ausnahme ist die [Home-Assistant-App](home-assistant.md):
Dort kommen Updates stattdessen über den Add-on-Store von Home Assistant.

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
| [Sicherheit & Datenschutz](security.md) | Was wo gespeichert ist, Netzwerk und Ports, der Tunnel, signierte Versionen |

> 🇬🇧 This documentation is also available **[in English](../en/index.md)**.

## Hilfe

- Problem melden: <https://github.com/ylabonte/poolpilot-relay/issues>
- Website: <https://poolpilot.eu>
