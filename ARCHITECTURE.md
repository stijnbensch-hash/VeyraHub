# Architectuur

Veyra Hub is een zelfstandig serverproces. Het staat bewust buiten de bronmap,
buildtargets en gegevensopslag van de Veyra-client.

## Gegevensstroom

1. Een beheerder voegt een Stremio-compatibele `manifest.json` toe.
2. De hub bewaart alleen manifestinformatie en bronvolgorde in `data/hub.json`.
3. Addoncatalogi worden op aanvraag opgehaald en naar Jellyfin-items vertaald.
4. Metadata wordt via de aangewezen addon opgehaald, met andere actieve
   metadata-addons als terugval.
5. Streamaddons worden parallel bevraagd. Alleen directe HTTP(S)-resultaten
   blijven over; de ingestelde addonvolgorde bepaalt de voorkeur.
6. De Jellyfin-streamroute stuurt de client tijdelijk door naar de gekozen
   directe bron. Mediabytes en tokens worden niet door de hub opgeslagen.

## Scheiding van de client

- geen Swift-code of Xcode-targets;
- geen import van clientmodellen of assets;
- een eigen Docker-image, configuratiemap en releasecyclus;
- koppeling uitsluitend via een gedocumenteerde Jellyfin-compatibele HTTP-laag;
- de webinterface gebruikt dezelfde visuele merktaal, maar is zelfstandig.

## Addon Registry

- elke addon wordt geregistreerd met een stabiele technische id; de
  zichtbare naam komt rechtstreeks van de addon en wordt niet door de hub
  overschreven;
- `enabled` staat los van installatie: uitschakelen bewaart configuratie,
  de hub stopt alleen met de addon aan te roepen; verwijderen is een aparte
  actie;
- een `configureURL` wordt afgeleid uit het manifest (behaviorHints) en
  opent altijd de addon zelf; de hub bouwt geen eigen configuratiescherm;
- elke addonaanroep (stream, catalogus, ondertitels) werkt de eigen
  gezondheidsstatus van die addon bij (`reachable`, laatste succes/fout,
  gesaneerde foutmelding) — onafhankelijk van `enabled`, zodat "aan maar
  onbereikbaar" zichtbaar blijft in het dashboard;
- capabilities zijn wat het manifest zelf opgeeft (`resources`), nooit
  hardcoded op naam: de hub roept een addon alleen aan voor een stream,
  ondertitel of catalogusaanvraag als die addon die resource ook meldt.
  Zo kan een metadata-only addon nooit voor een stream bevraagd worden, en
  omgekeerd;
- ondertitels zijn een eigen capability (`subtitles`), los van streams: een
  addon die geen `stream` levert kan toch ondertiteltracks aanleveren, en
  omgekeerd — de hub combineert beide onafhankelijk van elkaar.

## Beveiligingsgrenzen

- elk account (beheer en kijker) logt in met gebruikersnaam + wachtwoord;
  wachtwoorden worden gezouten en met PBKDF2-HMAC-SHA256 gehasht opgeslagen,
  nooit in leesbare vorm;
- inloggen levert een kortlevend toegangstoken en een refreshtoken op, per
  apparaat; alleen de SHA-256-hashes daarvan komen in `hub.json` terecht;
- beheerroutes vereisen bovendien dat de sessie aan het beheeraccount hangt;
- mediaserverroutes accepteren elke geldige, intrekbare sessie van een
  kijk- of beheeraccount;
- een sessie kan per apparaat worden ingetrokken, of in bulk via een
  wachtwoordreset;
- de publieke statusroute bevat geen addonconfiguratie;
- configuratiebestanden worden met alleen gebruikersrechten geschreven;
- publieke addon-URL's moeten HTTPS gebruiken; HTTP is alleen voor privéhosts;
- credentials in addon-URL's zijn niet toegestaan;
- manifest- en addonresponsen hebben vaste maximale groottes en time-outs.

Een TLS-afbrekende reverse proxy of vergelijkbare HTTPS-ingang blijft vereist
voor veilig gebruik buiten het lokale netwerk.
