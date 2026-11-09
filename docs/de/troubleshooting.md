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

## Wiederkehrende mDNS-RFC6762-Logzeilen

Wenn bei jedem Start Zeilen wie diese auftauchen:

```
INFO … mdns.go:475: dnssd: In both multicast query and multicast response messages, the Recursion Available bit MUST be zero on transmission. (RFC6762 18.7)
```

ist das **normal und harmlos** – es ist nichts kaputt. Sie kommen aus der
Discovery-Bibliothek, mit der sich das Relay per mDNS ankündigt: Vor dem Senden
jedes Pakets setzt sie ein Header-Flag zurück, das RFC 6762 auf null verlangt,
und protokolliert einen Hinweis. Das tatsächlich gesendete Paket ist korrekt, und
die Discovery funktioniert. Neuere Relays blenden diese Zeilen standardmäßig aus;
wenn du sie zurückhaben willst (zum Debuggen der Discovery), setze
`MDNS_VERBOSE_LOGS=1` in der Konfiguration oder aktiviere **Zusätzliche
mDNS-Logs** auf dem Konfigurations-Tab der Home-Assistant-App.

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

## Fernzugriff pausiert, oder das Relay hat seinen Haushalt vergessen

Kann sich der Fernzugriffs-Tunnel des Relays ein paar Minuten lang nicht
verbinden, fragt das Relay die PoolPilot-Cloud nach dem Grund und handelt
danach. Dasselbe tut es, solange es keine Ende-zu-Ende-Fernzugriffsadresse
hat, die es bedienen kann (die Cloud hat noch keine geliefert, oder der
Ende-zu-Ende-Zugang ist abgeschaltet): in der ersten Stunde nach einem Start
ein paar Mal, danach etwa einmal pro Stunde. Der Fernzugriff ist dann nicht
verfügbar, und die Logzeile sagt *„no end-to-end proxy to serve yet“*. Zu tun
ist dabei nichts. Jede Änderung steht
als eine `relay lifecycle:`-Zeile im Log:

- **„parking the tunnel; credentials kept“ (`state=suspended`)**: Das Abo
  des Haushalts ist nicht aktiv (zum Beispiel abgelaufen). Das Relay versucht
  nicht mehr, sich für den Fernzugriff zu verbinden, behält aber alles andere.
  Es prüft regelmäßig erneut, und sofort, sobald du zu Hause die App öffnest,
  und läuft von selbst weiter, wenn das Abo wieder aktiv ist. `GET /v1/status`
  zeigt den Grund in `tunnel.last_error` (z. B. `suspended: lapsed`).
- **`state=unknown`**: Die Cloud hat dieses Relay über einige Stunden hinweg
  mehrfach nicht erkannt. Das Relay pausiert den Fernzugriff genauso, behält
  seine Einstellungen und prüft weiter. Löst sich das nicht, gibt es den
  Haushalt dahinter in der Cloud sehr wahrscheinlich nicht mehr (z. B. weil er
  gelöscht wurde, bevor die Cloud Relays darüber informieren konnte, oder weil
  das Relay monatelang offline war). Neu koppeln funktioniert nicht, solange
  die alten Telefone noch gekoppelt sind. Mach deshalb einen
  [Werksreset](#werksreset) und richte das Relay danach erneut aus der App ein.
- **„household deleted in the cloud; dropped household data“**: Der Haushalt
  wurde gelöscht (z. B. über „Alle meine Daten löschen“ in der App). Das Relay
  vergisst alles, was dazugehörte (gekoppelte Telefone, Steuerungen,
  Cloud-Zugangsdaten), und ist bereit für eine neue Einrichtung. Seine eigene
  Identität behält es, Kopplungs-QR-Code und Fingerabdruck bleiben also gleich.
  Richte es einfach erneut aus der App ein (siehe unten). Eine neue Identität
  (z. B. bevor du das Gerät weitergibst) bekommst du nur per
  [Werksreset](#werksreset).
- **„household reset over the LAN API; dropped household data“**: Derselbe
  Reset, nur sofort. Nutzt du „Alle meine Daten löschen“ in der App, während
  dein Telefon im selben Netzwerk wie das Relay ist, bittet die App das Relay,
  den Haushalt sofort zu vergessen (`POST /v1/household-reset`, nur im LAN),
  statt zu warten, bis das Relay es selbst bemerkt. Das Ergebnis ist dasselbe
  wie in der Zeile darüber: Die Identität bleibt erhalten, und das Relay ist
  bereit für eine neue Einrichtung. Erreicht das Telefon das Relay in dem
  Moment nicht, geht nichts verloren — das Relay bemerkt es weiterhin selbst,
  mit eingerichteter Steuerung nach wenigen Minuten, ohne Steuerung
  innerhalb von etwa einem Tag.

## Relay neu koppeln

Zeige den Kopplungscode am Gerät an und koppel aus der App:

```bash
sudo poolpilot-relay show-pairing
```

Scanne den QR-Code mit der PoolPilot-App oder der Kamera deines Telefons, öffne
den ausgegebenen Link auf dem Telefon oder tippe den Fingerabdruck in die
manuelle Eingabe der App. Der Befehl ist schreibgeschützt und jederzeit
gefahrlos aufrufbar.

## Eigentümer- und Mitglieds-Telefone

Jedes mit dem Relay gekoppelte Telefon hat eine Haushaltsrolle, die beim Koppeln
festgelegt wird:

- **Eigentümer**: das Telefon, das das Relay eingerichtet hat, und ein Telefon,
  das die Eigentümer-Rolle mit einem [Wiederherstellungscode](#eigentümer-zugriff-verloren)
  zurückgeholt hat.
- **Mitglied**: ein Telefon, das mit einer Einladung des Eigentümers beigetreten ist.

Beide sehen den Pool, seine Messwerte, Warnungen und die eigene Weboberfläche
der Steuerung, und beide können **sich selbst** vom Relay entfernen. Das Relay
selbst ändern dürfen nur Eigentümer: Steuerungen hinzufügen, ändern oder
entfernen, den Fernzugriffs-Link einer Steuerung erneuern, Warnregeln ändern,
ein **anderes** Telefon entfernen, Updates installieren oder automatische
Updates umschalten sowie Haushalts- und Werksreset. Fragt ein Mitglieds-Telefon
eines davon an, antwortet die lokale API des Relays mit `403` und dem Fehlercode
`owner_required`, und nichts ändert sich.

Kein anderes Telefon kann das **letzte** Eigentümer-Telefon entfernen. Versucht
es ein Mitglieds-Telefon, erhält es `403` mit `owner_required` (siehe oben).
Versuchen zwei Eigentümer-Telefone im selben Moment, sich gegenseitig zu
entfernen, erhält die zweite Anfrage `409` mit dem Fehlercode
`last_owner_device`. In beiden Fällen ändert sich nichts. Selbst entfernen kann
sich das letzte Eigentümer-Telefon weiterhin. Ist es verloren, hol dir zuerst mit
einem [Wiederherstellungscode](#eigentümer-zugriff-verloren) die Eigentümer-Rolle
zurück und entferne dann das verlorene Telefon.

Ein entferntes Telefon verliert den Zugriff auf das Relay sofort. Das Relay
meldet die Entfernung außerdem an die PoolPilot-Cloud. Ist die Cloud in dem
Moment nicht erreichbar, behält das Relay die Meldung und versucht es bei seinen
regelmäßigen Kontakten weiter, bis die Cloud sie bestätigt, längstens 30 Tage;
solange das Abo des Haushalts nicht aktiv ist, nur alle paar Stunden.

Den Pool bedienen dürfen alle Telefone des Haushalts: Auch Mitglieder nutzen die
eigene Weboberfläche der Steuerung samt der Einstellungen, die sie anbietet.
Die Eigentümer-Liste oben betrifft die haushaltsweite Konfiguration des Relays
selbst, nicht die Einstellungen der Steuerung.

Telefone, die gekoppelt wurden, bevor das Relay Rollen gespeichert hat, bekommen
ihre Rolle einmalig beim ersten Start mit dieser Version: Das Telefon, das das
Relay eingerichtet hat, und jedes Telefon, dessen Herkunft das Relay nicht
sicher erkennen kann, werden Eigentümer; ein Telefon, das erkennbar später per
Einladung beigetreten ist, wird Mitglied. Hat ein Telefon die falsche Rolle, kann
ein Eigentümer es entfernen und neu einladen oder die Eigentümer-Rolle mit einem
Wiederherstellungscode zurückholen (unten).

Die Rolle auf dem Relay wird beim Koppeln festgelegt. Wer in den
Haushaltseinstellungen der App befördert oder herabgestuft wird, behält auf dem
Relay seine Rolle; soll ein Mitglieds-Telefon auf dem Relay Eigentümer-Rechte
bekommen, koppel es mit einem Wiederherstellungscode.

## Der Haushalt hat sein Geräte-Limit erreicht

In einem Haushalt können dein eigenes Gerät plus bis zu 10 weitere gleichzeitig
angemeldet sein. Ist er voll, koppelt das Relay kein weiteres Telefon per Einladung.
Die Eigentümer-Rolle mit einem Wiederherstellungscode zurückzuholen ist die
einzige Ausnahme: Sie wird auch bei vollem Haushalt angenommen, bis zu einem
Gerät über dem Limit, damit ein Eigentümer, der alle Telefone verloren hat,
wieder hineinkommt und die nicht mehr genutzten entfernen kann. Eine zweite
Wiederherstellung, solange der Haushalt schon ein Gerät über dem Limit liegt,
wird abgelehnt, bis ein Telefon entfernt ist. Die App erhält bei vollem
Haushalt `409` mit dem Fehlercode `household_device_quota` (oder `device_quota`,
wenn die Liste dieses Relays selbst voll ist). Es wird nichts gekoppelt und nichts
verbraucht – weder der Einladungscode noch der Wiederherstellungscode, der bis
zu seinem Ablauf gilt. Das Limit wird vor dem
Einladungscode selbst geprüft, daher bekommt auch eine vertippte oder
abgelaufene Einladung diese Antwort und wird erst als solche abgelehnt, wenn
wieder Platz ist.

Um Platz zu schaffen, entfernt ein Eigentümer in der App unter
**PoolPilot Relay → Gekoppelte Geräte** ein Telefon, das nicht mehr genutzt
wird, oder ein Mitglied meldet sich auf einem Telefon ab, das es nicht mehr
braucht. Danach mit demselben Code erneut koppeln.

Holst du dir die Eigentümer-Rolle zurück, weil kein Eigentümer-Telefon mehr übrig
ist, hält dich ein voller Haushalt nicht mehr auf: Mit dem Code wiederherstellen,
dann in der App unter **PoolPilot Relay → Gekoppelte Geräte** die nicht mehr
vorhandenen Telefone entfernen. Das setzt ein Relay und eine PoolPilot-Cloud
voraus, die beide diese Ausnahme enthalten. Bei Relays bis v1.0.0 lehnt das
Relay selbst noch mit `409 device_quota` ab; und bei einem neueren Relay, aber
einer Cloud ohne die Ausnahme, antwortet die Cloud mit
`409 household_device_quota`. In beiden Fällen wird nichts gekoppelt und der
Wiederherstellungscode bleibt gültig, daher muss sich zuerst ein Mitglied auf
einem Telefon abmelden, das es nicht mehr braucht. Kann das auch kein Mitglied
(die übrigen Telefone wurden zurückgesetzt oder deinstalliert, ohne sich
abzumelden), siehe [Immer noch hängen?](#immer-noch-hängen).

Relays bis v0.12.0 antworten in diesem Fall stattdessen mit `409 voucher_quota`,
worauf die App rät, ein paar Minuten zu warten und es mit demselben Code erneut
zu versuchen. Warten hilft auch dort nicht: Erst Platz schaffen.

## Eigentümer-Zugriff verloren

Wenn kein Telefon mehr als Haushalts-Eigentümer agieren kann, erzeuge am Gerät
einen einmaligen Wiederherstellungscode und scanne ihn mit dem Telefon, das der
Eigentümer werden soll:

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

Du kannst ein Relay aus der PoolPilot-App auf Werkseinstellungen zurücksetzen —
**nur vom Telefon des Eigentümers und nur zu Hause im selben Netzwerk wie das
Relay**. Über den Fernzugriffs-Tunnel und von einem Mitglieds-Telefon lehnt das
Relay einen Werksreset ab. Das löscht Kopplung und Identität, und der Agent
startet mit einer frischen neu (systemd fährt ihn sofort wieder hoch). Danach
ist das Relay entkoppelt — [koppel es neu](#relay-neu-koppeln), als wäre es neu.
Weil sich die Identität geändert hat, entferne zuerst den alten Relay-Eintrag in
der App, falls er noch gelistet ist.

**Verlässt das letzte Telefon das Relay, setzt es sich selbst zurück.** Entfernt
sich das letzte mit dem Relay gekoppelte Telefon zu Hause im selben Netzwerk
(zum Beispiel, wenn der Eigentümer sein eigenes Telefon trennt und kein anderes
Telefon mehr gekoppelt ist), macht das Relay genau dasselbe wie ein Werksreset:
Es gibt seinen Platz in deinem Haushalt in der Cloud frei, vergisst Kopplung,
Controller und Cloud-Zugangsdaten und startet mit einer frischen Identität neu,
sofort bereit für eine neue Einrichtung. Im Log steht dann *„relay reset: the
last paired device left“*. Über den Fernzugriffs-Tunnel passiert das nie: Das
Relay lehnt es ab, sein letztes Telefon aus der Ferne zu entfernen.

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
