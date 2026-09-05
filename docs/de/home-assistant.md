# PoolPilot Relay als Home-Assistant-App

*[🇬🇧 English](../en/home-assistant.md) · 🇩🇪 Deutsch*

Wenn bei dir ohnehin **Home Assistant** im Poolnetz läuft, kannst du das Relay
**als Home-Assistant-App** betreiben (bis 2026.2 hießen diese „Add-ons") — statt
auf einem separaten Raspberry Pi. Es ist derselbe Edge-Agent — gleiche Kopplung,
gleiche Funktionen — nur in einem Container, den der Home-Assistant-Supervisor
für dich verwaltet.

> Home-Assistant-Apps brauchen **Home Assistant OS** oder **Supervised** (sie
> setzen den Supervisor voraus). Bei Home Assistant Container oder Core nimm
> stattdessen den [normalen Installer](installation.md) auf einem kleinen
> Dauerläufer.

## Voraussetzungen

- Ein **PoolPilot-Konto** und die iOS- oder Android-App — das Relay wird aus der
  App mit deinem Haushalt gekoppelt.
- Eine **ProCon.IP**- oder **VIOLET**-Steuerung, erreichbar in deinem lokalen
  Netzwerk.
- Home Assistant OS oder Supervised auf einem 64-Bit-System (`aarch64` oder
  `amd64`).

## Installation

1. Öffne in Home Assistant **Einstellungen → Add-ons → Add-on-Store**.
2. Öffne das **⋮**-Menü (oben rechts) → **Repositories** und füge hinzu:

   ```
   https://github.com/ylabonte/poolpilot-relay
   ```

3. Suche im Store **PoolPilot Relay**, öffne es und wähle **Installieren**.
4. **Starte** die App, öffne dann die PoolPilot-Handy-App und kopple dieses
   Relay. Das Relay meldet sich per mDNS im LAN, die App findet es also
   automatisch und führt dich durch die Kopplung — die Konsole der App musst du
   nicht anfassen.

## Konfiguration

Es gibt nichts zu konfigurieren. Die App spricht mit der PoolPilot-Cloud und
legt ihre Identität auf ihrem eigenen persistenten Volume ab; gekoppelt wird aus
der Handy-App.

## Daten & Backups

Deine Kopplung, die Controller-Registrierung und die TLS-Identität des Relays
liegen auf dem persistenten `/data`-Volume der App:

- Sie überstehen **Neustarts und App-Updates** automatisch.
- Sie sind in **Home-Assistant-Backups** enthalten.
- **Ein Deinstallieren der App löscht `/data`.** Eine Neuinstallation kommt dann
  als *neues* Relay zurück und jedes gekoppelte Handy muss neu koppeln — mach
  vorher ein Backup oder rechne mit erneutem Koppeln.

Die TLS-Identität in `/data` ist der Pin, dem deine gekoppelten Handys vertrauen;
sie zu verlieren (Deinstallation oder Neuinstallation ohne Backup-Wiederher­stel­lung)
erzwingt das erneute Koppeln.

## Updates

Den Update-Ablauf steuert hier der Home-Assistant-Supervisor, das Relay
aktualisiert sich in der App also **nicht** selbst. Neue Relay-Versionen
erscheinen im Add-on-Store wie bei jeder anderen App.

## Netzwerk

Die App läuft im **Host-Netzwerk**, damit sie sich per mDNS ankündigen, deinen
Controller im LAN erreichen und ihre gepinnte Kopplungs-API auf Port **8443** für
die Handy-Apps bereitstellen kann. Achte darauf, dass Port 8443 auf dem Host
frei ist.

## Wiederherstellung / Fortgeschritten

Die Befehle `poolpilot-relay show-pairing` und `show-recovery` zeigen den
Kopplungslink und den Owner-Wiederherstellungscode. Im App-Image gibt es keine
Shell und Home Assistant hat keinen „In-App-Exec"-Knopf — führe sie also aus
einer Host-Shell aus, z. B. über das **SSH & Web Terminal**-Add-on mit
ausgeschaltetem Protection Mode. Der Containername enthält die Repository-ID,
matche ihn daher per Teilstring:

```sh
docker exec "$(docker ps --filter name=poolpilot_relay --format '{{.Names}}')" \
  /usr/local/bin/poolpilot-relay show-pairing
```

## Fehlerbehebung

- **Die Handy-App findet das Relay nicht** — prüfe, dass die App *gestartet* ist,
  dass Home Assistant und dein Handy im selben LAN sind und dass Port 8443 frei
  ist.
- **Logs** — öffne den **Log**-Tab der App.

---

*Läuft bei dir stattdessen ein Raspberry Pi? Siehe [Installation & Kopplung](installation.md).*
&nbsp;· 🇬🇧 This page is also available [in English](../en/home-assistant.md).
