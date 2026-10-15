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
| `POLL_INTERVAL` | `60s` | Der **Standard** dafür, wie oft das Relay deine Steuerung ausliest (eine Go-Dauer, z. B. `30s`, `2m`). Kürzer heißt frischere Werte und etwas mehr Last. Ein in der App gesetzter Wert (siehe [Abfrageintervall](#abfrageintervall)) hat Vorrang. |
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

## Abfrageintervall

Der Besitzer des Haushalts kann direkt in der PoolPilot-App (**Einstellungen →
Relay**) festlegen, wie oft das Relay die Steuerung ausliest — zwischen
**3 Sekunden und 10 Minuten**. Ohne SSH und ohne Neustart: Das Relay speichert
den Wert, nutzt ihn ab der nächsten Abfrage und behält ihn über Neustarts und
Updates hinweg.

- **Vorrang:** Ein in der App gesetzter Wert schlägt `POLL_INTERVAL`. Solange
  keiner gesetzt ist, fragt das Relay im Takt von `POLL_INTERVAL` ab (bzw. alle
  60 s, wenn das nicht gesetzt ist).
- **Lebensdauer:** Der Wert gehört zum Relay, nicht zum Haushalt — wird der
  Haushalt vom Relay entfernt, bleibt er erhalten. Ein Werksreset löscht ihn.
- **Wann langsamer sinnvoll ist:** Manche Steuerungen und manche Hosting-Setups
  davor reagieren empfindlich auf häufige Anfragen; eine volumenbegrenzte
  Verbindung ist ein weiterer Grund. Ein langsamerer Takt schont beides.

### Auswirkung auf Alarme

Alarme werden **einmal pro Abfrage** ausgewertet, das Abfrageintervall bestimmt
also direkt, wie schnell sie reagieren:

- Ein Messwert-Alarm (und seine Entwarnung) wird erst ausgelöst, wenn der Wert
  für seine **Entprellung** an **aufeinanderfolgenden Abfragen** außerhalb (bzw.
  wieder innerhalb) des Bereichs liegt — standardmäßig 3. Die Verzögerung liegt
  also bei etwa *Entprellung × Abfrageintervall*: rund 3 Minuten beim Standard
  von 60 s, 30 Minuten bei 10 Minuten und 9 Sekunden bei 3 Sekunden.
- Der Alarm **„keine aktuellen Daten"** ist zeitbasiert (standardmäßig 90 Minuten
  ohne erfolgreiche Abfrage). Die Obergrenze von 10 Minuten hält selbst eine
  volle Entprellung deutlich innerhalb dieses Fensters, sodass langsames Abfragen
  allein nie wie ein Ausfall aussieht.
- Erinnerungen an einen anhaltenden Zustand (die Ruhezeit, standardmäßig
  6 Stunden) sind ebenfalls zeitbasiert und ändern sich nicht mit dem
  Abfrageintervall.

### Für App-Entwickler: die LAN-API

Über die LAN-API des Relays (und dieselben Routen durch den Fernzugriffs-Tunnel),
mit dem Bearer-Token eines gekoppelten Geräts (den Wert zu ändern ist eine
Einstellung für den Besitzer):

- `GET /v1/poll-interval` → `{"seconds":60,"default_seconds":60,"overridden":false,"min_seconds":3,"max_seconds":600}`
  — `seconds` ist der aktuell geltende Takt, `default_seconds` der
  `POLL_INTERVAL`-Rückfallwert (der außerhalb der Grenzen liegen kann).
- `PUT /v1/poll-interval` mit `{"seconds":120}` → `200` und derselbe Body.
  Ganze Sekunden von 3 bis 600; alles andere ergibt
  `400 {"error":"poll_interval_out_of_range","min_seconds":3,"max_seconds":600}`,
  ein fehlender oder nicht ganzzahliger Wert `400 {"error":"bad_json"}`.
- Das nicht authentifizierte `GET /v1/info` meldet den geltenden Takt als
  `poll_interval_seconds`. Dieses Feld ist das **einzige** Fähigkeitssignal: Ein
  Relay, das die Einstellung noch nicht kennt, lässt es weg. Werte ein `404` auf
  `/v1/poll-interval` nicht als „älteres Relay" — über den Fernzugriffs-Tunnel
  kann ein `404` auch heißen, dass das Relay gerade nicht erreichbar ist.

## Erweiterte Einstellungen

Ein paar weitere Variablen existieren vor allem für Entwicklung, Tests und
Support. Auf einer normalen Installation zu Hause brauchst du sie nicht, und sie
zu ändern kann das Relay lahmlegen — setze sie nur, wenn du weißt, warum:

`TUNNEL_LISTEN`, `CTRL_FILTER_LISTEN`, `STATE_PATH`, `MDNS_DISABLED`,
`MDNS_VERBOSE_LOGS`, `FRPS_AUTH_TOKEN`, `PAIR_URL_BASE`, `REPO_DL_BASE` — dazu die
nur im Container genutzten `RESTART_ON_RESET` und `HA_OPTIONS`, die das Image der
[Home-Assistant-App](home-assistant.md) für dich setzt (auf einer
systemd-Installation lässt du sie ungesetzt).

`MDNS_VERBOSE_LOGS=1` aktiviert die INFO-Logs der Discovery-Bibliothek wieder
(siehe [Fehlersuche](troubleshooting.md#wiederkehrende-mdns-rfc6762-logzeilen));
standardmäßig sind sie aus.
