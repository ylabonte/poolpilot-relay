# Fehlerbehebung

*[🇬🇧 English](../en/troubleshooting.md) · 🇩🇪 Deutsch*

Die meisten Probleme laufen auf eines hinaus: Das Relay läuft nicht, Telefon und
Relay sind nicht im selben Netzwerk, oder das in der App gespeicherte Vertrauen
zum Relay ist veraltet. Fang mit den Logs an.

## In die Logs schauen

```bash
systemctl status poolpilot-relay        # läuft er? fehlgeschlagen? seit wann?
journalctl -u poolpilot-relay -f        # Live-Logs; mit Strg-C beenden
journalctl -u poolpilot-relay -b        # alles seit dem letzten Start
```

Dank des Installers überstehen diese Logs Neustarts. Der Selbst-Update-Helfer
loggt separat:

```bash
journalctl -u poolpilot-relay-updater
```

## Die App erreicht das Relay nicht

1. **Läuft es?** `systemctl status poolpilot-relay` sollte `active (running)`
   zeigen. Falls nicht: `sudo systemctl restart poolpilot-relay` und die Logs
   prüfen.
2. **Selbes Netzwerk?** Zu Hause findet die App das Relay über dein LAN. Stelle
   sicher, dass das Telefon im selben WLAN ist (nicht im Gast-WLAN oder auf
   Mobilfunk).
3. **Meldet „nicht erreichbar“ nach einem Werksreset?** Ein Werksreset gibt dem
   Relay eine brandneue Identität, die nicht mehr zu dem passt, was sich deine
   App gemerkt hat — deshalb kann die App es als *nicht erreichbar* melden,
   obwohl alles in Ordnung ist. Behebung: **das Relay in der App entfernen und
   erneut koppeln** (siehe unten).

## Relay neu koppeln

Zeige den Kopplungscode am Gerät an und koppel aus der App:

```bash
sudo poolpilot-relay show-pairing
```

Scanne den QR-Code mit deinem Telefon oder füge den ausgegebenen Link in die App
ein. Der Befehl ist schreibgeschützt und jederzeit gefahrlos aufrufbar.

## Besitzer-Zugriff verloren

Wenn kein Telefon mehr als Haushalts-Besitzer agieren kann, erzeuge am Gerät
einen einmaligen Wiederherstellungscode und scanne ihn mit dem Telefon, das der
Besitzer werden soll:

```bash
sudo poolpilot-relay show-recovery
```

Der Code läuft nach wenigen Minuten ab und ist einmalig. Er funktioniert nur auf
einem Relay, das schon einmal gekoppelt war. Siehe
[Befehle](commands.md#poolpilot-relay-show-recovery).

## Ein festhängendes oder fehlgeschlagenes Update

Das Selbst-Update rollt von selbst zurück, wenn eine neue Version nicht gesund
hochkommt — dein Relay fährt also weiter die Version, die es hatte. Um zu sehen,
was passiert ist:

```bash
journalctl -u poolpilot-relay-updater   # Ablehnungs- / Rollback-Gründe stehen hier
```

Um eine saubere Neuinstallation der aktuellen Version zu erzwingen, führe den
Installer erneut aus — er ist idempotent:

```bash
curl -fsSL https://get.poolpilot.eu/install.sh | bash
```

## Werksreset

Du kannst ein Relay aus der PoolPilot-App auf Werkseinstellungen zurücksetzen.
Das löscht Kopplung und Identität, und der Agent startet mit einer frischen neu
(systemd fährt ihn sofort wieder hoch). Danach ist das Relay entkoppelt —
[koppel es neu](#relay-neu-koppeln), als wäre es neu. Weil sich die Identität
geändert hat, entferne zuerst den alten Relay-Eintrag in der App, falls er noch
gelistet ist.

## Aktualisieren oder neu installieren

Den Installer-Einzeiler erneut auszuführen bringt ein Gerät auf die aktuelle
Version und repariert eine kaputte Installation. Deine bestehende Konfiguration
und Kopplung bleiben erhalten:

```bash
curl -fsSL https://get.poolpilot.eu/install.sh | bash
```

## Relay entfernen

So deinstallierst du vollständig:

```bash
# Dienst (und den Selbst-Update-Watcher, falls vorhanden) stoppen und deaktivieren
sudo systemctl disable --now poolpilot-relay
sudo systemctl disable --now poolpilot-relay-updater.path 2>/dev/null || true

# Programme, systemd-Units, Konfiguration und Zustand entfernen
sudo rm -f /usr/local/bin/poolpilot-relay /usr/local/bin/poolpilot-relay-updater
sudo rm -f /etc/systemd/system/poolpilot-relay.service \
           /etc/systemd/system/poolpilot-relay-updater.service \
           /etc/systemd/system/poolpilot-relay-updater.path
sudo rm -rf /etc/poolpilot-relay /var/lib/poolpilot-relay /var/lib/poolpilot-relay-updater
sudo systemctl daemon-reload
```

Der Installer hat außerdem das systemd-Journal dauerhaft gemacht. Um auch das
rückgängig zu machen (optional):

```bash
sudo rm -f /etc/systemd/journald.conf.d/95-poolpilot-persistent.conf
sudo systemctl restart systemd-journald
```

## Immer noch hängen?

Öffne ein Issue mit deinen Logs (schwärze alles Sensible):
<https://github.com/ylabonte/poolpilot-relay/issues>
