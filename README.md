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
   willekeurig wachtwoord/token:

   ```text
   VEYRA_HUB_USERNAME=admin
   VEYRA_HUB_TOKEN=vervang-dit-door-een-lang-willekeurig-wachtwoord
   ```

   `.env.example` kan hiervoor als startpunt worden gekopieerd.

3. Start met `docker compose up -d --build`.
4. Open `http://server-ip:8787`, vul het token in en voeg addonmanifesten toe.
5. Voeg `http://server-ip:8787` in Veyra toe als Jellyfin-server met dezelfde
   gebruikersnaam en hetzelfde token als wachtwoord.

Gebruik buiten het eigen netwerk HTTPS via een reverse proxy of privénetwerk.
De hub bewaart alleen de addonconfiguratie. De inloggegevens komen uit de lokale
omgeving, worden niet in `hub.json` geschreven en worden niet gelogd.

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

## API

- `GET /health` — publieke livenesscontrole zonder configuratiedetails.
- `GET /v1/status` — hubidentiteit en aantallen.
- `GET|POST /v1/addons` — addons bekijken/toevoegen.
- `PATCH|DELETE /v1/addons/{id}` — activeren, pauzeren of verwijderen.
- `GET /v1/streams/{movie|series}/{imdb-id}` — directe streams samenvoegen.

Alle `/v1`-routes vereisen `Authorization: Bearer <token>`.

## Ontwikkelen en controleren

Vereist Go 1.23 of nieuwer:

```sh
go test ./...
go run . -token een-lokaal-testtoken
```

## Grenzen van deze versie

- Er is nog geen multi-userbeheer, voortgangssynchronisatie of transcoding.
- Alleen directe `http`- en `https`-streams worden doorgegeven.
- De eerste directe bron die de addons opleveren wordt gebruikt voor de
  mediaserverstream; interactieve bronkeuze volgt later.

## App Store-randvoorwaarde

De hub is zelf geen App Store-app. Een client die zijn resultaten toont blijft
echter verantwoordelijk voor de rechten op en voorwaarden van alle getoonde
media en diensten. Daarom bevat de hub geen ingebouwde publieke addonlijst,
downloads, torrentuitvoering of omzeiling van toegangsbeperkingen. Een geslaagde
technische koppeling is geen garantie op goedkeuring voor distributie.
