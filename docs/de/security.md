# Sicherheit & Datenschutz

*[🇬🇧 English](../en/security.md) · 🇩🇪 Deutsch*

Das Relay steht in deinem Heimnetz, direkt neben deiner Poolsteuerung — da ist
die Frage berechtigt, was es speichert, mit wem es spricht und wer es erreichen
kann. Diese Seite beantwortet das in einfachen Worten.

## Was wo gespeichert ist

**Auf dem Gerät.** Alles, was das Relay über einen Neustart hinweg braucht,
liegt in einer einzigen Datei: `/var/lib/poolpilot-relay/state.json` bei einer
Linux-Installation bzw. `/data/state.json` in der
[Home-Assistant-App](home-assistant.md). Das Wichtigste darin:

| Was | Hinweise |
| --- | --- |
| Die TLS-Identität des Relays | Sein Schlüsselpaar. Daraus ergibt sich der Fingerabdruck, den du beim Koppeln siehst. |
| Gekoppelte Telefone | Ein Name, die Haushaltsrolle (Besitzer oder Mitglied) und ein **Hash** des Zugangstokens jedes Telefons, nie das Token selbst. |
| Deine Poolsteuerungen | Adresse, Name und Typ — und **Benutzername und Passwort** der Steuerung. |
| Alarmregeln | Was einen Push auslösen soll, dazu eine kurze Warteschlange noch nicht zugestellter Alarme. |
| Tunnel-Zugangsdaten | Was das Relay braucht, um sich am PoolPilot-Tunnelserver anzumelden. |
| Einstellungen | Der Schalter für automatische Updates und das in der App gesetzte Abfrageintervall. |

**Benutzername und Passwort deiner Poolsteuerung bleiben auf dem Gerät.** Das
Relay schickt sie nie an die PoolPilot-Cloud und gibt sie auch über seine eigene
API nie heraus. Meldet das Relay eine Steuerung bei der Cloud an, schickt es nur
ihren Typ, ihre lokale Adresse und ihren Namen.

Die Datei ist nicht verschlüsselt. Bei einer Linux-Installation können sie nur
der Dienst des Relays und root lesen: Der Dienst läuft als eigener,
unprivilegierter Systembenutzer, die Datei hat `0600`, und sie liegt hinter
einem Verzeichnis, das nur root betreten kann (`/var/lib/private`, wo systemd
die Daten des Dienstes ablegt). In Home Assistant liegt sie auf dem eigenen
`/data`-Volume der App. Behandle ein Backup des Geräts oder ein
Home-Assistant-Backup, das die App enthält, so wie das Gerät selbst.

**In der PoolPilot-Cloud.** Die Cloud kennt das Relay und den Haushalt, zu dem
es gehört, Typ, Adresse und Namen der Steuerungen sowie die Tunnel-Zugangsdaten,
die sie ausgestellt hat. Sie bekommt deine Alarme (welche Regel ausgelöst hat,
der Wert, der Name des Pools), damit sie sie als Push verschicken kann. Deine
laufenden Messwerte lädt das Relay nicht hoch.

## Netzwerk und Ports

**In deinem lokalen Netzwerk:**

| Port | Was |
| --- | --- |
| TCP 8443 (HTTPS) | Die lokale API des Relays, für die Kopplung und die App. Änderbar mit `LAN_LISTEN` (siehe [Konfiguration](configuration.md)) oder mit `lan_port` in Home Assistant. |
| UDP 5353 (mDNS) | Das Relay kündigt sich als `_poolpilot-relay._tcp` an, damit die App es ohne IP-Adresse findet. |

Außerdem spricht das Relay im lokalen Netzwerk mit deiner Poolsteuerung, über
HTTP oder HTTPS, je nachdem, was die Steuerung nutzt.

**Ausgehend ins Internet** — das Relay baut Verbindungen nur selbst auf, es
wartet nie auf eine von außen:

| Ziel | Wofür |
| --- | --- |
| `api.poolpilot.eu`, HTTPS (443) | Kopplung, Anmeldung der Steuerungen, Alarme, Update-Prüfung. |
| Der Tunnelserver, TCP 7000 (TLS) | Fernzugriff, wenn du unterwegs bist. Die Cloud nennt den Server beim Koppeln — derzeit `connect.poolpilot.eu`. |
| GitHub Releases, HTTPS (443) | Updates herunterladen. |
| `get.poolpilot.eu`, HTTPS (443) | Nur, wenn du den Installer ausführst. |

**Keine Portfreigabe, kein DynDNS.** Du öffnest nichts an deinem Router. Der
Fernzugriff funktioniert, weil das Relay den Tunnel zur Cloud selbst aufbaut.

## Der Tunnel in einfachen Worten

Zu Hause erreicht die App deinen Pool über dein WLAN. Unterwegs geht das über
den **Tunnel**: eine Verbindung, die das Relay von zu Hause zur PoolPilot-Cloud
aufbaut und offen hält.

- **Verschlüsselt.** Der Tunnel läuft über TLS. Bevor das Relay irgendwelche
  Zugangsdaten schickt, prüft es das Zertifikat des Tunnelservers gegen das,
  das ihm die PoolPilot-Cloud beim Koppeln gegeben hat — so meldet es sich
  nicht bei einem Betrüger an.
- **Nur für gekoppelte Telefone.** Auch eine Anfrage, die durch den Tunnel
  kommt, braucht das Zugangstoken eines Telefons, das du gekoppelt hast. Ohne
  lehnt das Relay sie ab — die Webadresse deines Relays zu kennen reicht nicht.
  Das gilt auch für die eigene Weboberfläche deiner Poolsteuerung. Die einzige
  Ausnahme ist eine kleine öffentliche Info-Seite (Version des Relays, ob es
  gekoppelt ist, und sein öffentlicher Fingerabdruck), die nichts verrät, womit
  jemand hineinkäme.
- **Das Heikle bleibt zu Hause.** Koppeln sowie Werks- und Haushaltsreset
  funktionieren nur im lokalen Netzwerk. Durch den Tunnel lehnt das Relay sie
  ab, selbst von einem gekoppelten Telefon.
- **Was die Cloud sieht.** Dein Telefon spricht HTTPS mit der PoolPilot-Cloud,
  und die Cloud reicht die Anfrage durch den Tunnel weiter. Beide Strecken
  durchs Internet — vom Telefon zur Cloud und der Tunnel von der Cloud zu
  deinem Relay — sind verschlüsselt, aber es ist keine
  Ende-zu-Ende-Verschlüsselung: Die Cloud leitet deine Anfragen weiter. Die
  letzte Strecke, vom Relay zu deiner Poolsteuerung im Heimnetz, ist oft
  unverschlüsseltes HTTP, weil viele Steuerungen nur das sprechen.
- **Durchgereicht, nicht aufbewahrt.** Die Cloud reicht den Tunnel-Verkehr nur
  durch. Deine Anfragen und die Antworten der Steuerung sind dort nur für den
  Moment des Weiterleitens sichtbar — sie werden weder gespeichert noch
  ausgewertet. Der Web-Eingang des Tunnels führt kein Zugriffsprotokoll, es
  entsteht also keine Spur, was du wann mit deinem Pool gemacht hast. Der
  Tunnel-Server meldet der Cloud nur, *dass* sich ein Relay verbunden hat oder
  offline gegangen ist, nie, was hindurchgeht; die Cloud behält davon nur den
  jeweils letzten Zeitpunkt, für die Online-Anzeige in der App. Protokolliert
  werden nur Warnungen und Fehler — normale Anmeldungen und der Verkehr selbst
  nicht. Eine Warnung zu einer abgelehnten oder fehlerhaften Verbindung kann
  die Internetadresse enthalten, von der sie kam, aber die Logs sind klein und
  rotieren, sodass sie nach kurzer Zeit überschrieben wird.

Im lokalen Netzwerk spricht die App HTTPS mit dem Relay und pinnt sein
Zertifikat: Beim Koppeln lernt die App den Fingerabdruck des Relays und vertraut
ab dann nur diesem einen Relay.

## Wo die Cloud läuft

Die PoolPilot-Cloud läuft auf Servern bei **Hetzner in Deutschland**. Ihre
Backups sind verschlüsselt und liegen ebenfalls in **Deutschland**.

## Signierte Versionen

Jede Version erscheint unter
[GitHub Releases](https://github.com/ylabonte/poolpilot-relay/releases) mit
einer Liste von SHA-256-Prüfsummen, signiert mit `minisign`.

- **Der Installer** prüft die SHA-256-Prüfsumme jedes Downloads und bricht bei
  einer Abweichung ab. Ist `minisign` installiert (der Installer bietet die
  Installation an), prüft er zusätzlich die Signatur.
- **Das Selbst-Update** prüft immer die Signatur, gegen einen fest ins Relay
  eingebauten Schlüssel, und danach die Prüfsumme jeder Datei. Auch die
  Download-Adresse ist fest eingebaut, sodass nicht einmal die PoolPilot-Cloud
  das Relay auf einen anderen Server umleiten kann. Siehe
  [Selbst-Update](self-update.md).

Du willst einen Download von Hand prüfen? Hol `sha256sums.txt` und
`sha256sums.txt.minisig` aus derselben Version, dann:

```bash
minisign -Vm sha256sums.txt -p minisign.pub   # die Signatur
sha256sum -c --ignore-missing sha256sums.txt  # die heruntergeladenen Dateien
```

Der öffentliche Schlüssel ist
[`deploy/relay/minisign.pub`](../../deploy/relay/minisign.pub) in diesem
Repository.

## Lizenz

Das Relay ist Open Source unter der [MIT-Lizenz](../../LICENSE). Das Programm
enthält [frp](https://github.com/fatedier/frp), das den Tunnel bereitstellt,
unter der Apache License 2.0 — siehe [NOTICE](../../NOTICE).

## Wenn PoolPilot Pro ausläuft

Zum Koppeln eines Relays brauchst du PoolPilot Pro. Läuft das Abo später aus,
parkt das Relay den Tunnel: Der Fernzugriff pausiert, aber das Relay behält
Kopplung, Einstellungen und Zugangsdaten. Sobald Pro wieder aktiv ist, läuft es
von selbst weiter. Siehe
[Fehlerbehebung](troubleshooting.md#fernzugriff-pausiert-oder-das-relay-hat-seinen-haushalt-vergessen).
