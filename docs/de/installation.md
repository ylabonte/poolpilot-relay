# Installation & Kopplung

*[🇬🇧 English](../en/installation.md) · 🇩🇪 Deutsch*

## Was du brauchst

- Einen kleinen, dauerhaft laufenden Computer mit **Linux und systemd** — ein
  Raspberry Pi ist die typische Wahl, aber jedes SBC oder jeder x86-Rechner
  funktioniert.
- Er muss im **selben lokalen Netzwerk** wie deine ProCon.IP- oder
  VIOLET-Steuerung sein.
- `curl` **oder** `wget` (Raspberry Pi OS bringt beides mit).

**Unterstützte CPU-Architekturen:** 64-Bit-ARM (`arm64`, z. B. Raspberry Pi
3/4/5), 32-Bit-ARM (`armv7`), 64-Bit-x86 (`amd64`), 32-Bit-x86 (`386`) und
`riscv64`. 32-Bit-ARMv6 (der ursprüngliche Pi / Pi Zero) wird **nicht**
unterstützt.

## Installieren

```bash
curl -fsSL https://get.poolpilot.eu/install.sh | bash
```

Du möchtest das Skript vor dem Ausführen lesen? Es liegt in diesem Repository —
hol es direkt von der Quelle, lies es, führe es dann aus:

```bash
curl -fsSLO https://raw.githubusercontent.com/ylabonte/poolpilot-relay/main/deploy/relay/install.sh
less install.sh && bash install.sh
```

### Was der Installer macht

Der Installer läuft **als dein normaler Benutzer** — außer bei den letzten
Installationsschritten — und zeigt dir genau, welche `sudo`-Schritte er ausführt,
*bevor* er nach deinem Passwort fragt. Kurz gesagt:

1. erkennt er deine CPU-Architektur und fragt die PoolPilot-Cloud, welche Version
   installiert werden soll;
2. lädt er die Version herunter und **prüft** sie (SHA-256-Prüfsumme, dazu eine
   `minisign`-Signatur, wenn das Tool verfügbar ist — er bietet die Installation
   an);
3. installiert er den Agenten nach `/usr/local/bin/poolpilot-relay`, schreibt
   eine Standardkonfiguration nach `/etc/poolpilot-relay/config` (nur wenn noch
   keine existiert) und installiert und startet den systemd-Dienst
   `poolpilot-relay`;
4. macht er das systemd-Journal dauerhaft (falls noch nicht), damit die Logs des
   Relays Neustarts überstehen;
5. installiert er bei update-fähigen Versionen zusätzlich den
   [Selbst-Update](self-update.md)-Helfer, damit sich das Gerät selbst aktuell
   hält; und
6. zeigt er am Ende den **Kopplungscode** dieses Geräts an.

Er installiert keine Telemetrie, keine Cron-Jobs und ändert keine
Shell-Profile. Ein erneuter Aufruf ist unbedenklich (idempotent) — so
[aktualisierst oder reparierst](troubleshooting.md) du auch ein Gerät.

## App mit dem Relay koppeln

Wenn die Installation fertig ist, läuft das Relay bereits. So verbindest du es
mit deinem Konto:

1. Öffne die **PoolPilot-App** auf einem Telefon, das im **selben Netzwerk** wie
   das Relay ist.
2. Die App findet das Relay automatisch. Folge den Anweisungen zum Koppeln.

Findet die App es nicht automatisch, zeige den Kopplungscode am Gerät an und
scanne ihn:

```bash
sudo poolpilot-relay show-pairing
```

Das zeigt einen QR-Code (und einen Link, den du in die App einfügen kannst) sowie
einen Fingerprint zur manuellen Eingabe. Der Befehl ist schreibgeschützt — er
ändert nie etwas. Details unter [Befehle](commands.md).

## Prüfen, dass es läuft

```bash
systemctl status poolpilot-relay      # läuft der Dienst?
journalctl -u poolpilot-relay -f      # Live-Logs
```

Kurz nach dem Start solltest du eine Zeile wie `agent starting version=…` sehen.

## Nächste Schritte

- [Konfiguration](configuration.md) — die Einstellungsdatei, falls du sie je
  brauchst.
- [Selbst-Update](self-update.md) — wie sich das Relay aktuell hält.
- [Fehlerbehebung](troubleshooting.md) — falls etwas nicht stimmt.
