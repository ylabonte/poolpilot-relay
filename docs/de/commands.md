# Befehle

*[🇬🇧 English](../en/commands.md) · 🇩🇪 Deutsch*

Der Installer legt ein paar Programme auf deinem Gerät ab. Im Alltag rufst du
keines davon von Hand auf — systemd startet das Relay für dich, und gesteuert
wird es über die PoolPilot-App. Diese Seite ist für die Fälle gedacht, in denen
du hinter die Kulissen schauen möchtest.

## Installierte Programme

| Pfad | Was es ist |
| --- | --- |
| `/usr/local/bin/poolpilot-relay` | Der **Agent** — das Relay selbst. Wird vom systemd-Dienst `poolpilot-relay` gestartet. Stellt außerdem die Hilfsbefehle `show-pairing` und `show-recovery` unten bereit. |
| `/usr/local/bin/poolpilot-relay-updater` | Der privilegierte **Selbst-Update-Helfer**. Wird bei update-fähigen Versionen installiert und automatisch von systemd gestartet, sobald ein Update bereitliegt — **du rufst ihn nie von Hand auf.** Siehe [Selbst-Update](self-update.md). |

Beide kommen mit systemd-Units in `/etc/systemd/system/`:
`poolpilot-relay.service` und (für das Selbst-Update)
`poolpilot-relay-updater.service` + `poolpilot-relay-updater.path`.

## `poolpilot-relay show-pairing`

Zeigt den **Kopplungscode** dieses Relays: einen QR-Code (einen Link, den die App
öffnet), denselben Link als Text zum Einfügen in die App und einen Fingerprint
zur manuellen Eingabe. Nutze ihn, wenn die App das Relay nicht automatisch
findet.

```bash
sudo poolpilot-relay show-pairing
```

`sudo` ist nötig, weil der Befehl die privaten Dateien des Agenten liest. Er ist
**strikt schreibgeschützt** — er startet, konfiguriert oder ändert das Relay nie
— und daher immer gefahrlos aufrufbar. Rufe ihn jederzeit erneut auf, um den Code
wieder anzuzeigen.

## `poolpilot-relay show-recovery`

Zeigt einen **einmaligen Wiederherstellungscode**, mit dem ein Telefon die
*Besitzer*-Rolle für den Haushalt dieses Relays zurückholen kann — zum Beispiel,
wenn du den Zugriff von allen gekoppelten Geräten verloren hast. Wer diesen Befehl
ausführen kann, hat bereits physischen/Root-Zugriff auf das Relay — genau darauf
beruht diese Wiederherstellung.

```bash
sudo poolpilot-relay show-recovery
```

Er zeigt einen QR-Code / Link und den Code an, sagt dir, wann der Code abläuft
(wenige Minuten), und erinnert daran, dass er einmalig ist. Er funktioniert nur,
wenn das Relay bereits bei der Cloud **registriert** ist (also schon einmal
gekoppelt war); bei einem brandneuen Relay koppelst du stattdessen normal mit
`show-pairing`. Wie `show-pairing` ist er schreibgeschützt.

## Den Dienst verwalten

Das Relay ist ein ganz normaler systemd-Dienst:

```bash
systemctl status poolpilot-relay           # läuft er? seit wann? letzte Log-Zeilen
journalctl -u poolpilot-relay -f           # den Live-Logs folgen
journalctl -u poolpilot-relay -b           # Logs seit dem letzten Start
sudo systemctl restart poolpilot-relay     # neu starten (z. B. nach Config-Änderung)
sudo systemctl stop poolpilot-relay        # stoppen
sudo systemctl disable --now poolpilot-relay   # stoppen und nicht beim Start hochfahren
```

Der Selbst-Update-Helfer loggt separat:

```bash
journalctl -u poolpilot-relay-updater      # was der Updater getan hat (und warum)
```

## Version und Hilfe

Der Agent versteht die beiden Schalter, die du erwartest:

```bash
poolpilot-relay version     # auch --version, -v  → gibt die Version aus, z. B. v0.1.0
poolpilot-relay help        # auch --help,    -h  → listet Befehle und Schalter
```

Jedes unbekannte Argument gibt dieselbe Hilfe auf die Standardfehlerausgabe aus
und beendet sich mit einem Fehlercode, statt versehentlich den Agenten zu starten.

Um gezielt die Version des **laufenden** Dienstes zu prüfen, zeigt die
PoolPilot-App sie ebenfalls, oder lies sie aus dem Start-Log:

```bash
journalctl -u poolpilot-relay | grep "agent starting"
```
