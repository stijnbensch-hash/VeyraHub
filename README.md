# Veyra Hub

Veyra Hub is een zelfstandig, self-hosted systeem dat door de gebruiker gekozen
addonbronnen als één mediaserver aanbiedt. Het is een apart project en deelt geen
code, configuratie, opslag of buildproces met de Veyra-app.

De hub vertaalt addoncatalogi, metadata, series en directe streams naar het deel
van de Jellyfin-API dat de huidige Veyra-client gebruikt. Daardoor kan hij in de
app als een gewone Jellyfin-server worden toegevoegd. Torrent-hashes, magnetlinks
en externe doorverwijzingen worden genegeerd. De hub levert zelf geen media of
publieke addonlijst. Gebruik alleen bronnen en media waarvoor je toestemming hebt.

## Starten met Docker

1. Kopieer deze map naar je server.
2. Maak naast `compose.yaml` een `.env` met een gebruikersnaam en een lang,
   willekeurig opstartwachtwoord:

   ```text
   VEYRA_HUB_USERNAME=admin
   VEYRA_HUB_TOKEN=vervang-dit-door-een-lang-willekeurig-wachtwoord
   ```

   `.env.example` kan hiervoor als startpunt worden gekopieerd. Deze waarden
   worden alleen gebruikt om, de eerste keer dat de hub start (als er nog geen
   enkel account bestaat), het beheeraccount aan te maken. Daarna is het een
   gewoon wachtwoord: wijzig het via de beheerpagina en `VEYRA_HUB_TOKEN` in
   `.env` wordt genegeerd.

3. Start met `docker compose up -d --build`.
4. Open `http://server-ip:8787` en log in met die gebruikersnaam/wachtwoord.
5. Maak in de beheerpagina een persoonlijk kijkaccount met een eigen
   gebruikersnaam en wachtwoord. Voeg de server in Veyra toe als
   Jellyfin-server met die gegevens.

Gebruik buiten het eigen netwerk HTTPS via een reverse proxy of privénetwerk.

## Accounts en sessies

- Elk account (beheer of kijker) heeft een eigen gebruikersnaam en wachtwoord.
  Wachtwoorden worden gezouten en gehasht opgeslagen (PBKDF2-HMAC-SHA256,
  210.000 iteraties); niemand hoeft ooit handmatig een API-sleutel te beheren.
- Inloggen via `POST /v1/auth/login` levert een toegangstoken (12u) en een
  refreshtoken (30 dagen) op, gebonden aan een apparaat-id. Jellyfin-clients
  zoals Veyra bewaren geen refreshtoken; `Users/AuthenticateByName` geeft
  daarom een toegangstoken die geldig blijft totdat de sessie wordt
  ingetrokken of het account wordt uitgeschakeld. Alleen tokenhashes komen
  in `hub.json` terecht.
- `POST /v1/auth/refresh` wisselt een geldig refreshtoken in voor een nieuw
  paar zonder opnieuw in te loggen; `POST /v1/auth/logout` trekt de huidige
  sessie in.
- Het beheeraccount mag addons, kijkaccounts en sessies beheren en hoort
  privé te blijven; kijkaccounts kunnen alleen de Jellyfin-compatibele
  mediaserver gebruiken.
- Een account kan direct worden gepauzeerd of verwijderd; een wachtwoordreset
  trekt automatisch alle sessies van dat account in
  (`PATCH /v1/users/{id}` met `password`), zodat oudere apparaten opnieuw
  moeten inloggen.
- `GET /v1/sessions` toont alle actieve sessies (gebruiker, apparaat,
  aanmaakdatum); `DELETE /v1/sessions/{id}` trekt een los apparaat in.

## Mediaservercompatibiliteit

De volgende routes zijn beschikbaar voor de huidige Veyra-client:

- serverinformatie en aanmelden;
- bibliotheken op basis van addoncatalogi;
- film-, serie-, sport- en afleveringslijsten;
- zoeken in catalogi die de `search`-extra ondersteunen;
- poster- en backdropdoorverwijzingen;
- directe videostreams via een beveiligde server-URL.

"Sports" is geen apart addontype maar gewoon een derde mediatype naast
`movie`/`series`: een addon levert een sportcatalogus door in zijn manifest
een catalogus met `"type": "sports"` op te geven, en die stroomt daarna door
dezelfde generieke catalog/meta/stream/subtitles-routes als films en series.
In de Jellyfin-compatibiliteitslaag verschijnt zo'n catalogus als een
bibliotheek van het type `livetv`.

Dit is een doelgerichte compatibiliteitslaag en nog geen volledige implementatie
van iedere Jellyfin-route. Andere Jellyfin-clients kunnen daarom deels werken,
maar zijn in deze versie niet allemaal gegarandeerd.

## Addons: configureren en status

- Heeft een addon een `configurable`/`configurationRequired` behaviorHint in
  zijn manifest, dan toont de beheerpagina een "Configureren"-knop die de
  addon zelf opent (bij Stremio-stijl addons op `<addon-url>/configure`). De
  hub bouwt daar bewust geen eigen instellingenformulier voor na.
- Elke keer dat de hub een addon aanroept voor een stream of catalogus
  onthoudt hij of dat lukte: `reachable`, `lastSuccessAt`, `lastErrorAt` en
  een gesaneerde `lastError` (nooit de ruwe addon-URL of een query-string
  met een sleutel erin) staan in `GET /v1/addons` en worden getoond in de
  beheerpagina. Dit is puur telemetrie; enabled/disabled blijft een losse,
  door de beheerder bepaalde schakelaar.

## VeyraHub Recorder (opnames)

Live-tv opnemen loopt via een eigen, optioneel proces naast de hub —
`veyrahub-recorder`, in de `recorder/`-map — niet via de hub zelf en niet via
Strand Recorder. Het is uitgeschakeld totdat je het opzet.

1. **Bouw en installeer de recorder op de VPS** (Go 1.23+):

   ```sh
   cd recorder
   go build -o veyrahub-recorder .
   sudo install -m 755 veyrahub-recorder /opt/veyrahub-recorder/veyrahub-recorder
   ```

2. **FFmpeg is verplicht** (opnemen zonder hercoderen):

   ```sh
   sudo apt install -y ffmpeg
   ```

3. **Comskip is optioneel** — alleen nodig voor reclamedetectie
   (`recorder/README.md` beschrijft waarom en hoe de opname er zonder eruit
   ziet). Installeer het en zet `recorder/comskip.ini` op
   `/etc/veyrahub-recorder/comskip.ini`; laat je dit weg, dan werken opnemen
   en afspelen gewoon door, alleen zonder reclame-overslaan in Veyra.

4. **Zet de recorder als systemd-service.** `recorder/veyrahub-recorder.service`
   is kant-en-klaar (eigen gebruiker, alleen-lezen bestandssysteem op de rest
   van de VPS, `ReadWritePaths` beperkt tot zijn eigen datamap):

   ```sh
   sudo useradd --system --home /var/lib/veyrahub-recorder --shell /usr/sbin/nologin veyrarecorder
   sudo cp recorder/veyrahub-recorder.service /etc/systemd/system/
   sudo tee /etc/veyrahub-recorder.env <<'EOF'
   VEYRA_RECORDER_TOKEN=een-lang-willekeurig-intern-token
   EOF
   sudo systemctl daemon-reload
   sudo systemctl enable --now veyrahub-recorder
   ```

   De recorder luistert standaard alleen op `127.0.0.1:8480` — hij is nooit
   rechtstreeks vanaf buiten de VPS bereikbaar.

5. **Vertel de hub waar de recorder draait.** Zet in de hub's `.env`
   (of `compose.yaml`-omgeving) hetzelfde token als hierboven:

   ```text
   VEYRA_HUB_RECORDER_URL=http://127.0.0.1:8480
   VEYRA_HUB_RECORDER_TOKEN=een-lang-willekeurig-intern-token
   ```

   en herstart de hub. Zonder deze twee variabelen blijft `/v1/recorder/*`
   gewoon bestaan maar antwoordt het met "VeyraHub Recorder is niet
   geconfigureerd." — de rest van de hub blijft onaangetast werken.

6. Klaar: de beheerpagina krijgt een "Recorder"-tabblad, en in Veyra's
   programmagids verschijnt een opnameknop bij elk nog niet uitgezonden
   programma (zie `recorder/README.md` voor hoe opslag, privacy en Comskip
   precies werken).

## API

- `GET /health` — publieke livenesscontrole zonder configuratiedetails.
- `POST /v1/auth/login` — `{username, password, deviceId?, deviceName?}` →
  `{accessToken, refreshToken, user}`.
- `POST /v1/auth/refresh` — `{refreshToken}` → nieuw token-paar.
- `POST /v1/auth/logout` — trekt de sessie van het meegestuurde toegangstoken in.
- `GET /v1/status` — hubidentiteit en aantallen. *(beheer)*
- `GET /v1/sessions` — actieve sessies over alle accounts. *(beheer)*
- `DELETE /v1/sessions/{id}` — een sessie/apparaat intrekken. *(beheer)*
- `GET|POST /v1/addons` — addons bekijken/toevoegen. *(beheer)*
- `PATCH|DELETE /v1/addons/{id}` — activeren, pauzeren of verwijderen. *(beheer)*
- `GET|POST /v1/users` — persoonlijke kijkaccounts bekijken/toevoegen. *(beheer)*
- `PATCH|DELETE /v1/users/{id}` — een kijkaccount pauzeren, wachtwoord
  resetten of verwijderen. *(beheer)*
- `GET /v1/streams/{movie|series|sports}/{imdb-id}` — directe streams samenvoegen, alleen van addons die `stream` als resource opgeven. *(beheer)*
- `GET /v1/subtitles/{movie|series|sports}/{imdb-id}` — ondertiteltracks samenvoegen, alleen van addons die `subtitles` als resource opgeven. Losstaand van streams: eender welk aantal ondertiteladdons kan tracks leveren, ongeacht welke addon de video zelf leverde. *(beheer)*
- `GET /api/v1/catalogs`, `GET /api/v1/catalog/{addonID}/{movie|series|sports}/{catalogID}`,
  `GET /api/v1/items/{movie|series|sports}/{id}`,
  `GET /api/v1/items/{movie|series|sports}/{id}/streams`,
  `GET /api/v1/items/{movie|series|sports}/{id}/subtitles` — de native API
  van de hub: neemt een ruwe (bijv. IMDb-stijl) id direct aan, zonder eerst
  een catalogus te doorzoeken.
- `GET /v1/recorder/status`, `GET|POST /v1/recorder/recordings`,
  `DELETE /v1/recorder/recordings/{id}`,
  `POST /v1/recorder/recordings/{id}/stop`,
  `GET /v1/recorder/recordings/{id}/file` — VeyraHub Recorder, alleen actief
  wanneer `VEYRA_HUB_RECORDER_URL`/`VEYRA_HUB_RECORDER_TOKEN` zijn gezet (zie
  hierboven). De `/file`-route accepteert het toegangstoken ook als
  `?access_token=`-querywaarde, voor spelers die geen eigen headers kunnen
  meesturen; elke andere route vereist zoals gebruikelijk de
  `Authorization`-header.

Routes gemarkeerd *(beheer)* vereisen `Authorization: Bearer <accessToken>`
van een ingelogd beheeraccount; de rest van de `/v1`-routes vereist alleen een
geldige sessie (beheer of kijker), op `/v1/auth/login` na.

## Ontwikkelen en controleren

Vereist Go 1.23 of nieuwer:

```sh
go test ./...
go run . -token een-lokaal-testtoken
```

## Grenzen van deze versie

- Er is nog geen voortgangssynchronisatie of transcoding.
- Alleen directe `http`- en `https`-streams worden doorgegeven.
- De eerste directe bron die de addons opleveren wordt gebruikt voor de
  mediaserverstream; interactieve bronkeuze volgt later.

## App Store-randvoorwaarde

De hub is zelf geen App Store-app. Een client die zijn resultaten toont blijft
echter verantwoordelijk voor de rechten op en voorwaarden van alle getoonde
media en diensten. Daarom bevat de hub geen ingebouwde publieke addonlijst,
downloads, torrentuitvoering of omzeiling van toegangsbeperkingen. Een geslaagde
technische koppeling is geen garantie op goedkeuring voor distributie.
