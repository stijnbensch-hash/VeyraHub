# VeyraHub Recorder

Eigen opnameproces voor VeyraHub. Het gebruikt FFmpeg om door de gebruiker
gekozen HTTP(S)-livestreams zonder hercodering op te slaan. Indien Comskip op
de host staat en `/etc/veyrahub-recorder/comskip.ini` aanwezig is, scant het voltooide opnames op advertentieblokken. De service
heeft een eigen `recordings.json` en eigen videobestanden onder
`/var/lib/veyrahub-recorder`; Strand Recorder wordt niet aangeroepen of gedeeld.

De service luistert alleen op `127.0.0.1:8480`. De Hub spreekt de recorder
aan met een intern token; Veyra-clients gebruiken hun bestaande VeyraHub-sessie
en krijgen alleen hun eigen opnames te zien. De beheerder ziet alle opnames.

De EPG op iOS en tvOS stuurt titel, zender, streamadres, start en einde naar
`POST /v1/recorder/recordings` op VeyraHub. Het streamadres kan
providergegevens bevatten en staat daarom alleen in een bestand met
gebruikersrechten op de VPS. Het verschijnt niet in lijstresponsen of logs.

De service verandert geen VeyraHub-opslag en gebruikt geen Strand-volume.
Gebruik enkel bronnen waarvoor je opname- en gebruiksrechten hebt.

Zie de hoofd-`README.md` (sectie "VeyraHub Recorder") voor de installatie op
de VPS: bouwen, FFmpeg/Comskip, de systemd-service en het koppelen aan de hub.

## Configuratie (omgevingsvariabelen)

| Variabele | Standaard | Betekenis |
| --- | --- | --- |
| `VEYRA_RECORDER_TOKEN` | *(verplicht)* | Intern token waarmee de hub zich bij de recorder identificeert. |
| `VEYRA_RECORDER_ADDRESS` | `127.0.0.1:8480` | Waar de recorder luistert — alleen loopback wijzigen als je weet wat je doet. |
| `VEYRA_RECORDER_DATA` | `/var/lib/veyrahub-recorder` | Map voor `recordings.json`. |
| `VEYRA_RECORDER_RECORDINGS` | `/var/lib/veyrahub-recorder/files` | Map voor de opgenomen `.ts`-bestanden. |
| `VEYRA_RECORDER_COMSKIP_INI` | `/etc/veyrahub-recorder/comskip.ini` | Comskip-configuratie; ontbreekt dit bestand, dan wordt reclamedetectie stilzwijgend overgeslagen. |

FFmpeg en (optioneel) `comskip` worden via `PATH` gevonden — er is geen apart
pad-instelling voor FFmpeg.
