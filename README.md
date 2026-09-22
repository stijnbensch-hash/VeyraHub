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
- film-, serie- en afleveringslijsten;
- zoeken in catalogi die de `search`-extra ondersteunen;
- poster- en backdropdoorverwijzingen;
- directe videostreams via een beveiligde server-URL.

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
- `GET /v1/streams/{movie|series}/{imdb-id}` — directe streams samenvoegen, alleen van addons die `stream` als resource opgeven. *(beheer)*
- `GET /v1/subtitles/{movie|series}/{imdb-id}` — ondertiteltracks samenvoegen, alleen van addons die `subtitles` als resource opgeven. Losstaand van streams: eender welk aantal ondertiteladdons kan tracks leveren, ongeacht welke addon de video zelf leverde. *(beheer)*

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
