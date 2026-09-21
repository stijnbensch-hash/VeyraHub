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

## Beveiligingsgrenzen

- alle beheer- en mediaserverroutes vereisen hetzelfde lokaal ingestelde token;
- de publieke statusroute bevat geen addonconfiguratie;
- configuratiebestanden worden met alleen gebruikersrechten geschreven;
- publieke addon-URL's moeten HTTPS gebruiken; HTTP is alleen voor privéhosts;
- credentials in addon-URL's zijn niet toegestaan;
- manifest- en addonresponsen hebben vaste maximale groottes en time-outs.

Een reverse proxy of privé-overlaynetwerk blijft vereist voor veilig gebruik
buiten het lokale netwerk.
