# Selbst-Update

*[🇬🇧 English](../en/self-update.md) · 🇩🇪 Deutsch*

Dein Relay **hält sich selbst aktuell**. Du musst dich nicht einloggen und
irgendetwas ausführen — neue Versionen kommen von allein, und zwar sicher. Diese
Seite erklärt, was passiert und wie du es steuerst.

> **Läuft dein Relay als [Home-Assistant-App](home-assistant.md)?** Dann gilt
> das alles nicht: Dort aktualisiert sich das Relay nicht selbst. Neue Versionen
> kommen über den Add-on-Store von Home Assistant, wie bei jeder anderen App.

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
- **Sofort, von Hand.** Du willst nicht bis zur Nacht warten? Führe den
  Installer-Einzeiler auf dem Gerät erneut aus:

  ```bash
  curl -fsSL https://get.poolpilot.eu/install.sh | bash
  ```

  Er holt und installiert die aktuelle Version und ist auch auf einem Gerät
  unbedenklich, das schon aktuell ist.

<!-- Die App-Formulierung (Knopf „Jetzt aktualisieren“ und Schalter für
     automatische Updates auf der Relay-Seite, nur Eigentümer) wiederherstellen,
     sobald poolpilot-app#440 sie ausliefert. -->
> **Kommt in die App:** ein Knopf **Jetzt aktualisieren** und ein
> **Ein/Aus-Schalter für automatische Updates** auf der Seite des Relays in der
> PoolPilot-App, für den Haushalts-Eigentümer. Die App hat beides noch nicht;
> bis dahin nimm den Installer oben und die Einstellung am Gerät unten.

## Updates steuern

Automatische Updates sind standardmäßig **eingeschaltet**. Steuern kannst du
sie derzeit am Gerät selbst.

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

Mit `UPDATE_DISABLED=1` prüft das Relay **nicht** auf Updates und installiert
sie **nicht** — es ist ein harter Aus-Schalter. Um ein solches Gerät später zu
aktualisieren, führe den Installer erneut aus:

```bash
curl -fsSL https://get.poolpilot.eu/install.sh | bash
```

(Der kommende Schalter in der App wird die sanftere Wahl: Er setzt die
nächtliche Installation aus, lässt dich aber weiterhin auf Wunsch
aktualisieren.)

## Wenn ein Update zu hängen scheint

Der Helfer rollt bei einem fehlgeschlagenen Update automatisch zurück, ein
festhängendes Gerät ist also selten. Falls du eines vermutest, ist der sicherste
Reset, den Installer-Einzeiler erneut auszuführen — er holt und installiert die
aktuelle Version neu und ist auch auf einem bereits gesunden Gerät unbedenklich.
Siehe
[Fehlerbehebung → Ein festhängendes oder fehlgeschlagenes Update](troubleshooting.md#ein-festhängendes-oder-fehlgeschlagenes-update)
für die Logs, die du zuerst ansehen solltest.
