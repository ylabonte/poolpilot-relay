# Selbst-Update

*[🇬🇧 English](../en/self-update.md) · 🇩🇪 Deutsch*

Dein Relay **hält sich selbst aktuell**. Du musst dich nicht einloggen und
irgendetwas ausführen — neue Versionen kommen von allein, und zwar sicher. Diese
Seite erklärt, was passiert und wie du es steuerst.

## Wie es funktioniert

Das Relay meldet sich regelmäßig bei der PoolPilot-Cloud, um zu prüfen, ob eine
neuere Version verfügbar ist. Wenn ja, lädt es sie herunter und **prüft** sie;
danach installiert ein kleiner privilegierter Helfer sie und startet das Relay
neu. Kommt die neue Version nicht gesund hoch, **rollt der Helfer automatisch
zurück** auf die Version, die vorher lief — ein fehlerhaftes Update kann dich
also nicht abhängen.

Updates sind kryptografisch signiert, und der Schlüssel zur Signaturprüfung ist
fest ins Programm eingebaut. Damit kann weder ein gehackter Download-Server noch
eine gehackte Cloud Code aufspielen, den dein Relay akzeptiert — es installiert
nur Versionen mit gültiger Signatur.

## Wann Updates passieren

- **Automatisch, über Nacht.** Ein bereitliegendes Update installiert sich in
  einem nächtlichen Fenster zwischen **03:00 und 05:00 Uhr Ortszeit**, zu einer
  pro Gerät festen Zeit innerhalb dieses Fensters (damit nicht eine ganze
  Nachbarschaft von Relays in derselben Sekunde neu startet). Ein Gerät übernimmt
  eine neue Version normalerweise binnen etwa eines Tages.
- **Auf Wunsch, aus der App.** Du willst nicht bis zur Nacht warten? Öffne das
  Relay in der PoolPilot-App und tippe auf **Jetzt aktualisieren** — ein
  wartendes Update installiert sich sofort.

## Updates steuern

Du hast zwei voneinander unabhängige Steuerungen:

### Aus der App — empfohlen

Die Seite des Relays in der PoolPilot-App hat einen **Ein/Aus-Schalter für
automatische Updates**:

- **Ein** (Standard): Updates installieren sich über Nacht von selbst.
- **Aus**: Das Relay **installiert Updates nicht von selbst**, aber du kannst
  jederzeit auf **Jetzt aktualisieren** tippen — etwa um einen Sicherheitsfix zu
  deinem eigenen Zeitpunkt einzuspielen.

Das ist die Einstellung, die die meisten wollen, weil sie das Gerät nie ohne
Update-Möglichkeit zurücklässt.

### Am Gerät — vollständiges Abschalten

Um ein einzelnes Gerät komplett aus dem Selbst-Update herauszunehmen, setze dies
in [`/etc/poolpilot-relay/config`](configuration.md):

```bash
UPDATE_DISABLED=1
```

und starte es neu:

```bash
sudo systemctl restart poolpilot-relay
```

Mit `UPDATE_DISABLED=1` prüft das Relay **nicht** auf Updates, installiert sie
**nicht** automatisch **und „Jetzt aktualisieren" in der App funktioniert dann
auch nicht** — es ist ein harter Aus-Schalter. Um ein solches Gerät später zu
aktualisieren, führe den Installer erneut aus:

```bash
curl -fsSL https://get.poolpilot.eu/install.sh | bash
```

(Das ist der Unterschied zum App-Schalter oben, mit dem du weiterhin auf Wunsch
aktualisieren kannst.)

## Wenn ein Update zu hängen scheint

Der Helfer rollt bei einem fehlgeschlagenen Update automatisch zurück, ein
festhängendes Gerät ist also selten. Falls du eines vermutest, ist der sicherste
Reset, den Installer-Einzeiler erneut auszuführen — er holt und installiert die
aktuelle Version neu und ist auch auf einem bereits gesunden Gerät unbedenklich.
Siehe
[Fehlerbehebung → Ein festhängendes oder fehlgeschlagenes Update](troubleshooting.md#ein-festhängendes-oder-fehlgeschlagenes-update)
für die Logs, die du zuerst ansehen solltest.
