# Moorage – plán

> Stav: **návrh, čeká na schválení** · 2026-11-20
> Projekt je **mimo Empty Pockets** – žádné EP vrstvy (účet, shake-to-report, site chrome, entitlements).

## Pitch

**Moorage** – jedno místo pro Docker hosty: spravuj compose stacky, čti živé logy a hlídej dostupnost zvenku. Spojuje to, co dnes dělají Dockge, Dozzle a Uptime Kuma, do jednoho objektu: **stack → kontejnery → logy → kontroly → alerty**.

## Publikum

- Primárně autor: vlastní VPS a domácí Raspberry Pi (na Pi nahradí Dockge).
- Open source (MIT) pro homelab a malé self-hosted servery. Bez placené verze, bez provozních nákladů.

## Výchozí analýza

| Vrstva | Dockge | Dozzle | Uptime Kuma | Moorage |
|---|---|---|---|---|
| Zdroj dat | `docker.sock` (zápis) | `docker.sock` (čtení) | HTTP/TCP/ping/DNS (+ volitelně socket) | agent nad Docker API + sondy z hubu |
| Centrální objekt | stack | kontejner | monitor | **stack** (obsahuje kontejnery, logy, kontroly) |
| Realtime | Socket.IO | SSE | Socket.IO | 1 WebSocket (multiplex) |
| Multi-host | agenti | agenti | – | hub + agenti (gRPC/mTLS) |
| Notifikace | – | alerty, webhooky | 90+ kanálů | vestavěné kanály, alert s kontextem |
| Auth | vlastní | vlastní / forward | vlastní + 2FA | lokální + TOTP, OIDC, forward-auth |
| Stav projektu (11/2026) | stojí (1.5.0, 3/2025) | aktivní (v11.5.0) | aktivní (2.5.12), hodně advisories | – |

### Konkurence

- **Dockhand** (Finsys) – nejbližší: stacky, git deploy, multi-host (agent Hawser), terminál, logy, file browser, Restic zálohy, OIDC. Bun + SvelteKit. **Licence BSL 1.1** (organizace platí), RBAC jen Enterprise. **Neřeší uptime sondy ani status stránku.**
- **Komodo** – stacky, buildy, servery, alerty; bez veřejné status stránky a externích sond.
- **Portainer** – široký kontejnerový manažer, těžký, bez uptime.
- **Coolify / Dokploy** – PaaS, jiný účel.

### Čím se Moorage liší

1. **Zdraví zvenku je součást stacku** – sondy a status stránka patří ke stacku, ne do jiné aplikace.
2. **Alert s kontextem** – notifikace nese stack, kontejner, exit code, posledních ~20 řádků logu a odkaz na log v čase pádu.
3. **Hlídač, který neumře s hlídaným** – hub může běžet mimo hlídané hosty, agenti mají watchdog hubu.
4. **MIT, malá Go binárka** – žádná BSL ani enterprise paywall.

## Jádrová smyčka

Nasadím/upravím stack → vidím, že je zdravý → když není, přijde alert s kontextem → jedním klikem jsem v logu → opravím compose → redeploy (alerty stacku se při tom samy ztiší).

## Rozhodnutí

Značení: **[U]** = rozhodl uživatel, **[A]** = navrhl agent s důvodem.

### Rozsah a vztahy
- **[U]** Pro autora, open source, MIT.
- **[U]** Coolify se neřeší. V EP infrastruktuře Moorage nahradí jen Dockge na Pi.
- **[A]** Stacky mimo adresář Moorage (rozpoznané labelem `com.docker.compose.project`) jsou **read-only**: logy, stats, zdraví ano, editace ne. Bezpečný default, Dockge to dělá stejně.

### Technologie
- **[U → A]** Kritéria uživatele: bezpečnost, rychlost, kompatibilita s Dockerem, malá velikost. Volba: **Go**.
  - Docker Engine, moby klient i Compose v2 jsou v Go → compose jako knihovna (`github.com/docker/compose/v2`), chování 1:1 s `docker compose up`, žádné CLI v image.
  - Paměťově bezpečný jazyk, statická binárka, image `FROM scratch`, malý supply-chain oproti npm.
  - Odhad: 15–40 MB RAM; image 25–50 MB (compose knihovna), read-only agent ~10 MB.
  - Rust zamítnut: bez kvalitní compose knihovny → stejně volání CLI.
- **[A]** Frontend **Svelte 5 SPA**, vestavěná do binárky přes `go:embed` (nejmenší bundle).
- **[A]** **SQLite** přes `modernc.org/sqlite` (bez CGO → statická binárka). Jen konfigurace, historie sond, události.
- **[A]** Realtime: WebSocket pro logy a operace; hub↔agent **mTLS WebSocket + yamux** (párování agenta jednorázovým tokenem). Původně gRPC – změněno v M3, důvod v „Stav milníků“.
- **[A]** Architektura: jedna binárka, režimy `hub`, `agent`, `all-in-one` (výchozí pro 1 host).

### Bezpečnost
- **[A]** Agent je **jediný** proces se socketem; vystavuje jen allowlist operací (vestavěný „socket proxy“).
- **[A]** Agent má režim `--read-only` (jen logy/stats/zdraví) – pro hosty, kde Moorage nemá nic měnit.
- **[A]** Webový terminál/exec **vypnutý ve výchozím stavu**, zapíná se jen konfigurací agenta (env/flag), nikdy z UI.
- **[A]** Auth: lokální účty (argon2id), **TOTP povinné pro admina**, volitelně OIDC (Authentik, Keycloak…) nebo forward-auth hlavička. Role `admin` / `viewer`.
- **[A]** Notifikace bez spouštění externích procesů (žádné Apprise CLI – zdroj RCE v Kumě), šablony bez přístupu k souborovému systému (SSTI v Kumě).
- **[A]** Sondy blokují cloud metadata endpointy a link-local adresy, pokud je uživatel výslovně nepovolí (advisory Kumy).
- **[A]** Žádná telemetrie.

### Data
- **[A]** Logy se **neukládají** (jako Dozzle). Výjimka: výřez logu přiložený k alertu se uloží s událostí.
- **[A]** Historie sond: surová data 30 dní, hodinové agregace 1 rok.
- **[A]** UI anglicky + čeština od začátku, i18n připravené.

## MVP (v1.0) – milníky

Každý milník je samostatně použitelný.

| # | Milník | Obsah |
|---|---|---|
| M1 | **All-in-one základ** | jedna binárka, auth (lokální + TOTP), seznam stacků/kontejnerů, compose editor, up/down/pull/restart, převod `docker run` → compose, živé logy s hledáním (regex), CPU/RAM stats |
| M2 | **Git verze stacků** **[U]** | každá změna compose/.env = commit (bare repo v datech Moorage), diff v UI, rollback jedním klikem; push na remote až se šifrováním `.env` |
| M3 | **Hub + agenti** **[U]** | rozdělení na hub/agent, mTLS tunel, párování tokenem, read-only režim agenta, přepínání hostů v UI |
| M4 | **Sondy a alerty** | HTTP(s) (keyword, JSON), TCP, ping, DNS, zdraví kontejneru; notifikace webhook, ntfy, Telegram, SMTP, Discord; **alert s kontextem**; **ztišení během nasazení** |
| M5 | **Alerty z logů** **[U]** | pravidla regex/klíčové slovo na kontejner → alert; agent sleduje jen kontejnery s pravidlem |
| M6 | **Watchdog hubu** **[U]** | agenti hlídají hub a při výpadku se domluví: **jedno** nouzové oznámení (a jedno při obnovení) přímo přes nakonfigurovaný kanál (ntfy/Telegram); detail v sekci „Watchdog hubu – koordinace“ |
| M7 | **Veřejná status stránka** **[U]** | stránka bez přihlášení, výběr stacků/sond, vlastní doména, RSS se správným content-type |
| M8 | **Auto-update image** **[U]** | kontrola nových digestů/tagů, notifikace nebo automatický pull+up (per stack), s commitem do gitu a ztišením alertů |
| M9 | **Webový terminál** **[U]** | exec do kontejneru, jen když ho agent povolí; audit log, kdo a kdy |
| M10 ✓ | **OIDC / forward-auth + správa uživatelů** | přihlášení přes OIDC (Authentik, Keycloak…) a hlavičku z reverse proxy (jen od důvěryhodné proxy); mapování na role `admin` / `viewer` (ty už existují od M1) |
| M11 | **Audit, záloha, dlouhodobý uptime** | audit log všech akcí (rozšíření tabulky z M9 na up/down/pull/rollback/smazání/změny nastavení); export/import konfigurace hubu; hodinové agregace výsledků kontrol na 1 rok |
| M12 ✓ | **Aktualizace Moorage z UI** | hub i všichni agenti jedním klikem, podepsané releasy (Ed25519) ověřované na každém uzlu, automatický rollback |
| – | **Vydání v1.0** | git repozitář, bezpečnostní průchod, image, instalační README, changelog |

### Mimo MVP (explicitně)
- Push/heartbeat sondy a hlídání expirace TLS/domén → ROADMAP.
- Ukládání a historické hledání v logech (na to je Loki/VictoriaLogs).
- Swarm, Kubernetes, Podman (Podman možná později).
- Buildy z Dockerfile, git deploy z cizích repozitářů, reverse proxy/TLS (to je PaaS, ne Moorage).
- Správa sítí/volume mimo compose.
- Windows host.

## Watchdog hubu – koordinace (M6)

**[U]** Problém: 10 agentů nesmí při výpadku hubu poslat 10 oznámení. **[U → A]** Zvažována plná mesh síť bez hubu – **zamítnuto**:
- sdílený stav (uživatelé, sondy, alerty, tajné tokeny) by potřeboval konsenzus (Raft) nebo CRDT replikaci – nejtěžší a nejchybovější část distribuovaných systémů;
- kvorum potřebuje ≥ 3 uzly; typická instalace (VPS + Pi) má 2, mesh by tam nic nepřinesla;
- split-brain při rozpadu sítě: obě poloviny alertují, sondují i mění konfiguraci;
- každý uzel by držel všechna tajemství → prolomení libovolného hostu = všechno; nejde dohromady s read-only agentem;
- spojení každý s každým nefunguje přes NAT (Pi doma) bez relay/WireGuard; dnes agent jen volá ven na hub;
- verze protokolu musí sedět mezi všemi uzly, víc RAM, víc údržby.

**[A]** Místo toho: data a řízení zůstávají na hubu, agenti si vyměňují jen signál „vidím / nevidím hub“:
- Hub rozešle agentům seznam ostatních agentů (adresy + klíče pro mTLS). Watchdog spojení jsou jen pro heartbeat, žádná data.
- Hub nedostupný → agent se zeptá ostatních. Oznámení pošle **jediný** živý agent s nejnižším ID (deterministický „reporter“, bez voleb). Stejně jedno oznámení „hub zpět po X min“.
- **Kvorum proti falešným poplachům:** „hub je dole“ až když ho nevidí většina agentů. Nevidí-li ho jen jeden, je to jeho síť → běžný alert „agent X ztratil spojení“, který pošle hub.
- **Malé instalace (1–2 agenti):** kvorum nejde, watchdog se zapíná jen na vybraných agentech (např. hub na Pi, watchdog na VPS → 1 oznámení).
- Pasivní záložní hub (opravdu bez jediného bodu selhání) → ROADMAP.

## Okrajové stavy (shrnutí)
- **Agent offline:** host šedý, data „naposledy viděno“, akce zakázané; sondy z hubu běží dál.
- **Hub offline:** watchdog agentů (M6); UI nedostupné, stacky běží dál (Moorage nikdy není v cestě provozu).
- **Neplatný compose:** validace přes compose knihovnu před uložením, chyba s číslem řádku, nic se nenasadí.
- **Selhaný up/pull:** stav stacku „degraded“, výstup operace uložen, rollback na poslední commit nabídnut.
- **Stack mimo adresář:** read-only štítek s vysvětlením, jak ho převzít.
- **Bouře alertů:** deduplikace a seskupení na stack, ztišení během deploye, ruční „maintenance“.
- **První spuštění:** průvodce – admin účet + TOTP, adresář stacků, první notifikační kanál.

## Náklady
Self-hosted, žádné placené API. Jediné „náklady“ jsou zdroje hostu (RAM – viz níže) a údržba. Čas údržby je hlavní riziko – proto malé závislosti a jedna binárka.

### Limit RAM (změřeno 2026-11-20)
**[U] Limit: 100 MB (PSS)** pro all-in-one ve stálém provozu, i s otevřeným UI a sledováním logů. Měření níže je jen podklad: trojice bez duplicit by vyšla na ~400 MiB; limit 100 MB stanovil uživatel.

Měření: Docker 29.8 ve WSL, stejné stacky (`whoami` s logujícím kontejnerem, `crashy`), 3 kontroly v Kumě i v Moorage (HTTP/DNS/TCP, resp. stack/stack/DNS), SQLite všude. PSS = soukromá paměť + poměrný díl sdílených stránek (nepočítá sdílené knihovny dvakrát jako RSS). 12 vzorků po 15 s.

| | Dockge 1 | Dozzle latest | Uptime Kuma 2 | **součet** | Moorage |
|---|---|---|---|---|---|
| Klid (bez prohlížeče), PSS | 207 MiB | 52 MiB | 192 MiB | **452 MiB** | 39 MiB |
| UI otevřené (stack, živé logy, dashboard), PSS | 292 MiB | 64 MiB | 194 MiB | **549 MiB** | 45 MiB |
| UI otevřené, z toho anonymní (heap) | 167 MiB | 23 MiB | 132 MiB | 322 MiB | 15 MiB |
| `docker stats` v klidu (cgroup bez cache) | 152 MiB | 17 MiB | 143 MiB | 312 MiB | – |

Odečtené duplicity (ve spojené aplikaci běží jednou):
- druhý Node.js runtime s Express + socket.io (Dockge i Kuma): **−39 MiB** (změřeno: holý Node 22 = 22 MiB, s Express + socket.io 39 MiB),
- webový server, auth a Docker klient Dozzle navíc k Dockge: **−10 MiB** (odhad podle klidového heapu Go serveru),
- s otevřeným UI druhý stream logů – Dockge pro terminál spouští `docker compose logs -f` (80 MiB RSS), což dělá i Dozzle: **−75 MiB** (odhad PSS).

Výsledek: klid 452 − 49 ≈ 403 MiB, s UI 549 − 124 ≈ 425 MiB → **~400 MiB pro trojici bez duplicit**; limit Moorage je 100 MB. Režii Dockge za spouštění TypeScriptu přes tsx (launcher 64 MiB + 2× esbuild 27 MiB) jsme neodečítali – není to funkce jiného nástroje.

Moorage je teď na 39–45 MiB PSS, tedy pod polovinou limitu. Limit je strop, který nesmíme překročit; kontroluje se po každém milníku stejnou metodou (skripty `scripts/ram/` – `trio.sh` spustí trojici, `mem.sh` a `pss.sh` – RSS/PSS/anon z `/proc/<pid>/smaps_rollup`).

## Otevřené otázky
1. Licence MIT potvrzena volbou „open source“ – ověřit kompatibilitu závislostí (compose v2 je Apache-2.0 → OK).
2. Kde bude repozitář (GitHub účet/organizace) a registry image (ghcr.io doporučeno).
3. ~~Vlastní vizuální identita~~ – vyřešeno: `.interface-design/system.md` (směr „klidný přístav“, schváleno 2026-11-20).
4. Minimální podporovaná verze Docker Engine (návrh: API 1.44+ / Engine 25+).

## Stav milníků

### M1 – hotovo (2026-11-20)
- Go 1.27, Docker Compose **v5.8 Go SDK** (operace bez CLI), moby client, compose-go validace, SQLite (modernc), coder/websocket; UI Svelte 5 + CodeMirror 6, vloženo přes `go:embed`.
- Ověřeno proti Docker 29.8 (WSL): setup s tokenem + TOTP, přihlášení, seznam stacků (vč. read-only projektu mimo složku), deploy / stop / restart / update / pull / down / delete, živý výstup operací, živé logy s regex hledáním, CPU/RAM, editor s validací, převod `docker run`, světlý/tmavý režim, šířka telefonu.
- Testy: `go test ./...` (auth, TOTP vektory RFC 6238, convert, engine, API guardy), `svelte-check` bez chyb.
- Velikost: binárka 35 MB, image `FROM scratch` 50 MB, UI 150 kB gzip JS.
- **Otevřené:** RAM v klidu ~89 MB RSS (cíl < 50 MB) – změřit heap vs. mapované stránky binárky, zvážit `GOMEMLIMIT`/`GOGC` default a líné načítání Compose SDK.
- Rozhodnutí během M1 **[A]**: modul se jmenuje `moorage` (cesta GitHub doplnit s repozitářem); UI anglicky, i18n až později; default port 5080; stack bez kontejnerů = „Not deployed“.

### M2 – hotovo bez pushe na remote (2026-11-20)
- **[A]** Historie je **bare git repo `<data>/history.git`**, ne repo ve složce stacků: nesahá na data připojená uvnitř složek stacků ani na vlastní git repo uživatele, a nic se tam omylem nezacommituje. Strom: `<stack>/<compose soubor>`, `<stack>/.env`. Zápis přes go-git (čisté Go, žádná `git` binárka v image); repo je platné pro běžný git (`git fsck --strict` čisté).
- Verze vzniká při: vytvoření, uložení, rollbacku, smazání stacku a **změně mimo Moorage** (ssh, editor). Ta se zachytí při startu, před každým uložením/rollbackem/smazáním a při otevření historie a dostane vlastní verzi s autorem „moorage“. Uložení beze změny verzi nevytvoří.
- UI: záložka **History** v detailu stacku – Logbook (dny, čas, typ, zpráva, autor, hash) a diff vybrané verze (řádkový, 3 řádky kontextu, čísla řádků). **Roll back to this version** s volbou „Deploy right after“ (výchozí zapnuto); rollback je sám novou verzí, takže jde vrátit. Rollback je zakázaný, dokud jsou v editoru neuložené změny.
- Smazání stacku: poslední stav zůstává v historii (obnova smazaného stacku z UI zatím není).
- Ověřeno: testy (`history_test.go`: uložení, změna mimo Moorage, diff, rollback, přejmenovaný compose soubor, hunky) + v prohlížeči proti Docker 29.8 (edit → diff → rollback s deployem → kontejner na staré image), desktop i šířka telefonu.
- **[A] Push na remote odložen:** `.env` je v historii (rollback ho musí umět vrátit), takže push by vynesl hesla. Push přijde spolu se šifrováním tajných hodnot (ROADMAP: šifrované `.env`) – lokální repo je na to připravené.
- Velikost: binárka 38 MB (+3 MB go-git). RSS v klidu ~100 MB – otevřený bod s RAM trvá.

### M3 – hotovo (2026-11-20)
- Režimy jedné binárky: `all-in-one` (UI + hub + lokální Docker), `hub` (UI + hub bez Dockeru, pro hub mimo hlídané hosty), `agent` (bez UI, jen Docker tohoto hostu).
- **[A] Transport změněn z gRPC na mTLS WebSocket + yamux:** agent vytáčí ven na hub (funguje za NAT), přes jedno spojení běží víc proudů a hub v nich volá malé HTTP API agenta. Důvody: žádné generování kódu z protobufu, stejné HTTP/streamovací kódy pro lokální i vzdálený host, menší binárka. Vlastní port (`MOORAGE_AGENT_LISTEN`, výchozí `:5081`), aby reverse proxy před UI nemohla odříznout klientský certifikát.
- **Bezpečnost:**
  - Hub má vlastní CA (`<data>/ca`, ECDSA P-256). Join token = tajemství (192 bitů, uložený jen hash, jednorázový, platnost 24 h) + otisk CA → agent ověří, že mluví se správným hubem, ještě než token odešle.
  - Agent si klíč generuje sám a posílá jen CSR; soukromý klíč nikdy neopustí host.
  - Odebrání hostu = smazání z DB hubu + okamžité odpojení; certifikát pak hub odmítne (ověřeno, agent dostane srozumitelnou hlášku).
  - **Agent = vestavěný socket proxy:** vystavuje jen pevný seznam operací se stacky, nikdy surové Docker API. **Read-only se vynucuje na agentovi** (`MOORAGE_READ_ONLY=true`), takže ani kompromitovaný hub na hostu nic nezmění.
- UI: přepínač hostů v postranním panelu (offline = šedá tečka + „seen … ago“, read-only = zámek), stránka **Hosts** (stav, verze, přidání hostu s hotovým `compose.yaml` pro agenta, přejmenování, odebrání). URL obsahují host (`#/h/<host>/stacks/<name>`).
- Historie verzí (M2) žije na agentovi u souborů; hub ji jen zobrazuje.
- Ověřeno: `tunnel_test.go` (párování, mTLS, read-only na straně agenta, chyby přes tunel, logy a operace, jednorázový token, odebrání, odmítnutí hubu s jinou CA) + v prohlížeči proti Docker 29.8 se dvěma procesy: párování přes UI → deploy na agentovi s živým výstupem → logy, stats, historie přes tunel → agent offline → read-only → odebrání. Režim `hub` naběhne bez Dockeru.
- Velikost: binárka 40 MB (+1,6 MB). RSS hubu po pár minutách klidu 47 MB (dříve měřeno 89–100 MB krátce po startu a operacích) – RAM znovu změřit systematicky.
- Známá omezení: obnova (rollback) a historie fungují jen pro spravované stacky; při stejném Dockeru pro hub i agenta (dev) vidí agent stacky hubu jako read-only; hlídání hubu agenty (watchdog) až M6.

### M4 – hotovo (2026-11-20)
- **Sondy běží na hubu** (fungují i při offline agentovi): HTTP(S) se stavovými kódy, klíčovým slovem (i „nesmí obsahovat“), JSON cestou a hodnotou, volitelně neplatný TLS; TCP port; ping (ICMP, nejdřív neprivilegovaný socket, pak raw); DNS (A/AAAA/CNAME/MX/TXT/NS, vlastní resolver, očekávaná hodnota); **Stack** – down, když kontejner skončí, restartuje se ve smyčce nebo je unhealthy; zastavený stack = pauza, ne alert.
- Stavy: up → pending (selhání pod limitem „Alert after“) → down (otevře incident) → up (uzavře, pošle „Back up … after X min“). Při restartu Moorage se otevřené incidenty načtou, nic se nepošle dvakrát.
- **Alert s kontextem:** kontejnery a exit kódy stacku + posledních 20 řádků logů všech kontejnerů (i zastavených) v čase pádu, odkaz do Moorage (`MOORAGE_PUBLIC_URL`). Kontext se ukládá k incidentu a je vidět v detailu kontroly.
- **Ztišení při deployi:** Deploy/Restart/Update/Stop/Down/rollback ztiší kontroly navázané na stack na 3 min; selhání v okně se ukládají jako „silenced“ (žlutý tik), nepočítají se do uptime ani do limitu. Události stacku (`events`) jsou zároveň **kotvy deployů v pásku** a základ budoucí časové osy/audit logu.
- **Seskupení:** alerty jednoho stacku do 10 s = jedna zpráva („DOWN: 2 checks“), down a up odděleně.
- **Notifikace:** webhook (JSON, volitelný Bearer), ntfy (priorita, tagy, click, token), Telegram (čistý text), Discord (vypnuté @mentions, logy nemohou utéct z bloku kódu), e-mail (jen STARTTLS/TLS, ochrana proti vložení hlaviček). Žádné externí procesy ani uživatelské šablony. Tajemství se po uložení už nezobrazí (maska, uložená hodnota zůstává).
- **[A] SSRF ochrana (`netguard`):** sondy i notifikace nesmí na link-local (169.254.0.0/16, fe80::/10), metadata cloudů (100.100.100.200, fd00:ec2::254), multicast ani 0.0.0.0 – kontrola na skutečné IP při spojení (chrání i proti DNS rebindingu). Soukromé sítě (192.168…, 10…) a localhost povolené – homelab je potřebuje. Výjimka per kontrola „Allow link-local“.
- UI: stránka **Checks** (problémy nahoře, příčina, pásek, uptime 24 h, latence), **detail kontroly** (hero uptime 24 h/7 d/30 d, **pásek s kotvami deployů a legendou**, incidenty s kontejnery a logy), **formulář** s typy jako karty, „Test now“ bez uložení, navázání na stack, kanály; **Notifications** se „Send test“; v detailu stacku karta zdraví s pásky (nebo odkaz „Add a check“ s předvyplněným stackem); v postranním panelu odkaz Checks s počtem down.
- Data: surové výsledky 30 dní (mazání každou hodinu). **Hodinové agregace na 1 rok zatím nejsou** – odloženo.
- Ověřeno: `monitor_test.go` (stavy, retries, ztišení, uptime bez ztišených, kotvy, seskupení, kontext, HTTP/keyword/JSON/TCP, SSRF), `notify_test.go` (ntfy/Discord/webhook formát, injekce hlaviček, @everyone, maska tajemství, blokace metadata webhooku) + v prohlížeči proti Docker 29.8 s lokálním webhook přijímačem: test zprávy, alert `crashy` s exit code 3 a logy, výpadek `whoami` mimo Moorage → alert s 20 řádky → Deploy z UI → kotva v pásku a „Back up after 20 s“; přesně jedna zpráva na změnu stavu; telefonní šířka bez přetečení.
- Známé: ping v nerootovém prostředí (WSL, rootless) neprojde – hláška vysvětlí proč a doporučí TCP; v Docker image (root) funguje.

### M5 – hotovo (2026-11-20)
- **Pravidla nad logy jako nový typ kontroly (`Kind = "log"`):** sdílí stavový automat, pásek s kotvami deployů, incidenty, uptime, ztišení při deployi (3 min), seskupení alertů (10 s) i notifikační kanály. Nastavení: host, stack, volitelně kontejner (služba nebo název), vzor (`pattern`, max. 256 znaků), přepínače `regex` (RE2 – lineární čas, bez ReDoS) a `caseSensitive`, práh `threshold` (1–1000, výchozí 1) a klouzavé okno `window` (10 s–24 h, výchozí 5 min).
- **Sledování událostmi (watcher), ne dotazováním:** pro každou zapnutou log kontrolu běží goroutina s životností svázanou s runnerem kontroly (zastaví se při úpravě, vypnutí nebo smazání). Zaznamenává shody do kruhového bufferu (posledních 100 shod, řádky oříznuté na 400 znaků). Pravidelný tik `Probe` jen čte buffer: počet shod za `window` ≥ `threshold` → `down` se zprávou `N matches of "…" in X min: <první řádek>`; po klidovém okně bez shody se kontrola sama vrátí do `up`. Zastavený stack = pauza (`Paused`), ne alert.
- **[A] Po restartu Moorage (nebo úpravě kontroly)** je buffer prázdný – kontrola, která byla dole, zůstane dole, dokud watcher nesledoval celé okno („watching again since …“). Jinak by každý restart poslal falešné „Back up“ a hned nový alert.
- **[A] Ztišení při deployi:** shody, které přijdou během 3min ztišení, se ukážou jako ztišené selhání, ale **po skončení ztišení se nepočítají**. Chyby při startu po deployi (DB ještě neběží…) tak nespustí alert 3 minuty po deployi; trvalý problém (crash loop, chyba dál tiskne) alert spustí, stejně jako u kontroly Stack.
- **[A] Filtrování přímo na agentovi (`Host.WatchLogs`, `GET /logs/watch`):** agent sám zkompiluje filtr a přes mTLS tunel posílá jen shodné řádky; neshodné řádky host vůbec neopustí. Hub filtr aplikuje ještě jednou pro obranu do hloubky a při 404 od staršího agenta automaticky spadne zpět na `/logs` s filtrováním na hubu. Read-only agent endpoint povoluje.
- **[A] Žádné duplicity ani přehrávání historie:** `Engine.WatchLogs` si pro každý kontejner pamatuje čas posledního přečteného řádku a čte od pozdějšího z (start sledování, poslední řádek + 1 ns) – nikdy `Tail = "all"`. Seznam kontejnerů prochází každých 5 s (a hned po konci streamu, nejdřív za 1 s): běžící kontejnery sleduje, nové po Deploy/Update přibere. **Kontejner, který neběží, přečte jednou bez sledování, kdykoli se změní jeho stav nebo čas startu** (`StartedAt` z inspect) – zachytí se tak i běh kratší než 5 s (pád hned po startu, crash loop). Watcher v monitoru se při chybě hostu připojuje znovu s backoffem 2 s → 60 s.
- **Kontext alertu a „Test now“:** `AlertContext.Matches` nese až 10 posledních shodných řádků (zobrazí se v notifikaci nad odděleným blokem posledních 20 řádků stacku i v detailu incidentu; v Discordu zůstávají uvnitř bloku kódu). `POST /api/checks/test` pro `log` nespouští watcher, ale otestuje vzor nad `TailLogs(stack, 200)` a vrátí počet shod i ukázku řádků.
- **UI:** karta typu **Log** v `CheckForm.svelte` (host, stack, volitelný kontejner, vzor s přepínači „Regular expression“ a „Case sensitive“, inline kontrola syntaxe regexu, „Alert when N matches within M minutes“, ukázka shod v „Test now“) a zobrazení vzoru a nalezených řádků v `CheckDetail.svelte` a `ChecksView.svelte`.
- Ověřeno: `engine_test.go` (filtr: podřetězec vs. regex, velikost písmen, neplatný regex, limit 256 znaků), `api_test.go` (`validateCheck` pro `log`), `monitor_test.go` (práh a okno, automatické zotavení po klidovém okně, ztišení při deployi, zastavený stack = pauza, limit kruhového bufferu 100 položek, `Matches` v kontextu alertu, reconnect bez duplicit), `tunnel_test.go` (`WatchLogs` přes mTLS tunel na read-only agentovi), `svelte-check` (0 chyb) a `vite build`.
- Ověřeno v prohlížeči proti Docker 29.8 (kontrola přes UI, webhook přijímač): `whoami`/`ticker` vzor „warn slow response“ 2× za 1 min → alert s 6 řádky bez duplicit + 20 řádků stacku; `ticker` znovu vytvořen mimo Moorage (nové ID) → sledování pokračuje do 5 s, opět 6 shod za minutu; `crashy` (běh < 1 s, exit 3) mimo Moorage → přesně 1 shoda „FATAL“ ze stderr; restart Moorage → žádné falešné „Back up“.
- RAM: 48,5 MiB PSS (19 MiB anon) s otevřeným UI, 5 kontrolami a 2 log pravidly – pod limitem 100 MB.
- Revize po Geminim (Claude): opraveny duplicity po restartu kontejneru, ztráta řádků krátkých pádů, „Back up“ po restartu Moorage, počítání shod z doby deploye; JS kontrola regexu už nehlásí chybu u RE2 příznaků `(?i)`; název se při přepnutí na Log nepřebírá ze stacku.
- Známá omezení: shody se počítají podle času příchodu na hub, ne podle času v logu; při výpadku agenta (offline) kontrola hlásí „host offline“ a řádky z doby výpadku se nedoženou.

### M6 – hotovo (2026-11-20)
- **Koordinovaný watchdog hubu (bez konsenzu / Raftu):** řízení a data zůstávají na hubu (`watchdog` tabulka v SQLite + příznak `watchdog`, `watchdog_addr` a `watchdog_cert` u agentů). Hub při každém připojení agenta i po změně nastavení nebo kanálu posílá zúčastněným agentům konfiguraci (`PUT /watchdog`): povolení, `grace` (60–3600 s, výchozí 120 s), okno údržby (`maintenance_until`), název hubu, `MOORAGE_PUBLIC_URL`, vybraný kanál včetně jeho tajného klíče, seznam zúčastněných peerů a peer certifikát. Agent konfiguraci i stav výpadku ukládá do `<data>/agent/watchdog.json` s právy `0600` (přežije restart agenta během výpadku hubu); při vypnutí účasti agent soubor smaže.
- **[A] Oddělený peer mTLS certifikát (`watchdog.key`):** běžný certifikát agenta je jen klientský (`ExtKeyUsageClientAuth`), proto si agent vygeneruje vlastní klíč `watchdog.key` (nikdy neopustí host), hub si přes tunel vyžádá CSR (`POST /watchdog/csr`) a podepíše peer certifikát s `ExtKeyUsageServerAuth` + `ExtKeyUsageClientAuth`, CN = ID agenta, `DNSNames` = `[ID agenta]`, platnost 1 rok (automatická obnova při připojení, zbývá-li < 30 dní).
- **Peer kanál (`GET /watchdog/status` přes TLS 1.3 mTLS):** jediný peer endpoint (`MOORAGE_WATCHDOG_LISTEN`, inzerovaná adresa `MOORAGE_WATCHDOG_ADDR`). Server přijme jen klientský certifikát podepsaný CA hubu, jehož CN je v aktuálním seznamu peerů; klient ověřuje `ServerName` = očekávané ID peera. Odebraný agent ze seznamu vypadne při dalším pushi a ostatní ho odmítnou. Každých 15 s (timeout 5 s) si zúčastnění agenti vymění jen `id`, `hubReachable`, `hubDownSince`, `reportedDown` a `configVersion`.
- **Kvorum a deterministický reporter (`DecideWatchdog`):** hub je považován za nedostupný, když více než `N/2` zúčastněných agentů (včetně sebe, s čerstvým stavem < 45 s) nevidí hub déle než `grace` (`N=1` rozhoduje sám, `N=2` musí výpadek vidět oba; při `N=2` a jednom nedostupném agentovi kvorum nevznikne – zdokumentováno jako záměr). Oznámení pošle **jediný** živý agent z hlasujících s **nejnižším ID** (`ag_…`). Výpadek je identifikován nejstarším `hubDownSince` oříznutým na minutu (`reportedDown`; ostatní agenti hodnotu přebírají od reportera, nepočítají vlastní); před odesláním reporter zkontroluje `reportedDown` peerů, takže ani opožděný agent s nižším ID, ani restartovaný reporter nepošle zprávu podruhé.
- **[A] Obnova („hub is back“) a výpadek jednotlivého agenta:** po návratu hubu pošle zprávu o obnovení původní reporter (nebo živý agent s nejnižším ID, který zaznamenal `reportedDown` peera, pokud původní reporter mezitím sám vypadl). Kdo obnovení pošle, ukazuje to ostatním (`recoverySent`), takže původní reporter, který se vrátí později, už druhé „hub is back“ nepošle. Během `maintenance_until` se nic neposílá a výpadek, který začal během údržby, se nehlásí ani po jejím skončení. Naopak když spojení ztratí jen jeden zúčastněný agent déle než `grace` a hub běží, **hub** pošle přes kanál watchdogu běžný alert `Host <name> is offline since …` / `Host <name> is back after …`.
- **UI (stránka Hosts – karta „Hub watchdog“):** zapnutí/vypnutí, výběr kanálu s jasným upozorněním, že tajemství vybraného kanálu se kopíruje na zúčastněné agenty, `grace` v minutách, přepínače účasti jednotlivých agentů (zobrazují peer adresu nebo důvod, proč agent nemůže hlídat: chybí `MOORAGE_WATCHDOG_ADDR` při 2+ agentech, stará verze bez `/watchdog`, offline), tlačítko **Send test via agent** (`POST /api/watchdog/test` → `POST /watchdog/test` přímo z agenta) a **Pause for 1 h** / **End pause** (`POST /api/watchdog/maintenance`).
- Ověřeno: `watchdog_test.go` (10 scénářů čisté funkce `DecideWatchdog` vč. `N=1`, `N=3`, 1 ze 3 bez kvora, výpadku nejnižšího ID, výpadku kratšího než `grace`, restartu reporteru během výpadku i za klidu, pozdního návratu nejnižšího ID a převzetí recovery, údržby během i po skončení, ignorování stavu > 45 s a `N=2` s nedostupným peerem; test práv `0600` a smazání `watchdog.json`; mTLS test odmítnutí cizí CA i odebraného agenta; integrační test 1 hubu a 3 agentů v procesu: test přes agenta → stop hubu → přesně 1 down zpráva → restart hubu se stejnou CA a DB → přesně 1 recovery zpráva), `monitor_test.go` (`TestWatchdogHostOfflineAndRecoveryAlert`), `svelte-check` (0 chyb) a `vite build`.
- Ověřeno naostro (Claude): hub + 3 agenti jako samostatné procesy (`scripts/dev/wd-agent.sh`), webhook přijímač, tolerance 60 s. Nastavení přes UI → `watchdog.json` 0600 u všech tří → test přes agenta došel. Zastavení hubu → **přesně 1** oznámení od agenta s nejnižším ID („seen by 3 of 3 agents“), ostatní si zapsali `observedBy`. Restart reportera během výpadku → nic navíc. Reporter zastaven, hub zpět → obnovení poslal další agent; reporter se vrátil → **nic navíc**. Jeden agent odříznutý od hubu (špatná adresa) → agenti nic, hub „Host wd1 is offline since 18:38“ a po návratu „Host wd1 is back after 2 min“.
- Revize po Geminim (Claude), chyby nalezené naostro: (1) vrácený reporter posílal druhé „hub is back“ → `recoverySent` + regresní test; (2) „Host … is offline since“ ukazoval čas připojení agenta před výpadkem hubu → počítá se nejdřív od startu hubu; (3) „back after 1 min“ u 2min výpadku kvůli zaokrouhlení ID nahoru → oříznutí; (4) test přes agenta uváděl ID místo jména.
- RAM: hub 21 MiB anon, agent 14–16 MiB anon (PSS se dělí mezi 4 procesy téže binárky, ~21–31 MiB) – hluboko pod limitem 100 MB.
- Známá omezení: hub po vlastním restartu nezná dřívější „offline“ alerty agentů a může je poslat znovu; bez `MOORAGE_WATCHDOG_ADDR` agent inzeruje adresu posluchače (např. `[::]:5082`), kterou ostatní nedosáhnou – při 2+ agentech ji nastavte. Když je původní reporter při návratu hubu živý pro hub, ale nedostupný pro ostatní agenty na peer portu, mohou přijít dvě zprávy o obnovení.
- Známá omezení: při `N=2` zúčastněných agentech a současném výpadku hubu i jednoho z agentů (např. společná lokalita) zbývající agent nemá kvorum (> 2/2) – pro 2 lokality, kde hub běží v jedné z nich, má hlídat pouze vzdálený agent (`N=1`).

### M7 – hotovo (2026-11-22)
- **Veřejné status stránky bez přihlášení (`internal/status`):** podpora více stránek (`status_pages` a `status_page_items` v SQLite), každá s vlastním výběrem kontrol nebo celých stacků, vlastním veřejným názvem (`display_name`), volitelným členěním do sekcí, přepínačem publikování (`published`) a historie incidentů (`show_incidents`) a volitelnou vlastní doménou (`domain`).
- **[A] Serverové vykreslování (`html/template`) a nulový únik interních dat:** veřejná stránka i RSS kanál se skládají výhradně z dedikovaného DTO `PublicPage`, do kterého vůbec nevstupují interní názvy kontrol, typy, cíle (URL, IP, porty, DNS), ID ani názvy hostů, názvy stacků a kontejnerů, zprávy sond (`Result.Msg`), příčiny incidentů (`Cause`), logy (`Context`), latence ani kotvy deployů. Veškerý text je čistý (žádný Markdown ani vlastní HTML/CSS/JS → vyloučeno XSS). Přísná `Content-Security-Policy` (`default-src 'none'; style-src 'self'; script-src 'self'…`), žádné čtení ani nastavování cookies, povoleny jen metody `GET`/`HEAD`.
- **[A] Fonty Onest a JetBrains Mono bez duplikace v binárce:** `status.New` při startu vyhledá už vložené `*.woff2` soubory v `internal/web/dist/assets` pomocí `fs.Glob` a vystaví je pod `/status-assets/*.woff2`. Časy se vykreslují jako `<time datetime="RFC3339">` v UTC a malý statický skript `/status-assets/status.js` (bez inline JS) je přepíše do lokální zóny návštěvníka. Výchozí motiv je světlý (`sailcloth`), tmavý přes `prefers-color-scheme: dark`.
- **Izolace vlastní domény a volitelný oddělený port (`MOORAGE_STATUS_LISTEN`):** na hlavním portu běží stránky na `/status/<slug>` a `/status/<slug>/feed.xml`. Pokud hlavička `Host` (bez portu, case-insensitive) odpovídá `domain` některé status stránky, Moorage na tomto hostu obslouží **výhradně** `/` (stránka), `/feed.xml` (RSS) a `/status-assets/*`; vše ostatní (včetně `/api/*`, SPA a jiných `/status/<jiný-slug>`) vrátí 404. Doména se nesmí shodovat s hostem `MOORAGE_PUBLIC_URL`. Volitelný `MOORAGE_STATUS_LISTEN` (např. `:5085`) spustí samostatný HTTP server, který nikdy neobsluhuje admin UI ani `/api/*`. Neexistující slug, nepublikovaná stránka i neznámá doména vracejí identickou 404 odpověď.
- **Denní pásek (30 UTC dní), stavy a cache bez stampede:** jeden agregační SQL dotaz (`GROUP BY check_id, ts / 86400000`) nad existujícím indexem `check_results(check_id, ts)` spočítá 30 denních sloupců; ztišené výsledky (`silenced`) nesnižují uptime a den s pouze ztišenými výsledky je žlutý (`Maintenance`), ne červený. Položka typu stack agreguje všechny zapnuté kontroly stacku: stav = nejhorší z nich (`Down > Maintenance > Operational > Paused > No data`), uptime (24 h, 30 d i za den) = **nejnižší** z jejich uptime a překrývající se incidenty jejích kontrol se zobrazí jako **jeden** výpadek. Seznam stránek (slug, doména, položky) drží služba v paměti a znovu ho načte jen po úpravě, takže směrování – i každý požadavek admin UI, který se musí porovnat s doménami status stránek – nesahá do SQLite. Veřejná data každé stránky se cachují na 60 s (`Cache-Control: public, max-age=60`) se singleflight zámkem (100 souběžných požadavků sestaví data jednou) a invalidací při úpravě stránky nebo kontroly.
- **RSS 2.0 (`application/rss+xml; charset=utf-8`):** generováno přes `encoding/xml` s `atom:link rel="self"`, stabilními `<guid isPermaLink="false">` (`inc-<id>-<otisk veřejného názvu>-down` / `-up`; nezávisí na pořadí položek, takže přeřazení nezpůsobí duplicity ve čtečce), jedna položka při začátku výpadku a jedna při obnovení (probíhající výpadek má jen položku `down`; krátké výpadky < 60 s se vypisují v sekundách, nikdy „0 min“).
- **Admin UI:** položka **Status pages** v postranním panelu (`StatusPagesView.svelte`, `StatusPageForm.svelte`) – seznam se stavem, štítkem Published/Draft a kopírováním URL; editor s výběrem kontrol a stacků (stacky bez kontrol jsou neaktivní s nápovědou), řazením, sekcemi a **Preview** neuložených změn (`POST /api/status-pages/preview` – nic neukládá). Doména se při uložení převede na malá písmena.
- Ověřeno: `status_test.go` (test nulového úniku tajných názvů/URL/hesel/logů v surových bajtech HTML, RSS i preview JSON; HTML/XML escapování `<script>`; routing vlastní domény, `STATUS.example.com:443`, blokace `/api/*` a SPA na doméně i na `MOORAGE_STATUS_LISTEN`, identická 404 pro draft/neznámý slug/doménu; validace kolize s `MOORAGE_PUBLIC_URL`; mapování stavů vč. `pending` → Operational a `silenced` → Maintenance; hranice UTC dne `23:59:59` vs. `00:00:00`; kaskádové smazání kontroly; RSS content-type, XML parsování, stabilní GUID, 40s a 2m trvání; 100 souběžných požadavků = 1 DB build, invalidace po editaci, obnova po TTL; CSP a zákaz POST/Set-Cookie), `svelte-check` (0 chyb) a `vite build`.
- Ověřeno naostro (Claude) na dev hubu s reálnými daty (whoami, crashy, DNS, log pravidla) a `MOORAGE_STATUS_LISTEN=127.0.0.1:5085`: stránka vytvořená přes UI s názvy `Homelab <b>status</b>` a `DNS "x" & <script>…`, doménou `status.test` a stackem `crashy` jako „Worker“. `curl`: slug, feed, doména (i `STATUS.TEST:443`) → 200; na doméně `/api/session`, `/index.html`, SPA assety a cizí slug → 404; status port nikdy neobslouží API ani SPA; POST → 405; přísná CSP, žádné `Set-Cookie`. V HTML ani feedu se neobjevil žádný interní řetězec (názvy kontrol, stacků, hostů, ID agentů, cíle, logy), escapování drží. Feed čtený v `encoding/xml`, prohlížeč: světlý i tmavý režim, 375 px bez vodorovného posunu, fonty Onest/JetBrains Mono načtené, časy přepsané do místní zóny.
- Revize po Geminim (Claude): (1) **Preview ukládal** – u publikované stránky tak zveřejnil rozpracované změny → samostatný náhled neuložených dat; (2) RSS stránky s vlastní doménou odkazoval na `MOORAGE_PUBLIC_URL`, tedy **prozrazoval adresu admin UI** → vlastní doména má vždy přednost; (3) každý požadavek (i každé volání admin API) četl doménu z SQLite → index stránek v paměti; (4) stack se dvěma kontrolami ukazoval jeden výpadek dvakrát a (5) mrtvý stack (kontrola stacku 0 %) vedle zdravého log pravidla jako „50 %“ → sloučení incidentů a nejnižší uptime; (6) GUID ve feedu závisel na pořadí položek → otisk názvu; (7) doména s velkými písmeny se odmítala → převod na malá; (8) fonty bez `unicode-range` → prohlížeč stahoval latin i latin-ext; (9) odkaz „Open public page“ sledoval neuložený formulář. Regresní testy: `TestStatusPageReviewRegressions`, `TestStatusPageDraftPreviewDoesNotSave`.
- **[A] RAM – výchozí měkký limit Go runtime 48 MiB (`cmd/moorage/main.go`, přepíše `GOMEMLIMIT`):** při měření se ukázalo, že hub po startu (nesouvisí s M7) drží první ~2,5 min až 78 MiB anonymní paměti (107 MiB RSS – nad limitem), než ji vynucené GC sníží na ~19 MiB; Go sbírá smetí až při zdvojnásobení heapu. S limitem hub drží ~17–19 MiB anon (RSS ~48 MiB) od startu. Limit je měkký – větší živá data znamenají jen častější GC, nikdy pád. Zátěž status stránky (600 souběžných požadavků) přidá ~4 MiB, které se do 20 s vrátí.
- Známá omezení: denní sloupce se počítají ze surových výsledků (retence 30 dní); stránka bez vlastní domény odkazuje v RSS přes `MOORAGE_PUBLIC_URL` (adresa admin UI – pro veřejné publikování použijte vlastní doménu); pokud není nastavena vlastní doména ani `MOORAGE_PUBLIC_URL`, absolutní odkazy v RSS se skládají z `http://<Host>/status/<slug>` (za HTTPS reverse proxy nastavte `MOORAGE_PUBLIC_URL` nebo vlastní doménu stránky).

### M8 – hotovo (2026-11-22)
- **Kontrola nových digestů (Part A) a novějších verzí tagů (Part B):** agent kontroluje lokální image a vzdálený registr (`CheckImages`, `StackImages`, `RollbackImages`, `ChangeImage` na rozhraní `host.Host`), hub plánuje kontroly, drží politiky a výsledky v SQLite (`update_settings`, `stack_update_policies`, `image_checks`, `image_updates`) a odesílá dávkované notifikace.
- **Part A – nový digest stejného tagu (`internal/engine/images.go`, `internal/updater`):** porovnání lokálního `RepoDigests` / běžícího kontejneru (`ImageInspect`) se vzdáleným digestem (`DistributionInspect`). Pevná sada stavů: `up-to-date`, `update-available`, `pulled-not-applied`, `pinned` (`@sha256:`), `built` (`build:`), `local-only` (bez `RepoDigests`), `unreachable` (s důvodem: `unauthorized`, `not found`, `rate limited`, `timeout`, `credential helper not supported`, `blocked address`) a `unsupported` (starší agent vrátí 404). Deduplikace vzdálených dotazů podle reference image napříč stacky na hostu, max. 4 souběžné požadavky na hosta s timeoutem 20 s.
- **Přihlašovací údaje k registrům bez externích procesů:** čtení pouze přímých záznamů `auths` z `$DOCKER_CONFIG/config.json` (nebo `~/.docker/config.json`). Externí binárky `credsStore` a `credHelpers` se nikdy nespouštějí (pokud registr vyžaduje helper, služba skončí jako `unreachable` s jasným důvodem `credential helper not supported`).
- **Politiky per stack (`off`, `notify`, `auto`) a časové okno aplikace:** výchozí politika je `notify` (žádný stack se nerestartuje sám od sebe, dokud ho admin nepřepne na `auto`). V režimu `auto` hub spustí `pull + up` pouze uvnitř nastaveného okna (`03:00–05:00` v časové zóně hubu, podpora oken přes půlnoc i DST s ` _ "time/tzdata"` pro `FROM scratch`), nejvýše jednou za okno, a nikdy pro zaneprázdněný (`busy`), zastavený (`running == 0`), nespravovaný nebo read-only stack. Tlačítko **Check now** (s limitem 1× za minutu na stack) pouze zkontroluje stav a nikdy nespustí auto-update.
- **Ztišení kontrol, historie aktualizací a jednoklikový rollback:** před každou aktualizací se zaznamená `StackEvent(..., "update", "auto-update", ...)` (3min ztišení kontrol stacku, kotva na Soundings stripu) a staré/nové ID + digest image do `image_updates` (retence 90 dní). Pokud při auto-update selže samotný `pull`, běžící kontejnery se nezměnily – nezapíše se falešný záznam do `image_updates` a chybová notifikace odejde jen jednou pro daný vzdálený digest. Pokud selže `up`, uloží se záznam `up-failed` s posledními 20 řádky výstupu. **Roll back update** přetaguje image zpět na předchozí lokální ID (`ImageTag`), nebo pokud bylo smazáno (`docker image prune`), stáhne image podle původního digestu (`<repo>@<oldDigest>`), spustí `up` bez pullu, označí záznam jako `rolled-back`, přepne politiku stacku na `notify` a uloží vadný digest do `skipped_digests`, aby ho další okno znovu nenasadilo.
- **Part B – novější tagy verzí (notify-only + jednoklikový přepis `compose.yaml`):** striktní parser verzí `^v?\d+(\.\d+){0,2}(-[a-z0-9.]+)?$` (odmítá `latest`, `stable`, `alpine`, datumové tagy `20260419` i pre-release sufixy `-rc1`, `-beta`). Kandidát musí mít stejný prefix `v`, stejný počet číselných složek a stejný sufix (`-alpine`), rozdělený na nejvyšší verzi stejné major řady (`newerTag`) a vyšší major řady (`newerMajor`). Seznam tagů se čte přes Registry HTTP API v2 (`GET /v2/<repo>/tags/list?n=1000`) přes `netguard` (SSRF ochrana), s podporou `WWW-Authenticate: Bearer` token flow, stránkováním `Link` hlavičkou (strop 10 000 tagů) a proudovým čtením JSON tokenů v konstantní paměti. Tlačítko **Change to `<tag>`** provede cílený textový přepis řádku `image:` dané služby v `compose.yaml` (zachová uvozovky, komentáře, odsazení i CRLF; odmítne `${TAG}` a duplicitní hodnoty s hláškou `Edit compose.yaml manually` a otevře editor), zvaliduje compose, uloží git verzi (`Change <service> image <repo> <old> → <new>`) s přihlášeným uživatelem jako autorem a spustí akci `update`.
- **UI:** nová stránka **Updates** v postranním panelu (`UpdatesView.svelte`), karta **Images** na přehledu stacku (`StackDetail.svelte`) a zobrazení záznamů `image_updates` v Logbooku (`HistoryView.svelte`).
- **[A] Aktualizace digestu ≠ git verze.** `compose.yaml` ani `.env` se nemění, prázdný commit by zahltil historii a jeho obnovení by starou image stejně nevrátilo. Historií aktualizace je záznam v `image_updates` + událost stacku (Logbook, kotva na stripu). Změna tagu (Part B) soubor mění, takže je git verzí.
- **[A] Žádné credential helpery.** Moorage nespouští externí procesy (viz Kuma a Apprise RCE). Registr s helperem je `unreachable: credential helper not supported`; README popisuje připojení vlastního `config.json` s `auths`. Registr bez záznamu se kontroluje anonymně i při globálním `credsStore` (Docker Desktop ho nastavuje vždy).
- **[A] Selhaný start nové image se v dalším okně nezkouší znovu** (`up-failed` → digest do `skipped_digests`), jinak by se stack každou noc restartoval do stejné chyby. Selhaný pull se zkouší dál, ale ohlásí se jednou na digest.
- **[A] Přeskočený digest blokuje auto-update celého stacku.** Akce `update` stahuje všechny služby, takže aktualizace jiné služby by vrácenou image nainstalovala znovu.
- Ověřeno: `images_test.go` a `updater_test.go` (všech 8 bodů Part A a 3 body Part B ze sekce 6), `go vet ./...`, `go test ./...`, `svelte-check` (0 chyb) a `vite build`.
- **Ověřeno živě** (review, lokální `registry:2` na 127.0.0.1:5000, Docker 29 s containerd image store):
  - stavy `up-to-date`, `pulled-not-applied`, `update-available`; tagy `1.1` / `2.0 (major)` z HTTP registru, `v1.12` a `1.38` z Docker Hubu (bearer flow), `1.0-alpine` bez kandidáta;
  - jedno upozornění na webhook v okně, druhý běh nic; auto-update v okně (záznam, událost, upozornění);
  - návrat zpět i u `pull_policy: always`; staré ID po přesunu tagu v containerd store neexistuje, takže proběhl pull podle digestu;
  - `Change to 1.1` (uvozovky i komentář zachovány, git verze s autorem); `Update now` s výstupem v panelu; zastavený registr → `pull-failed`;
  - starší agenti → `unsupported`; nespravovaný stack s 12 službami za 29 s;
  - šířka 375 px bez vodorovného posunu; RAM hubu po naplánované kontrole 18 MiB anon / 48 MiB RSS (beze změny).
- **Review (Claude) – opraveno 12 chyb:**
  1. Nespravované stacky (jména mimo `nameRe`, např. `EmptyPockets`) se vůbec nekontrolovaly. Teď se čtou obrazy z kontejnerů.
  2. Upozornění „Updated …“ vypisovalo i nezměněné služby (`8403682 → 8403682`); texty jsou teď podle plánu (`N image updates available`, `whoami · web: … has a new build`, `Update of X failed: <chyba>`).
  3. Po aktualizaci, změně tagu nebo návratu zpět zůstával uložený starý stav kontroly (UI dál nabízelo `Change to 1.1` u stacku na 1.1). Teď se stack po operaci zkontroluje znovu.
  4. Globální `credsStore` dělal ze všech veřejných image mimo Docker Hub `unreachable`.
  5. Záznam se přepnul na `rolled-back` už při startu návratu, i když pak selhal. Teď až po úspěchu.
  6. Návrat zpět spouštěl `up` s pull policy služby, takže `pull_policy: always` stáhl zpět vadnou image. Teď se použije interní `up-local` (pull `never`).
  7. Přeskočený digest jedné služby nezabránil auto-update stacku kvůli jiné službě (`update` stahuje všechny).
  8. Neúspěšný `up` nové image se opakoval každé okno.
  9. Událost `update` (ztišení + kotva) se zapsala i když akce vůbec nezačala (`busy`).
  10. „Update now“ čekalo v HTTP požadavku na konec pullu a s požadavkem se rušil i zápis záznamu. Teď vrací operaci (výstup v panelu) a záznam se dopíše na pozadí.
  11. Chyběl jitter ±10 % intervalu.
  12. Služby stacku se kontrolovaly po jedné (12 služeb > 60 s = timeout volání agenta). Teď paralelně, se sdíleným dotazem pro stejnou referenci; volání agenta má na kontrolu 5 min.
  - Drobnosti: digest se bere z `RepoDigests` vlastního repozitáře (ne prvního v seznamu), opakované chybové řádky v logu, „Check all now“ už není druhé primární tlačítko.
- Známá omezení: ruční „Check now“ souběžně s naplánovaným během může přepsat značku ohlášení (vzácné, nanejvýš jedno upozornění navíc); služba s lokální image bez digestu ve stejném stacku může nechat `update` selhat už na pullu (neověřeno živě); po smazání stacku zůstávají jeho řádky `image_checks` (UI je nezobrazuje); bez automatického rollbacku při selhání kontrol po update a bez automatického povyšování tagů verzí (obojí v `ROADMAP.md`); externí `credsStore`/`credHelpers` binárky nejsou z bezpečnostních důvodů spouštěny; u Docker Hubu anonymní kontroly podléhají rate limitům, dokud nejsou v `config.json` uloženy přihlašovací údaje v `auths`.

### M9 – hotovo (2026-11-22)
- **Webový terminál do kontejnerů (`internal/engine/terminal.go`, `internal/api/terminal.go`, `TerminalPanel.svelte`):** interaktivní TTY relace (`docker exec`) v prohlížeči přes xterm.js + fit addon (načítané líně přes dynamický `import()`, takže hlavní bundle neroste o xterm.js). Data tečou přes WebSocket (`GET /api/hosts/{host}/stacks/{name}/terminal`) – binární rámce pro surové bajty terminálu v obou směrech, textové JSON rámce pro řídicí zprávy (`resize`, `exit`, `error`). U vzdálených agentů hub přeposílá rámce přes WebSocket uvnitř existujícího yamux tunelu (`GET /stacks/{name}/terminal`).
- **[A] Ve výchozím stavu vypnuto, zapíná se výhradně konfigurací agenta (`MOORAGE_TERMINAL=true`):** UI ani hub nemají žádný přepínač ani API pro zapnutí terminálu. Rozhodnutí vynucuje samotný agent (nebo lokální host v `all-in-one` režimu): pokud `MOORAGE_TERMINAL` není `true`, endpoint na agentovi vrátí `403 Forbidden` (`terminal_disabled`), takže ani kompromitovaný hub neotevře shell na hostu, jehož správce to nepovolil. Agent reportuje stav v `AgentInfo.terminal` (`host.Entry.terminal`); pokud je terminál vypnutý, UI tlačítko vůbec nezobrazí. Starší agenti bez endpointu (404) vrátí srozumitelnou zprávu `Update the agent to use the terminal`.
- **[A] Read-only režim má vždy přednost (`MOORAGE_READ_ONLY=true`):** read-only agent nikdy nepovolí terminál, i když je zároveň nastaveno `MOORAGE_TERMINAL=true` (při startu vypíše varování do logu). Read-only znamená, že Moorage na hostu nic nemění, zatímco shell v kontejneru umožňuje měnit cokoliv.
- **[A] Žádné libovolné příkazy a pouze kontejnery daného stacku:** prohlížeč vybírá pouze ze tří pevných shellů (`sh` → `/bin/sh`, `bash` → `/bin/bash`, `ash` → `/bin/ash`, cokoliv jiného → 400), spouštěných pod výchozím uživatelem kontejneru s `TERM=xterm-256color` (žádné `Privileged`, žádný přepis `User`). Agent ověřuje, že cílový kontejner skutečně patří do stacku z URL pomocí `projectContainers(stack)` a že běží (`state == "running"`), takže nelze otevřít shell v cizím kontejneru ani v samotném kontejneru Moorage. Pokud obraz zvolený shell nemá (např. `busybox` bez `bash` nebo distroless/`scratch` bez `/bin/sh`), terminál zobrazí `This container has no /bin/bash. Try sh.` (resp. `…has no /bin/sh`).
- **[A] Auditní záznam bez ukládání napsaných příkazů a výstupu (`terminal_sessions`):** příkazy i výstup terminálu obsahují hesla, tokeny a tajemství, proto se nikdy nezaznamenávají ani nelogují. Při startu relace se do SQLite (`terminal_sessions`, retence 1 rok) zapíše uživatel, klientská IP (ze `RemoteAddr`, shodně s rate limiterem přihlášení), host, stack, kontejner, služba, shell a čas startu; při ukončení se doplní čas konce, důvod (`exit`, `closed`, `idle`, `limit`, `revoked`, `error`, `hub-stopped`) a návratový kód. Při startu hubu se případné neukončené záznamy po pádu uzavřou s důvodem `hub-stopped`. Každé otevření zapíše také `StackEvent(host, stack, "terminal", user, "<service> · <shell>")` (druh `terminal` není v `DeployKinds`, takže neztišuje kontroly ani nepřidává kotvu deploye) a záznam do serverového logu (`slog`) při startu i konci. Seznam relací je viditelný pro admina v Logbooku stacku i globálně na stránce **Hosts**.
- **[A] Limity, odvolání relace a úplný úklid:** max. 4 souběžné terminály na hub (5. dostane `Too many open terminals`), limit čtení WebSocketu 64 KiB (pro vložení schránky), 16KiB buffer s backpressure, idle timeout 15 min (resetovaný pouze vstupem z prohlížeče, nikoliv výstupem běžícího příkazu), tvrdý limit 8 h na relaci. Každých 30 s se znovu ověřuje platnost session cookie a role `admin` (odhlášení, expirace nebo změna role na `viewer` ukončí otevřený terminál s důvodem `revoked`; otevřený terminál neprodlužuje platnost session). Uzavření kterékoliv strany (prohlížeč, agent, timeout, odvolání session, vypnutí hubu nebo agenta) ukončí shell i vše, co z něj běží (viz další bod).
- **[A] Ukončení shellu druhým, pevným execem.** Docker nemá „kill exec“ a zavření spojení TTY execu shell **neukončí** (ověřeno na Docker 29: `/bin/sh` i `sleep` běžely dál). Každá relace proto dostane proměnnou `MOORAGE_TERMINAL_SESSION=<náhodné id>`, kterou dědí všechny procesy z ní spuštěné. Při ukončení, pokud shell ještě běží, agent spustí v kontejneru pevný skript (nikdy nic z prohlížeče), který těmto procesům pošle `HUP` a po sekundě `KILL`. Vyžaduje `cat` v obrazu (busybox, debian, alpine ho mají). Zastavující se agent i hub ukončí otevřené shelly před koncem procesu.
- **[A] Odmítnutí až po upgradu WebSocketu.** Prohlížeč nevidí HTTP status odmítnutého WebSocketu, takže hub nejdřív upgraduje (cross-origin a viewer dál dostanou HTTP 403) a důvod (vypnutý terminál, cizí kontejner, zastavený kontejner, neznámý shell, 5. terminál, starší agent, offline host) pošle jako rámec `error` a zavře s kódem 1008.
- Ověřeno: `terminal_test.go` (všech 9 bodů ze sekce 6 včetně kontroly úniku gorutin a úniku napsaných/vypsaných tajemství do DB, událostí i logu), `go vet ./...`, `go test ./...`, `svelte-check` (0 chyb) a `vite build`. Hlavní bundle 585 kB (187 kB gzip) xterm.js neobsahuje; xterm (331 kB) a fit addon se načtou až s terminálem.
- **Ověřeno živě** (review, hub all-in-one + agenti `wd1` terminál zapnutý, `wd2` read-only + `MOORAGE_TERMINAL=true`, `wd3` výchozí; stack `busybox` / `debian:12-slim` / `traefik/whoami`):
  - vstup i výstup přes tunel, `exit 7` → rámec `exit` s kódem 7; `bash` v busyboxu a `sh` ve `scratch` obrazu → srozumitelná hláška a `exit 127`;
  - `wd2` i `wd3` odmítnou (`The web terminal is disabled on this host.`), stejně cizí kontejner, neznámý shell; lokální host funguje;
  - po zavření prohlížeče, přepnutí shellu i odchodu ze stránky v kontejneru nezůstane shell ani `sleep` na pozadí s `trap "" HUP`;
  - restart hubu s otevřeným terminálem → řádek `hub-stopped`, prohlížeč dostane zprávu, shell na agentovi skončí; zastavený agent → `error` („Lost the connection to the host.“) a shell skončí;
  - obnovování stránky stacku po 5 s terminál nepřipojuje znovu; Logbook i Hosts ukazují relace; šířka 375 px bez vodorovného posunu; RAM hubu s otevřeným terminálem 17 MiB anon / 48 MiB RSS (beze změny).
- **Review (Claude) – opraveno 7 chyb:**
  1. **Shell po odchodu dál běžel.** Zavření spojení TTY execu proces neukončí (hlášení Gemini „po `Close()` proces okamžitě skončí“ neplatilo); v kontejneru zůstávaly `/bin/sh` i spuštěné příkazy. Teď je ukončí pevný exec (viz [A] výše).
  2. Zastavení agenta nechalo otevřené shelly běžet (proces skončil dřív, než se k relacím dostal rozpad tunelu). `Engine.Close` je teď ukončí.
  3. Prohlížeč neviděl, proč byl terminál odmítnut (HTTP status před upgradem), jen „Connection refused or closed.“ – ani `Too many open terminals`, ani `Update the agent…`.
  4. Výpadek agenta se zapsal jako `exit` s kódem 0. Teď `error` a zpráva v terminálu.
  5. Vypnutí hubu otevřené terminály neukončilo (`HubCtx` se nikde nenastavoval) a řádek zůstal otevřený do dalšího startu. Teď `hub-stopped` hned a hub na zápis počká.
  6. Kód ukončení se četl hned po konci proudu, kdy Docker ještě hlásí běžící exec s kódem 0. Teď se čeká na `Running: false`.
  7. Cizí kontejner hlásil „Stack not found.“; teď „This container isn't part of the stack.“ (kód `not_in_stack`).
  - Drobnosti: hláška o chybějícím shellu se vypisovala dvakrát (a vkládala se i do běžného výstupu obsahujícího „no such file or directory“); barvy terminálu z tokenů (`--well`, `--foam`, `--buoy`) místo cizí modré; popisek „exit · exit 127“.
- Známá omezení: mobilní klávesnice na dotykových telefonech není speciálně přizpůsobena (layout na 375 px se vejde a zalamuje); proces spuštěný s vyčištěným prostředím (`env -i`) nebo shell při `SIGKILL` agenta může relaci přežít; ukončovací skript potřebuje `cat` v obrazu; Logbook stacku ukazuje relace jako samostatný seznam nad verzemi, ne prolnuté po dnech.

### M10 – hotovo (2026-11-23): uživatelé, OIDC, forward-auth
- Implementace: agent (Claude) podle `prompts/M10-oidc-users.md`; review a živý test proti Dexu (mock connector) a forward-auth přes `curl --interface`.
- **[A] Lokální uživatelé přes jednorázový odkaz** (48 h, uložen jen SHA-256): heslo i TOTP volí pozvaný, admin je nikdy nezná. TOTP povinné pro **všechny** lokální uživatele (viewer čte compose, `.env` a logy). Reset nahradí heslo a TOTP až po dokončení, teprve pak skončí relace.
- **[A] Break-glass:** vždy aspoň jeden aktivní lokální admin s TOTP; demote/disable/delete se kontrolují v jedné transakci. Akce v seznamu nikdy na sebe sama. OIDC/proxy přihlášení až po dokončení setupu.
- **[A] Role a stav se čtou při každém požadavku** (`SessionUser`); změna role, vypnutí a smazání ukončí relace hned.
- **[A] OIDC:** code flow + PKCE S256, state a nonce (v paměti, max. 2000, 10 min, jednorázově), go-oidc + vlastní JWKS (refetch jen pro neznámé `kid`, max. 1× za 30 s). Účty podle (issuer, subject), nikdy podle jména či e-mailu; kolize jména → `-2`. Role ze skupin při každém přihlášení (admin > viewer > výchozí role, `admin` jako výchozí nejde). Rozbitá konfigurace nezastaví start (tlačítko se skryje, chyba v panelu SSO).
- **[A] OIDC relace 12 h** (lokální a proxy 7 dní): role z IdP se mění jen při přihlášení a odhlášení u IdP relaci neukončí. Proxy ověřuje hlavičku při každém požadavku.
- **[A] Forward-auth** jen od TCP peeru v `MOORAGE_TRUSTED_PROXIES`, jinak se hlavičky smažou; platí jen pro `/api/session` a přihlášené routy (ne login, status stránky, `/metrics`, agentský port). Neplatná konfigurace proxy zastaví start (bezpečnostní nastavení se nehádá).
- Nové závislosti: `github.com/coreos/go-oidc/v3`, `golang.org/x/oauth2` (+ go-jose). Binárka +576 KiB, RAM beze změny.
- **Review (Claude) – opraveno:** pozvánka vložená do už otevřené záložky ukázala přihlášení (fáze se počítala jen při načtení); admin ze setupu měl „Never signed in“; OIDC relace 7 dní → 12 h. Nestabilní `TestWatchdogIntegrationThreeAgentsHubOutageAndRecovery` (agenti zapisovali do TempDir po konci testu) opraven.
- Neověřeno: skutečný Authentik/Keycloak; IdP na jiné doméně (SameSite cookie); RP-initiated logout (Dex nemá `end_session_endpoint`).

### M12 – hotovo (2026-11-23): aktualizace Moorage z webového UI
- Implementace: Gemini (Argon) podle `prompts/M12-gemini-argon.md`; bezpečnostní review agentem (17 nálezů), opravy agentem, živý test binárního režimu, režimu pod systemd a v kontejneru (testovací klíč, `-tags moorage_testkey`).
- **[A] Podepsané releasy:** workflow `.github/workflows/release.yml` na každý push na `main` (patch +1 od posledního stabilního tagu, tag vytvoří publish; změny jen v `*.md` release nedělají; vždy jen jeden běh najednou) nebo na ručně pushnutý tag `vX.Y.Z` (minor/major, pre-release; striktní semver). Image se staví bez QEMU: Dockerfile kompiluje na platformě runneru a cross-kompiluje (node:24-alpine nemá arm/v7) – job `build` bez secretů (`npm ci --ignore-scripts`), samostatný job `sign` (čistý checkout, Ed25519 nad `SHA256SUMS`; `sign.go` odmítne klíč, který nesedí s `releasePublicKeyB64`), `publish` (release, pak `:latest`). Akce pinnuté na SHA.
- **[A] Ověřuje instalující uzel** vlastním vestavěným klíčem (podpis, checksum binárky, digest image z podepsaného `images.json`, verze). Kompromitovaný hub nic jiného nenainstaluje. Prázdný vestavěný klíč = vše se odmítne. Testovací klíč z env jen s build tagem `moorage_testkey`.
- **[A] Kontejnerový režim:** pomocný kontejner z nového image (`moorage.role=updater`) zkopíruje konfiguraci cíle a ponechá compose labely; starý kontejner `<name>-moorage-prev` dostane restart policy `no` (původní se vrátí při rollbacku) a smaže se po 24 h. Self-ID jen kontejner, který běží a spouští `/moorage`.
- **[A] Binární režim:** atomická výměna se zachováním práv, `moorage.prev`, re-exec. Rollback nezávislý na watcheru: nová binárka při startu počítá pokusy (`self-update-pending.json`, max. 3 / 120 s) a po překročení obnoví předchozí, takže funguje i pod systemd s crash-loopem.
- **[A] Hub:** kontroluje releasy denně (jen User-Agent), „Update all“ postupně agenti a hub naposledy, stop na první chybě; čeká 180 s, ztišení host-offline 3 min; úspěch hubu až po potvrzení zdraví; overlay „Moorage is restarting“ jen ve fázi `restarting`, s timeoutem 4 min a zavřením. Read-only agent se aktualizovat smí (podepsaný kód), `MOORAGE_SELF_UPDATE=false` odmítne.
- **Review – opraveno 17 chyb:** mimo jiné crash-loop pod systemd bez rollbacku, overlay blokující UI navždy, job uvízlý v `restarting`, „done“ před ověřením zdraví, rollback hlášený jako timeout, starý kontejner se spouštěl po rebootu, workflow bez kontroly klíče a s `:latest` před podpisem, helper bez sítě/SELinux/uživatele cíle, nejednoznačná self-ID, rollback podle tagu místo ID image, špatný ruční příkaz (arch, checksum), sériový a necachovaný přehled.
- **Zbývá uživateli před prvním releasem:** `go run ./scripts/release/genkey.go`, veřejný klíč do `internal/selfupdate/key.go` (commit), privátní do `gh secret set MOORAGE_RELEASE_KEY`, pak tag `v…`.
- Neověřeno: workflow na GitHubu, arm buildy, GHCR, hub pod systemd s padající binárkou, overlay v prohlížeči.

### M13 – hotovo (2026-11-23), kromě bezpečnostního průchodu (odloženo uživatelem)
- **Horní lišta** (`Shell.svelte`): stránky nad obsahem, co se nevejde, padá do „More“ podle naměřené šířky (skrytá měřicí kopie, oříznutá na 0×0, jinak zvětšovala stránku); aktuální stránka v přetečení pojmenuje tlačítko More; uživatelské menu (Account, motiv, odhlášení). Levý panel jen hosté a stacky: na 1152×720 má seznam stacků 483 px místo 2,5 řádku. Pod 768 px šuplík se stránkami pod stacky. „Updates“ → „Image updates“, nově i odkaz Hosts.
- **Automatické vyplňování:** `name` atributy ve všech přihlašovacích formulářích, skrytý username u změny hesla.
- **Rozbalovací prvky** se zavírají kliknutím mimo a Escape (`lib/dismiss.ts`, šipky v menu přes `menuKeys`).
- **MultiSelect** (`lib/MultiSelect.svelte`, vlastní, bez závislosti: nativní checkboxy dávají přístupnost; +1,6 kB gzip): místa kontroly s důvodem nedostupnosti, kanály, rozsah údržby, položky status stránky, filtry audit logu (API nově přijímá více hostů/akcí), výběr kontejnerů v logu (filtr kontejneru přijímá seznam oddělený čárkou; starší agent vrátí „no containers“), agenti watchdogu.
- **`.env` editor** (`lib/EnvEditor.svelte`, `lib/dotenv.ts`): bezeztrátový model (test round-trip v Node), maskování tajných klíčů, chybějící a nepoužité proměnné z compose, vložení více řádků.
- **Vlastní stack Moorage** (`engine/self.go`, `selfupdate/stackapply.go`):
  - **[A]** Detekce přes self-ID z M12 a compose labely.
  - **[A]** Odmítá uzel sám (kód `self_stack`, 409): stop, down, delete, up/update, rollback image; auto-update se přeskočí jako Notify.
  - **[A]** Restart přímo přes engine.
  - **[A]** Apply & restart: záloha souborů do `<data>/self-stack-prev`, uložení verze, pomocný kontejner z aktuálního ID image se všemi mounty, sítí, uživatelem a SELinux cíle → `compose up -d` → 15 s stabilního běhu (hub: `/api/session`) do 120 s → jinak obnova souborů a `up`; výsledek v `<data>/self-stack-result.json` na stránce stacku.
  - **[A]** Uložení odmítne odebrání služby, socketu nebo datového mountu.
  - **Živě ověřeno** (all-in-one jako compose stack z lokálního image): odmítnutí, neškodná změna nasazena, rozbitá změna vrácena za 126 s, restart. Při testu nalezena a opravena chyba: odmítnutí vracelo 502 a zapisovalo se do auditu jako selhání.
- **Nová služba od image** (`internal/imagefind`, `POST /api/image-lookup`, jen admin, bez auditu):
  - **[A]** Registr anonymně (token z https realm, index → linux/amd64, config blob, 1 MiB, 20 s, cache 1 h, guard proti metadata adresám; privátní registry v síti povoleny).
  - **[A]** Zdrojový repozitář jen pro GitHub, GitLab.com a Codeberg (jiný host z labelu se nekontaktuje).
  - **[A]** Úprava `build:` na image a varování (env_file, relativní soubory, privileged…).
  - **[A]** Generované compose: porty, volumes, nastavení image zakomentovaná v `.env`.
  - Ověřeno proti Docker Hubu a GHCR.
- Neověřeno: horní lišta jen přes rozměry v prohlížeči (bez prohlížení všech šířek), Apply & restart na agentovi (jen all-in-one), Podman.

## M13 – UX a bezpečnost (návrh 2026-11-23, schváleno)

Cíl: Moorage se pohodlně ovládá i na malém displeji, `.env` se edituje jako v Coolify, nová služba začíná od image a samotný stack Moorage nejde omylem vypnout. Na závěr bezpečnostní průchod celé aplikace.

### 1. Navigace: stránky do horní lišty (rozhodnutí uživatele)
- Dnes je na 1152×720 (16:10 se škálováním) v levém panelu 9 odkazů a na stacky zbudou asi 2,5 řádku.
- **Levý panel:** jen přepínač hostu, hledání, seznam stacků (všechna volná výška) a dole stav hostu.
- **Horní lišta nad obsahem:**
  - Checks (s počtem problémů), Status pages, Maintenance, Templates, Dependencies, Updates, Notifications, Audit log, Users, Hosts;
  - vpravo uživatelské menu (Account, motiv, odhlášení);
  - odkazy, které se nevejdou, padají do menu „More“ (priority+: počítá se šířka, ne pevný breakpoint);
  - položky jen pro adminy viewer nevidí.
- **Pod 768 px** zůstává dnešní šuplík (drawer), stránky jsou v něm pod stacky.
- Rozhodnuto: „Updates“ (image) se přejmenuje na „Image updates“, ať se neplete s „Moorage updates“ na Hosts.
- Ověřit na 1440×900, 1280×800, 1152×720, 1024×640, 768 a 375 px: žádný vodorovný posuv, aktivní stránka viditelná, fokus a klávesnice v menu More.

### 2. `.env` jako v Coolify
- Dva režimy, přepínač **Variables | Raw**; volba se pamatuje v prohlížeči. Raw je dnešní editor.
- **Bezeztrátový model:** parser zachová komentáře, prázdné řádky, pořadí, `export`, uvozovky i víceřádkové hodnoty v `"…"`. Tabulka mění jen klíče a hodnoty, takže přepnutí tam a zpět soubor nezmění (test round-trip).
- **Tabulka** (klíč, hodnota, smazat, přidat):
  - Vložení více řádků `KEY=VAL` do pole klíče je rozdělí do řádků.
  - Neplatné jméno klíče je chyba; duplicitní klíč dostane varování („last wins“).
- **Tajné hodnoty:** klíč obsahující `PASSWORD|SECRET|TOKEN|KEY|PRIVATE` se ukáže maskovaně s tlačítkem zobrazit. Žádný nový skrytý stav per stack.
- **Nápověda z compose:**
  - proměnné z `${VAR}` bez výchozí hodnoty, které v `.env` chybí, se ukážou s tlačítkem „Add“;
  - proměnné v `.env`, které compose nepoužívá, jsou označené jako nepoužité (jen informace).
- Viewer vidí hodnoty maskované a jen pro čtení. Stejný editor se použije v New stack i v recenzi šablony.

### 3. Stack, ve kterém běží Moorage (rozhodnutí uživatele: editovatelný, nasazení přes pomocníka)
- **Detekce:**
  - uzel (all-in-one i agent) pozná vlastní compose projekt podle self-ID z M12 (běžící kontejner s `/moorage` a label `com.docker.compose.project`);
  - hub dostane příznak `self: true` u stacku;
  - binární režim žádný takový stack nemá.
- **Zakázané akce:** Stop, Down, Delete, Pull+up z auto-updatu (politika se vynutí na Notify; aktualizace jde přes Moorage updates). Refuse vynucuje **uzel sám**, ne jen skryté tlačítko v UI.
- **Restart:** přímo přes engine. Daemon restart dokončí, i když volající proces skončí; UI ukáže overlay „Moorage is restarting“ (z M12).
- **Uložení compose/.env + „Apply & restart“:**
  - verze se uloží do git historie jako dnes;
  - pak se spustí pomocný kontejner z aktuálního image ID (jako v M12, s kontextem cíle: síť, SELinux, uživatel), který udělá `compose up -d` pro tento projekt;
  - počká, až nový Moorage odpoví (hub `/api/session`, agent znovu připojen), nejdéle 120 s;
  - při neúspěchu vrátí předchozí verzi compose/.env z historie, znovu `up` a zapíše rollback do historie i auditu.
- Validace před nasazením: služba Moorage musí ve compose zůstat, socket a datový volume musí zůstat namountované. Jinak odmítnout s vysvětlením (jinak by se Moorage odřízl).
- Ostatní služby ve stejném stacku (např. reverse proxy) se nasazují stejnou cestou.

### 4. Nová služba od image (rozhodnutí uživatele: katalog + repozitář image + generování)
- **New stack, krok 1:** pole „Image“ (např. `vaultwarden/server`, `ghcr.io/org/app:1`). Vedle zůstává „Empty“ a „From `docker run`“.
- **Krok 2: kandidáti se zdrojem,** uživatel jednoho vybere:
  1. **Šablona** z katalogu R7 (bundled/local/remote), která image používá.
  2. **Compose z repozitáře image:**
     - label `org.opencontainers.image.source` z konfigurace image v registru (anonymní token nebo přihlašovací údaje z M8);
     - u GitHub/GitLab/Codeberg zkusit `compose.yaml|compose.yml|docker-compose.yml|docker-compose.yaml` v kořeni, `docker/` a `deploy/` výchozí větve;
     - `build:` sekce a chybějící `env_file`/relativní soubory označit nebo odstranit s varováním.
  3. **Vygenerované z metadat image:** exposed porty, volumes, env výchozí hodnoty, user, healthcheck.
- **Krok 3:** dnešní editor (compose + nový `.env` editor) a create.
- **Bezpečnost:**
  - stahuje hub, jen HTTPS, 1 MiB na soubor, 20 s, žádné redirecty na http, žádné privátní IP (stejná pravidla jako vzdálené indexy šablon);
  - cache 1 h; GitHub API jen 1 volání na repozitář (výchozí větev), soubory přes raw.
  - Obsah je nedůvěryhodný: varování jako u šablon (privileged, socket, host network), vždy recenze před vytvořením, nic se nespouští.
- **Stavy:**
  - image nenalezen nebo registr nedostupný: hláška, zůstává ruční vložení;
  - repozitář bez compose: jen ostatní kandidáti;
  - soukromý image bez přístupu: návod na přihlášení (M8 `auth`).

### 5. Automatické vyplňování přihlášení
- `autocomplete` už je (username, current-password, one-time-code, new-password).
- Doplnit `name` atributy (některé správce hesel je potřebují) a skrytý username i u změny hesla v Account.
- Ověřit uložení a vyplnění v Chrome i Firefoxu.

### 6. Bezpečnostní průchod celé aplikace (po bodech 1–5) – odloženo uživatelem
- **Rozdělení na oblasti, každá prověřená zvlášť** (agenti paralelně, skill `security-review`):
  - auth/session/OIDC/forward-auth;
  - hub↔agent tunel a agentské API (allowlist, read-only);
  - práce se soubory stacků (path traversal, symlinky, git historie s `.env`);
  - SSRF (kontroly, šablony, vyhledání image, webhooky);
  - self-update a self-stack;
  - UI (XSS, CSP, CSRF, WebSocket origin);
  - závislosti (`govulncheck`, `npm audit`).
- Výstup: seznam nálezů se závažností, opravy, záznam do PLAN.
- Neplatí jen pro novou práci; projde se celý kód.

### 7. Vlastní dropdown s multi-selectem
- **Jedna komponenta `MultiSelect`:**
  - tlačítko ukazuje vybrané jako „3 selected“ nebo první dvě jména + „+2“;
  - po otevření panel s hledáním (od 8 položek), zaškrtávacími řádky, „Select all / Clear“;
  - u položky může být důvod, proč nejde vybrat (např. agent offline);
  - zavírá se kliknutím mimo i Escape.
- **Implementace:** postavit na hotovém primitivu `bits-ui` (Svelte 5, MIT: klávesnice, fokus, ARIA listbox) a nastylovat tokeny; vlastní implementace jen pokud by bundle narostl o víc než ~30 kB gzip.
- **Kde:**
  - **CheckForm:** místa kontroly (lokace s důvodem nedostupnosti), notifikační kanály;
  - **MaintenanceForm:** stacky a kontroly (dnes výběr po jedné + Přidat);
  - **StatusPageForm:** přidání více kontrol/stacků naráz (pořadí položek se dál řadí v seznamu);
  - **AuditView:** filtr hostů a skupin akcí;
  - **LogViewer:** výběr kontejnerů (dnes „All“ nebo jeden);
  - **Hosts → watchdog:** sledující agenti.
- Jednoduché jednovýběrové `<select>` (interval, metoda, role) zůstávají nativní: na mobilu dávají systémový výběr a jsou přístupné bez práce navíc.
- Hotovo hned (2026-11-23): menu „More actions“ u stacku a přepínač hostů se zavírají kliknutím mimo a Escape (`web/src/lib/dismiss.ts`, focus se vrací na tlačítko). Ostatní rozbalovací prvky dostanou totéž.

### Pořadí
1. Autofill a horní lišta + ověření displejů (malé, viditelné hned), spolu s MultiSelect.
2. `.env` editor.
3. Self-stack.
4. Nová služba od image.
5. Bezpečnostní průchod.

### Mimo rozsah
- Sdílené proměnné mezi stacky.
- Šifrování `.env` v historii.
- Parsování README z Docker Hubu (uživatel nevybral).
- Vyhledávání image v registrech (autocomplete).
- Pody a Kubernetes.

### Otevřené otázky
- Jakým způsobem implementovat: agenti (Claude) jako u M10, nebo prompty pro Argon jako u M12?

## Roadmapové funkce (2026-11-22)
Uživatel chtěl všechny položky ROADMAP se stavem „líbí se“ hned. Dělají je agenti jeden po druhém (zadání `prompts/roadmap-agent-common.md` + `prompts/R*.md`), každou pak Claude zkontroluje a otestuje živě.

### R1 – audit log všech akcí – hotovo
- Tabulka `audit_log` (kdo, kdy, IP, akce, host, stack, cíl, výsledek; retence 1 rok). Zapisuje ji **jeden middleware** nad všemi zapisovacími routami: akce z mapy `route pattern → klíč` (`internal/api/audit.go`), host/stack/cíl jen z hodnot v cestě. Test selže, když nová zapisovací routa v mapě chybí.
- **[A] Nikdy tělo požadavku ani query** (hesla, TOTP, compose/.env, tokeny, URL webhooků) – jen klíč akce a hodnoty z cesty.
- **[A] Odmítnutí (4xx) se nezapisuje**, nic se nestalo; 2xx = `ok`, 5xx = `failed`. Asynchronní operace (up, pull, update) se zapíšou při přijetí, jejich výsledek je dál v událostech stacku a logu operace.
- Přihlášení (`auth.login`, `auth.login_failed` se zkoušeným jménem a IP, `auth.logout`, `auth.setup`), otevření terminálu (`terminal.open`), automatické akce s aktérem `system` (`image.auto_update`, `host.paired`).
- UI: stránka **Audit log** v postranním panelu (jen admin, filtry, „Load more“, na úzké obrazovce karty); sekce v Historii stacku. Viewer dostane 403.
- Ověřeno živě: create/up/stop/down/delete na `wd1` se zapsaly; neplatný compose (422), stop na zaneprázdněném stacku (409) a delete běžícího stacku (409) ne; chybné přihlášení se jménem a IP; tajemství z compose ani heslo v DB nejsou; 375 px bez vodorovného posunu. Review bez nalezené chyby.
- Známá omezení: odkaz z Historie otevře globální log bez filtru stacku; filtr podle aktéra nemá index.

### R2 – plánovaná údržbová okna – hotovo
- Tabulka `maintenance_windows`: jednorázové okno, nebo týdenní (dny, čas, délka, IANA zóna); rozsah = vše, nebo stacky a/nebo kontroly (JSON). Stránka **Maintenance** v postranním panelu (viewer jen čte), štítek „Maintenance until …“ u stacku a kontroly.
- **[A] Nový stav `maintenance`:** selhání v okně se uloží, ale nepošle upozornění ani neotevře incident. Selhání se počítají do opakování, takže kontrola, která je po konci okna dál dole, upozorní **jednou a hned** (monitor ji na konci okna spustí ihned). Zotavení během okna nic neposílá. Down nahlášený před oknem zůstává nahlášený a jeho zotavení se pošle.
- **[A] Uptime počítá jen `up` a `down`**, údržba se nezapočítává; na status stránce má den vlastní stav.
- **[A] Watchdog hubu a výpadky agentů ztiší jen okno s rozsahem „vše“.** Okno nad stackem o celých hostech nic neříká. Okno „vše“ prodlouží `maintenanceUntil`, které se posílá agentům.
- Status stránka: karta „Maintenance“, stav položky, „Planned maintenance“ na 7 dní, položka v RSS. Začátek a konec okna jsou události stacku druhu `maintenance` (nejsou deploy druh), v Logbooku vlastní sekce.
- DST: neexistující čas začne v nejbližším platném, při posunu zpět jen jedno okno; vše v UTC. Index aktivních oken v paměti, obnovovaný z DB (přežije restart).
- Ověřeno živě: padající TCP kontrola v okně nad `whoami` → `maintenance`, žádný webhook; smazání okna → hned jedno DOWN; plánované okno na zítřek na veřejné stránce `home`; agent ověřil 375 px.
- **Review (Claude) – opravena 1 chyba:** log kontrola nahlášená jako DOWN před oknem poslala po jeho konci falešné „up again“ a za minutu stejné DOWN znovu (shody z okna se po něm nepočítaly). U nahlášeného downu se teď počítají. Test `TestMaintenanceAnnouncedLogDownStaysDown`.
- Známá omezení (R2): prodloužení pauzy watchdogu na agentech (okno „vše“) nemá test a nebylo ověřeno živě; úprava běžícího okna zapíše „Ended early“ + nový start; smazané okno zmizí i z RSS; okno jen nad kontrolou neukáže štítek na stránce stacku.

### R3 – `/metrics` pro Prometheus + Grafana dashboard – hotovo
- `MOORAGE_METRICS_TOKEN` (min. 16 znaků) zapne `GET /metrics`; bez něj 404, bez/s chybným tokenem 401 (porovnání v konstantním čase). Nikdy na listeneru status stránek ani na jejich doménách. Formát exposition psaný ručně, bez nové závislosti.
- **[A] Scrape nikdy nečeká na Docker ani agenty:** stav kontrol a hostů z paměti, kontejnery/uptime/aktualizace z background snapshotu (obnova po 30 s jen dokud někdo scrapuje, po 5 min ticha se zastaví; každý host 10 s timeout, offline host se neptá).
- **[A] `moorage_check_state{state}` místo stavového labelu na `check_up`** – stabilní série; doporučené pravidlo `moorage_check_state{state="down"} == 1`. Každá série hostu má i `host_id` (jména se dají přejmenovat a nejsou unikátní).
- Metriky: build info, stáří snapshotu, host up/read-only/agent info, check up/state/latence/uptime 24h a 30d, kontejnery stacku po stavech, kontejner up, dostupná aktualizace image, odeslané/selhané alerty podle typu kanálu. CPU/RAM a restarty vynechány (vzorkování ~1 s na stack, resp. inspect na kontejner – příliš drahé).
- Dashboard `deploy/grafana/moorage-dashboard.json` (18 panelů, proměnné datasource a host); test ověří, že dotazuje jen existující metriky. V Hosts jeden řádek, zda jsou metriky zapnuté, a URL (nikdy token).
- Ověřeno živě: 401 bez tokenu i se špatným, 404 na status listeneru, 200 se sériemi z lokálního hostu i agentů `wd1`–`wd3`; zastavený `wd3` → `moorage_host_up 0` a jeho kontejnerové série zmizí; scrape 0,18 s; RAM hubu beze změny (17 MiB anon). Review bez nalezené chyby.
- Známá omezení: dashboard nebyl naimportován do skutečné Grafany; žádný rate limit na chybné tokeny (spoléhá na náhodný token).

### R4 – časová osa stacku – hotovo
- Záložka **History** je jedna časová osa: verze (s diffem a „Roll back“), události stacku, incidenty kontrol stacku, aktualizace image a pro admina audit a relace terminálu. `GET /api/hosts/{host}/stacks/{name}/timeline` (kurzor `ms~druh~id`, filtr `kinds`, `since`/`until`), filtrovací čipy, „Load more“.
- **[A] Stabilní stránkování napříč zdroji:** každý zdroj max. limit+1 řádků pod kurzorem, stránka končí u nejvyššího posledního řádku zdroje, který má další; test se stejnou sekundou v 6 zdrojích a velikostmi stránky 1–13.
- **[A] Jedna akce = jeden záznam:** audit + událost (+ verze nebo záznam aktualizace) téhož aktéra do 10 s se sloučí (vzájemně nejbližší pár); seznam párů v `internal/api/timeline_merge.go`.
- **[A] Audit a terminál filtruje server** (viewer je nedostane ani při `kinds=audit`); událost `terminal` se v ose nezobrazuje vůbec (prozradila by viewerovi aktivitu v terminálu).
- „Co se změnilo předtím“: u incidentu i ze stránky kontroly (`#/h/{host}/stacks/{name}/history?at=<unix>`) – 2 h před incidentem se zvýrazněnými změnami a „Show everything“. Nedostupná historie verzí (offline/starší agent) nezablokuje zbytek osy, jen ukáže poznámku.
- Ověřeno živě: `crashy` – incidenty a verze, „Show what changed before“; nový stack na `wd1` (create, up, save, restart) = 4 záznamy, každý sloučený s auditem; 375 px bez vodorovného posunu; při 5s obnovování stránky se osa znovu nenačítá.
- **Review (Claude) – opravena 1 chyba:** po akci na stránce stacku (Restart, Deploy…) se nový záznam v ose neobjevil až do obnovení stránky. Teď se osa po skončení operace načte znovu (počítadlo, takže polling ji dál nespouští).
- Známá omezení: jen posledních 200 verzí (limit hostu); každá stránka znovu čte historii z hostu.

### R5 – graf závislostí mezi stacky + kaskádová upozornění – hotovo
- Tabulka `stack_deps` (zdroj `detected`/`manual`, příznak `ignored`), sloupec `incidents.caused_by`. Detekci dělá agent (`GET /deps/facts`, posílá jen jména sítí, služeb, kontejnerů a hostů – hodnoty env nikdy), hrany počítá hub. Starší agent (404) → jen ruční hrany.
- **[A] Detekce je opatrná:** síťová hrana jen když síť `external` (i přes `name:`) vytváří právě jeden stack; hrana podle jména hostu (links, URL/`host:port` v env) jen při sdílené síti, bez nejednoznačných jmen. Spouští se při create/edit/deploy/update/rollback/delete, každých 10 min a 15 s po startu; jedna detekce na host naráz. Rozbitý compose si nechá dřívější hrany.
- **[A] Kaskáda nesmí ztratit upozornění:** každá kontrola má dál vlastní incident; zpráva závislého stacku se připojí k dávce kořene (do hloubky 3, smyčky se ignorují) → „DOWN: pg — also affected: app“. Kanály = sjednocení. Závislý, který spadl dřív, zůstane odeslaný („alerted earlier“). Obnova kořene počká 60 s na závislé; kdo zůstane dole, dostane vlastní upozornění „still down after pg recovered“. Údržba a ztišení po deployi mají přednost.
- UI: stránka **Dependencies** na host (ruční SVG vrstvený graf, pod 640 px jen seznam, varování na smyčky), karta „Depends on / Used by“ na stránce stacku (přidat, odebrat, ignorovat), „Root cause“ u incidentu v časové ose.
- Ověřeno živě na `wd1`: `r5app` používá síť `r5pg_backend` jako external → hrana se našla sama. `docker stop` databáze → **jedna** zpráva „r5pg … Also affected, probably caused by r5pg: r5app“ (4 s po downu závislého); `docker start` → „r5pg up is up again … Still down after r5pg recovered: r5app“ a za 10 s samostatné DOWN pro `r5app`. Smazání stacků odstranilo hrany. Review bez nalezené chyby.
- Známá omezení: závislí s opakováním obvykle padnou až po odeslání zprávy kořene, takže přijdou jako druhá zpráva „Also affected by …“ (sbíraná 60 s); obnova kořene se závislými dole se zpozdí o 60 s; stack víc než 3 kroky od kořene se přiřadí nejbližšímu; status stránky a metriky kaskádu neukazují.

### R6 – kontroly z více míst (hlasování agentů) – hotovo
- HTTP/TCP/ping/DNS kontrola může běžet z hubu i vybraných agentů (`checks.locations`, max. 8) s kvórem `majority` (výchozí, víc než polovina hlasujících; 2 ze 4 = remíza = up), `any` nebo `all`. Výsledky po místech v `check_location_results` (30 dní), souhrn v `check_results` jako dřív.
- Agent: `POST /probe` se **stejným kódem sond jako hub**, vynucuje druh (ne stack/log), timeout max. 30 s, max. 8 sond naráz (9. = „busy“ = nehlasuje, ne falešný down). `MOORAGE_PROBES` (výchozí zapnuto); **[A] read-only agent sonduje**, protože sonda na hostu nic nemění a hub tam už může spustit libovolný stack.
- **[A] Nehlasuje:** offline, starší (404), vypnutý, zaneprázdněný agent, žádná odpověď v timeoutu, místo bez ICMP. **Nikdo neodpověděl → stav `unknown`**: bez upozornění, bez vlivu na uptime; nahlášený down zůstává.
- **[A] Kontrola jen z hubu jde přesně starou cestou** (prázdné `locations`).
- Zprávy a upozornění s rozpadem po místech („down from 2 of 3 locations: …; no answer: wd3 (…)“), metriky `moorage_check_location_up` a latence po místech; formulář s výběrem míst a kvóra, karta „Locations“ s mini pruhem po místech.
- Ověřeno živě: TCP na zavřený port z hubu + `wd1`–`wd3` → „down from 3 of 3 … no answer: wd3 (checks are turned off on this agent)“ s rozpadem ve webhooku; zastavený `wd1` → HTTP kontrola hub+wd1+wd2 „up from 2 of 2“, kontrola jen z `wd1` → `unknown` bez upozornění (přišlo jen běžné „host offline“); karta Locations. Review bez nalezené chyby.
- Známá omezení: hub neomezuje počet sond na jednoho agenta (nad 8 → nehlasuje); latence po místech bez grafu; po přepnutí zpět jen na hub zůstanou staré řádky do retence.

### R7 – šablony stacků – hotovo
- Šablona = složka s `template.yaml` (proměnné: výchozí hodnota, `secret`, `generate: password|hex32|uuid`, `pattern`, `required`; neznámé klíče = chyba), `compose.yaml` a volitelným `.env`. Zdroje: 7 vestavěných (whoami, nginx, PostgreSQL 17, Valkey 8, Adminer, Gitea, Caddy – licence MIT/Apache/BSD, Vaultwarden vynechán kvůli AGPL), lokální složka `MOORAGE_TEMPLATES_DIR`, vzdálené indexy přes HTTPS.
- **[A] Bez centrálního katalogu a bez spouštění čehokoli:** zástupné symboly se jen nahrazují (jen deklarované `${NAME}`), nic se nevyhodnocuje, Moorage nespouští git. Před vytvořením se **vždy ukáže výsledný compose a .env** (tajné hodnoty skryté); varování (ne blokace) na `privileged`, host síť, `pid: host`, socket Dockeru.
- **[A] Vytvoření jde existující cestou** `POST …/stacks` (validace, git verze, audit `stack.create`); poznámka „Created from template …“ se skládá na serveru z katalogu, nikdy z textu klienta, a agent ji znovu čistí. Starší agent poznámku ignoruje, stack vznikne. Render nic nezapisuje ani neaudituje.
- **[A] Vzdálené indexy:** HTTPS (i přesměrování), 1 MiB na soubor, 16 MiB na obnovu, 200 šablon, 20 s; hub si sám resolvuje jméno a vytáčí jen povolenou IP (i proti DNS rebindingu); soukromé adresy jen se zaškrtnutím „allow local network“, link-local a metadata nikdy. Selhaná obnova nechá poslední dobrou kopii.
- Ověřeno živě přes agenta `wd1`: Valkey ze šablony (jméno, port) → kontrola souborů s heslem skrytým → „Create and deploy“ s výstupem operace; verze „Created from template valkey (bundled, version 1)“; vygenerované heslo (32 znaků) Valkey přijme, bez něj NOAUTH; audit jen `stack.create` + `stack.up`; index na `192.168.1.1` zablokován se srozumitelnou chybou, `http://` odmítnuto. Agent ověřil všech 7 vestavěných šablon přes `docker compose`. Review bez nalezené chyby.
- Známá omezení: výchozí porty vestavěných šablon (8081–8084, 3000) mohou kolidovat; lokální složka se čte při každém výpisu; Gitea vyžaduje ruční dokončení instalátoru.

### R8 – podpora Podmanu – částečně (neověřeno)
- Ukončeno na žádost uživatele dřív. **Nic nebylo spuštěno proti skutečnému Podmanu** (ve WSL není nainstalovaný); fixtures v `internal/engine/testdata/podman/` jsou ručně psané podle dokumentace.
- Hotovo v kódu (`internal/engine/podman.go`): detekce enginu z `/version` (`engine`, `engineVersion`, `rootless` v `HostInfo` a `host.Entry`, UI „Podman 5.x (rootless)“), normalizace stavů kontejnerů a health, statistiky ze dvou vzorků s „–“ a důvodem místo nesmyslů, UTC časy logů a navázání streamu bez duplicit, `Wait` u odstraněné exec relace, digest image přes registr (HEAD) když chybí `/distribution`, varování k compose a nízkým portům. 20 testů proti falešnému compat API.
- **Změna i pro Docker:** paměť 0 = „neznámá“ s důvodem (dřív „0 B / 0 B“, např. Pi bez memory accounting). Ověřeno, že Docker cesta dál ukazuje CPU/RAM normálně.
- Zbývá: spustit `scripts/dev/podman-smoke.sh` proti Podmanu 4.9 a 5.x (rootful i rootless) a projít matici funkcí z hlášení agenta; přihlašovací údaje z Podman `auth.json`; README (nastavení socketu). Lokální host v seznamu hostů zatím typ enginu neuvádí.

## Další krok
1. Commit a push M10 + M12 + M13.
2. Bezpečnostní průchod celé aplikace (M13 bod 6, odložený).
3. Podepisovací klíč releasů (uživatel: genkey, `key.go`, `gh secret set`) a první tag – ověří workflow, GHCR a aktualizaci z UI naostro.
4. **M11 – export/import konfigurace, hodinové agregace kontrol** (audit log všech akcí je hotový jako R1).
5. Ověřit R8 (Podman) proti skutečnému Podmanu: `scripts/dev/podman-smoke.sh`, matice v sekci R8.
6. Vydání **v1.0**. Z plánu 1.1/1.2 zbývá jen automatický rollback po selhání kontrol.

## Další vylepšení (schváleno 2026-11-23)

Čtyři navazující změny nad M13/R5/R7:

1. **Odkazy na webové stránky služeb (`internal/engine/containers.go`, `StackDetail.svelte`):**
   - **[U]** Explicitní label `moorage.url` (jedna či více URL) + autodetekce z `homepage.href`, Traefik (`traefik.http.routers.<r>.rule` s `Host`/`PathPrefix` + TLS) a Caddy (`caddy` / `caddy_N`).
   - **[A]** Striktní sanitizace: jen `http://` a `https://`, bez přihlašovacích údajů, max. 512 znaků a max. 5 odkazů na kontejner; v UI `target="_blank" rel="noopener noreferrer"`. Zastavený kontejner odkaz zobrazí tlumeně (`--haze`), nebezpečné či neplatné labely se ignorují.
2. **Repozitář `moorage-templates` + jednokliková předvolba (`TemplateSources.svelte`):**
   - **[U]** Samostatné repo `moorage-templates` se šablonami ve formátu `index.json` + `<id>/template.yaml`, `compose.yaml` (a volitelně `.env`), plus CI validace.
   - **[U]** V **Template sources** jednoklikové tlačítko *Add official templates* (opt-in bez nechtěného síťového provozu po instalaci).
3. **Dependencies – vizualizace zařízení a přepínač „All hosts“ (`internal/api/deps.go`, `DepsView.svelte`, `lib/deps.ts`):**
   - **[U]** Výchozí pohled zůstává per-host; přibude přepínač **Current host / All hosts** (`GET /api/deps`).
   - **[A]** Každé zařízení (Hub / Agent) je v SVG grafu vykreslené jako zaoblený kontejner se stavovou tečkou (online/offline) a štítkem `Hub`/`Agent`, v němž leží jeho stacky. Offline agent drží známé hrany se šedým rámečkem.
4. **Stránka Nastavení (`SettingsView.svelte`, `Shell.svelte`, `state.svelte.ts`):**
   - **[U]** Sloučení `Hosts`, `Notifications`, `Audit` a `Users` pod jednu položku **Settings** (`#/settings/hosts`, `notifications`, `audit`, `users`).
   - **[A]** Zpětná kompatibilita starých rout (`#/hosts`, `#/notifications`, `#/audit`, `#/users`) a omezení `Notifications`/`Audit`/`Users` pouze pro roli `admin` (viewer vidí jen `Hosts`).
- **Mimo rozsah:** automatická detekce mezihostových závislostí, hádání HTTP odkazů z raw portů, přesun `Template sources` do `Settings`.

## Zpřehlednění UI a navigace (schváleno 2026-11-23)

1. **Čistší horní lišta:** sloučení `Checks`, `Status pages` a `Maintenance` pod **Monitoring** (`MonitoringView.svelte`, `#/monitoring/checks|status-pages|maintenance` se zachováním zpětné kompatibility starých cest) a přesun **Template sources** do 5. záložky v **Settings** (`#/settings/templates`, jen admin).
2. **Přehlednější Home dashboard (`Home.svelte`):** přehled všech zařízení (Hub + agenti) s rychlým přepnutím, společný seznam problémů (stacky + padající kontroly), seznam služeb s webovými odkazy (`StackSummary.urls`) a upozornění na dostupné aktualizace imagí.
3. **Zpřehlednění `StackDetail.svelte`:** webové odkazy stacku přímo v hlavičce, přesun `StackDeps` do samostatného tabu **Dependencies** vedle `Containers`, `Compose`, `Logs` a `History`.
4. **Globální příkazová paleta (`Ctrl+K` / `Cmd+K`, `CommandPalette.svelte`):** rychlé vyhledávání napříč všemi hosty, stacky, webovými odkazy služeb, kontrolami a stránkami aplikace.

## Architektonický princip: Univerzální jádro vs. Add-ons (schváleno 2026-11-23)

- **[U]** **Moorage je univerzální služba.** Jádro obsahuje jen to, co potřebuje každý uživatel Dockeru/Compose (stacky, logy, terminál, git historie, hub + agenti, watchdog, checks, status pages, maintenance, image updates, OIDC/forward-auth, `moorage.url`, navigace a `Ctrl+K`).
- **[U]** Funkce zaměřené na konkrétní skupinu uživatelů nebo vlastní autorovo použití patří mimo jádro jako **Add-ons / Extra** (zapínatelné volitelně v **Settings → Add-ons**, ve výchozím stavu vypnuté, bez zbytečné zátěže v základním UI):
  - **Proxy & dashboard label autodetection (`proxy-labels`):** čtení `traefik.*`, `caddy*` a `homepage.href` (nativní `moorage.url` zůstává vždy v jádru).
  - **Official templates (`official-templates`):** jednoklikové připojení externího repozitáře `moorage-templates` (základní *Template sources* zůstává čistě obecné pro jakékoli HTTPS repo).
  - **Prometheus & Grafana (`prometheus-metrics`):** přehled `/metrics` endpointu a odkazu na `deploy/grafana/moorage-dashboard.json`.
  - Budoucí specifické nástroje z `ROADMAP.md` (`Trivy`, `Restic`, `Podman`, specifické webhooky/integrace) se budou přidávat stejně — přes volitelné add-ony nebo externí HTTP/kontejnerové kontrakty.
  - Samostatný katalog add-onů žije v repozitáři **`moorage-addons`** (`<id>/addon.json` + `<id>/README.md` s povinným odhadem **RAM a CPU** zátěže + generovaný `index.json`).
  - **[U]** Životní cyklus add-onů (**Install / Enable / Disable / Uninstall**):
    - `builtin` add-ony se zapínají/vypínají v DB (`0 MB` když jsou vypnuté).
    - `sidecar` add-ony se instalují stažením `compose.yaml` z `moorage-addons` (přes `netguard`) jako spravovaný stack `addon-<id>`; vypnutí = `stop` stacku (`0 MB` RAM), odinstalace = `down` + smazání stacku a záznamu v `installed_addons`.
    - **[U]** Katalog `moorage-addons/index.json` se načítá automaticky při otevření stránky **Settings → Add-ons** (s krátkou paměťovou cache, žádný nechtěný provoz při startu samotného hubu).

## Další vlna vylepšení a jádrové funkce z roadmapy (2026-11-23, implementováno)

**Cíl:** Zpřehlednit správu add-onů (filtry, automatický katalog, jeden společný stack `add-ons` pro sidecary) a stránku Hosts, zrychlit práci s `.env` po vzoru Coolify, rozšířit notifikace a převést klíčové položky z roadmapy do jádra.

### 1. Add-ons (`AddonsView.svelte`, `internal/api/addons.go`)
- **[U]** **Automatické načítání katalogu:** při otevření **Settings → Add-ons** se automaticky načte i katalog z `moorage-addons/index.json` (využívá stávající in-memory cache na hubu, tlačítko *Refresh catalog* vynutí čerstvé stažení).
- **[U]** **Filtrování:** lišta filtrů podle stavu (`All`, `Installed`, `Enabled`, `Disabled`, `Not installed`), druhu (`Built-in`, `Sidecar`) a kategorie (`Integrations`, `Observability`, `Backups`, `Security`, `Templates`). Prázdný výsledek nabídne reset filtrů.
- **[U]** **Společný stack `add-ons` pro sidecary:** místo samostatného stacku `addon-<id>` se všechny sidecar add-ony na vybraném hostu přidávají jako pojmenované služby (`services.<id>`) do jednoho společného stacku `add-ons` (sloučení `services`, `volumes`, `networks` s kontrolou kolizí).
  - **[A]** Zapnutí/vypnutí jednoho sidecaru spouští/zastavuje jen jeho službu (`up -d <service>` / `stop <service>`). Odinstalace odstraní jeho definici ze společného `compose.yaml` a provede `up -d --remove-orphans` (celý stack `add-ons` se smaže až po odebrání posledního sidecaru).
  - **[A]** Zpětná kompatibilita: již existující stacky `addon-<id>` zůstanou funkční, dokud se neodinstalují.

### 2. `.env` v compose po vzoru Coolify (`EnvEditor.svelte`, `dotenv.ts`)
- **[U]** **Automatické doplnění z `compose.yaml`:** každá proměnná `${VAR}` i `${VAR:-default}` nalezená v `compose.yaml` se automaticky přidá do `.env` (pokud tam ještě není; u `${VAR:-default}` volitelně s prázdnou hodnotou nebo zobrazeným výchozím placeholderem), bez nutnosti klikat po jedné.
- **[U]** **Přehled služeb v tabulce:** u každého řádku `.env` se zobrazí štítky služeb z `compose.yaml`, které danou proměnnou používají (např. `app`, `worker`). Zůstává maskování tajných hodnot a vložení více řádků `KEY=VALUE` přes schránku.
- **Mimo rozsah:** globální sdílené proměnné napříč různými stacky (zůstává v `ROADMAP.md`).

### 3. Rozšíření notifikací (`internal/notify/`, `NotificationsView.svelte`, `Shell.svelte`)
- **[U]** **Nové události pro stávající i nové kanály:** kromě `DOWN`/`Back up` kontrol a watchdogu přidat volitelné odběry událostí: `auto-update` (nový digest/aktualizace), `rollback` (vč. automatického), `deploy.failed` (selhání operace stacku) a `agent.offline` / `agent.online`.
- **[U]** **Další notifikační kanály:** `Slack` (webhook), `Gotify` (server URL + app token), `Pushover` (user + app token), `Matrix` (homeserver + room + access token) a rozšířený `Webhook` (vlastní HTTP hlavičky / metoda). Vše nad `netguard`, bez externích CLI a bez uživatelských šablon.
- **[U]** **Oznámení v otevřeném UI + Web Push (po vzoru Kumy):** živé toast notifikace + volitelný zvuk / browser Notification API při otevřeném UI přes stávající WebSocket, plus volitelný Web Push (PWA service worker + VAPID klíče uložené v SQLite) pro upozornění na telefon/desktop i při zavřeném prohlížeči.

### 4. Zpřehlednění stránky `Hosts` (`HostsView.svelte`, `AuditView.svelte`)
- **[U]** **Čisté rozdělení:**
  - Hlavní pohled **Hosts** ukazuje jen přehledný seznam hostů (stav online/offline, verze, read-only, typ enginu, rychlé akce přejmenování/odebrání/aktualizace) a v hlavičce tlačítko **+ Add host** (otevře modal/drawer s tokenem a `compose.yaml` pro agenta).
  - **Hub watchdog** přesunout do samostatné sbalitelné karty nebo podzáložky *Watchdog* (se souhrnným stavem v hlavičce).
  - **Terminal sessions** přesunout ze stránky Hosts pod **Settings → Audit** (kam logicky patří k ostatním auditním záznamům).

### 5. Jádrové funkce z roadmapy
1. **Obnova smazaného stacku z historie:**
   - Využívá existující `history.git` (M2 už ukládá poslední stav smazaného stacku). Přidat API pro výpis smazaných stacků v historii hostu a obnovu posledního commitu jedním klikem (`CreateNoted` + volitelně `up`), s auditem `stack.restore`.
2. **Úklid disku s náhledem + denní úloha v nastavení:**
   - Na agentovi (`host` / `engine`) endpoint pro náhled nepoužitých objektů (`images`, `volumes`, `build cache`, odhad uvolněných bytů) a spuštění úklidu po potvrzení adminem (volitelně per typ).
   - V **Settings** (per host nebo globálně) přepínač **Daily automatic cleanup** (výchozí zapnuto nebo volitelné v základu – denní cron např. v 03:30 pro nepoužité images a build cache; volumes raději jen na explicitní zaškrtnutí, aby se nesmazala data zastaveného stacku).
3. **Export a import konfigurace i dat (`M11`):**
   - Export do jednoho archivu (`.tar.gz`): SQLite databáze hubu (konfigurace, uživatelé, sondy, incidenty, audit log, nastavení, kanály) + definice stacků (`compose.yaml`, `.env`) a `history.git` z připojených hostů. Tajemství v exportu šifrovaná heslem/klíčem zadaným při exportu (nebo jasné varování).
   - Import s náhledem co se přepíše, validací schématu a bezpečným restartem/obnovou DB. (Aplikační Docker volumes zůstávají mimo jádro – řeší sidecar `restic-backup`.)
4. **Automatický rollback při selhání kontrol po auto-update (`v1.1`):**
   - Navazuje na M8 (`image_updates`) a M4 (`monitor`). Pokud po automatické aktualizaci stacku přejde některá jeho navázaná kontrola (po vypršení 3min deploy ztišení a v definovaném okně, např. 10 min) do stavu `DOWN`, hub automaticky provede rollback na předchozí digest/commit z historie, přepne politiku aktualizace stacku na `notify` (aby se nesmyčkoval) a pošle notifikaci o automatickém rollbacku.
   - **[U]** **Viditelnost v UI:** o automatickém rollbacku musí uživatel v UI jasně vědět — zobrazí se výrazný upozorňující banner v detailu stacku i na Home dashboardu (s důvodem, kdy k rollbacku došlo a možností potvrdit/skrýt), plus záznam v Logbooku a Auditu.

### 6. Zjednodušení konfigurace, rozumné výchozí hodnoty a pročištění UI
- **[U]** **Autodetekce a rozumné výchozí hodnoty:**
  - `SecureCookies`: automaticky `true`, pokud `PublicURL` začíná `https://` nebo požadavek přichází přes HTTPS / důvěryhodné `X-Forwarded-Proto: https` (explicitní `MOORAGE_SECURE_COOKIES` má přednost).
  - `PublicURL`: pokud není v envu ani v DB, nabídne se/odvodí z prvního přístupu admina (s možností úpravy v UI).
  - `WatchdogListen`: na agentovi výchozí `:5082` zapnuto v základu (místo nutnosti ručně nastavovat env).
- **[U]** **Nekritické proměnné v Settings UI (env jako override):**
  - Přesunout správu `PublicURL`, `MetricsToken` (včetně generátoru tokenu a přepínače přímo v UI / add-onu Prometheus), `UpdateChannel` (`stable`/`pre`), denního úklidu disku a notifikací do **Settings** v DB. Pokud je nastavena příslušná `MOORAGE_*` env proměnná, slouží jako read-only override (v UI označeno štítkem *from env*).
- **[U]** **Bezpečnostní přepínače zůstávají přísně v envu:**
  - `MOORAGE_READ_ONLY`, `MOORAGE_TERMINAL`, `MOORAGE_TRUSTED_PROXIES`, `MOORAGE_PROXY_*`, `MOORAGE_OIDC_*` a `MOORAGE_SELF_UPDATE` nelze měnit z UI (prevence eskalace při prolomení účtu).
- **[U]** **Configuration / Diagnostics v Settings:**
  - Stručná diagnostická karta ukazující jen reálné problémy a varování (např. `HTTPS proxy bez SecureCookies`, chybějící `PublicURL` pro odkazy v alertech/RSS, chyba v `MOORAGE_OIDC_*`, chybějící `WatchdogAddr` při 2+ agentech).
- **[U]** **Pročištění popisků v UI:**
  - Odstranit dlouhé úvodní odstavce pod nadpisy stránek a zřejmé popisky u běžných tlačítek/polí (`Hosts`, `Add-ons`, `Notifications`, `Monitoring`, `Settings`).
  - Ponechat nebo doplnit jen stručné inline nápovědy / tooltipy `?` tam, kde je to opravdu potřeba (regex v log kontrolách, Watchdog kvorum, `Read-only`, proxy hlavičky, cron úklidu disku).

### Okrajové stavy a bezpečnost
- **Sidecar `add-ons` stack:** pokud instalace nového sidecaru selže při `up`, vrátit `compose.yaml` do předchozího stavu, aby nepoškodil už běžící sidecary ve stejném stacku.
- **Denní úklid disku:** nikdy nemazat volumes automaticky bez explicitního povolení (zastavený stack by přišel o data); respektovat `readOnly` agenta.
- **Automatický rollback:** spustit maximálně jednou na jednu aktualizaci (`auto_rollback_attempted`), aby při trvalé externí chybě nedošlo k nekonečné smyčce.
- **Import dat:** atomická výměna SQLite (přes `VACUUM INTO` / zálohu předchozího souboru) a odmítnutí nekompatibilní verze schématu.

### Stav a rozhodnutí (2026-11-23, dokončení)
- **Add-ony ve společném stacku `add-ons`:** vypnutí nastaví službě `scale: 0` (profil kontejner nezastavil), odinstalace nejdřív škáluje na 0 a pak službu odebere (`up` neodstraňuje osiřelé kontejnery). Instalace čeká na výsledek `up` a při chybě vrátí předchozí `compose.yaml`; kolize jména služby s jiným add-onem se odmítne.
- **Public URL z Settings** se načte při startu (alerty, OIDC, pozvánky, status stránky); změna se projeví po restartu a UI na to upozorní. **Kanál aktualizací** se přepíná hned.
- **Automatický rollback:** jen pro záznam s aktérem `auto-update`, okno 13 min (3 min ticho po deployi + 10 min), pokus se zapíše před rollbackem (neúspěšný se neopakuje), souběžné DOWN se serializují.
- **Export zálohy** `.mbk`: šifrovaný heslem (scrypt + AES-256-GCM po 64KiB blocích, detekce useknutí), obsahuje DB hubu, `compose.yaml`/`.env` všech spravovaných stacků a `history.git` každého připojeného hosta; export je POST (heslo mimo URL) a je v auditu.
- **Import zálohy** ve dvou krocích: nahrání → dešifrování, kontrola SQLite (`quick_check`), verze schématu (novější odmítne) a náhled (počty, stacky po hostech, co se přepíše) → potvrzení s volbou DB / stacky. DB se vymění při restartu (stará zůstane jako `moorage.db.bak`), stacky se jen uloží (nic se nespustí). Historie se neimportuje, zůstává v souboru pro ruční obnovu.
- **Notifikace:** události `deploy_failed` (operace z UI a obnova stacku) a `agents` (offline / zpět online, s prodlevou a údržbou z watchdogu, pro všechny agenty); kanál může vypnout i alerty kontrol. **Web Push** bez externí knihovny (RFC 8291/8292, VAPID klíč v DB, service worker `/sw.js`), zapíná se v Account; posílá kontroly, hosty, selhané operace a rollbacky, ne informativní auto-updaty.
- **`.env` editor** proměnné z `compose.yaml` jen nabízí (čipy + *Add all*), nic nepřidává sám při otevření.
- Denní úklid disku zůstává ve výchozím stavu vypnutý.

### Pořadí realizace (po schválení)
1. UI úklid: **Hosts** + **Add-ons** (filtry, auto-katalog, společný stack `add-ons`) + **`.env` editor**.
2. Jádro z roadmapy: **Obnova smazaného stacku** → **Úklid disku s náhledem a denní úlohou** → **Automatický rollback po auto-update** → **Export/import konfigurace i dat**.
3. **Rozšíření notifikací** (události, kanály, UI toasty / Web Push).

---

## M15 – Přehled hostů, soubory stacků, hromadné akce, VPN add-on a sidecary (návrh 2026-11-24, čeká na schválení)

**Cíl:** Doplnit do jádra praktické nástroje pro každodenní správu hostů a stacků (soubory, disk/RAM, hromadné operace; přehled portů přesunut do M17B), přidat volitelný add-on pro Tailscale/NetBird a rozšířit `moorage-addons` o `log-shipper` a `node-exporter`.

> **[U] Architektonický princip:** Snažíme se **co nejméně zatěžovat jádro** (udržet nízkou paměť nadále hluboko pod limitem 100 MB PSS, malou binárku a nízké CPU). Věci pro užší skupinu uživatelů patří do **Add-ons** / **Sidecar add-ons**, a nové pohledy v jádru (porty, soubory, disk/RAM) čtou data **na vyžádání** nebo s lehkým intervalem bez zbytečných smyček na pozadí.

### 1. Jádro Moorage
1. **Přehled obsazených portů (Port map):** → **přesunuto do M17B (2026-11-26)**, protože se překrývá s kontrolou kolizí portů v linteru a se čtením nativních portů hosta. Specifikace je v M17 v sekci „Port map a kontrola kolize portů“.
2. **Prohlížeč a editor souborů ve složce stacku (`Files` tab v `StackDetail`):**
   - **[A]** Nový tab **Files** vedle `Compose` / `Logs` / `History`: strom/seznam souborů uvnitř složky stacku (mimo `.git`), náhled a editace textových souborů (`Caddyfile`, `nginx.conf`, `prometheus.yml`, skripty…) s maximální velikostí souboru (např. 512 KiB) a detekcí binárek (ty jsou jen ke stažení/info, ne k editaci).
   - **[A]** Bezpečnost: striktní kontrola cesty (`filepath.Rel` + odmítnutí `..` a symlinků mířících mimo složku stacku); respektuje `readOnly` agenta; uložení/vytvoření/smazání souboru se zapíše jako verze do `history.git` (pokud nejde o velké/vyloučené datové soubory) a do auditu (`stack.file_save`, `stack.file_delete`).
3. **Hlídání volného disku a RAM hosta:**
   - **[A]** Agent v `HostInfo` / novém periodickém reportu posílá využití disku (`/` a `StacksDir` / Docker root: celkem, volno, %) a RAM hosta (`MemTotal`, `MemAvailable` z `/proc/meminfo` + `statfs` na Linuxu).
   - **[A]** V UI přehledný ukazatel na kartě hosta (v `Home` i `Hosts`) a nový typ varování v **Diagnostics** + volitelná kontrola typu **Host resources** (práh zaplnění disku např. 85 % / 95 % a RAM), aby při blížícím se vyčerpání přišel alert stávajícími notifikačními kanály.
4. **Hromadné operace nad stacky (Bulk actions):**
   - **[A]** Na stránce hosta (`Home` / seznam stacků) přepínač výběru více stacků + lišta hromadných akcí (`Start / Up`, `Restart`, `Stop`, `Update images`, změna politiky auto-updatu).
   - **[A]** Backend (`POST /api/hosts/{host}/stacks/bulk`) spouští operace postupně ve frontě (ne 20 `compose up` najednou, aby nedošlo k přetížení CPU/RAM hosta), vrací průběžný stav a každou operaci řádně audituje; respektuje `readOnly` a `self` stack Moorage.

### 2. Add-on: Tailscale / NetBird detekce a správa portů (`vpn-expose`)
- **[U]** Volitelný vestavěný add-on v **Settings → Add-ons** (ve výchozím stavu vypnutý).
- **Detekce:** Agent zjišťuje přítomnost síťového rozhraní `tailscale0` / `wt0` (NetBird) a jeho IPv4/IPv6 adresy (případně DNS jména z lokálního rozhraní, pokud je dostupné bez nutnosti privilegovaného socketu) a zobrazí je u hosta a u služeb stacku.
  - **[A] (2026-11-26)** Detekci rozhraní a model adres hosta (`kind: vpn`) dodá už **M17B** (`engine/hostnet.go`, `host_addresses:<hostID>`). `vpn-expose` ji jen znovu použije a přidá akce nad `ports:`; vlastní detekci rozhraní nepíše.
- **Snadné vystavení na jeden klik / omezení portů na VPN síť:**
  - **[U]** Základem je **čistá úprava `ports:` v `compose.yaml`** (přepnutí `PORT:PORT` ↔ `<vpn_ip>:PORT:PORT` nebo zrušení veřejného vystavení `0.0.0.0`, s git commitem a `up` – funguje stejně pro Tailscale i NetBird bez speciálních práv).
  - **[U]** Pokud je v kontejneru agenta dostupný `tailscaled.sock`, add-on navíc nabídne **Tailscale Serve** (HTTPS vystavení služby do tailnetu na jeden klik).

### 3. Sidecar add-ons (`moorage-addons`)
1. **Log shipper (`log-shipper`):**
   - Kontejner (např. **Vector** nebo **Fluent Bit** s předkonfigurovaným čtením Docker container logů v read-only režimu) přidávaný do společného stacku `add-ons`.
   - Cílová URL (VictoriaLogs / Loki / OTLP) se konfiguruje přes `.env` společného stacku `add-ons`, čímž jádro Moorage zůstává bez ukládání historických logů.
2. **Node Exporter (`node-exporter`):**
   - Oficiální `prom/node-exporter` jako sidecar v `add-ons` (síť/pid hosta v read-only režimu pro CPU, disk I/O, síť, teplotu na Raspberry Pi).
   - Funguje po boku stávajícího vestavěného add-onu `prometheus-grafana` (`/metrics` Moorage pro stacky/sondy + `node-exporter` na `:9100` pro detailní metriky OS), s připraveným doporučením scrape jobu a Grafana dashboardu v README add-onu.

### 4. UI, opravy a dokumentace
1. **Oprava `settings/general`:**
   - Prověřit a opravit routování, ukládání a chování **Settings → General** (`#/settings/general` – zobrazení, formulář, diagnostika, generování metrics tokenu, záloha/obnova).
2. **Animace v UI:**
   - Jemné, rychlé přechody a mikrointerakce (rozbalování karet/sekcí, modály, toasty, přepínání pohledů) v souladu se systémem `.interface-design/system.md` („klidný přístav“) a s podporou `prefers-reduced-motion: reduce`. Bez animací přírůstků v log vieweru.
3. **Aktualizace dokumentace (`README.md` a docs):**
   - Aktualizovat `README.md` (a související dokumentaci/návody k nasazení) o všechny nové funkce (Settings → General, Add-ons a společný stack `add-ons`, zálohy `.mbk`, úklid disku, rozšířené notifikace, Web Push a novinky z M15).
4. **Odstranění duplicitních šablon (`templates`):**
   - Projít vestavěné šablony i katalog `moorage-templates` a odstranit duplicitní šablony (případně odduplikovat stejná ID mezi vestavěnými a vzdálenými zdroji).

### Mimo rozsah (odloženo do `ROADMAP.md`)
- GitOps sync (pull z vlastního Git repozitáře) a správa přihlášení k privátním registrům → zůstávají v `ROADMAP.md` na další vlnu.

### Okrajové stavy
- **Port map:** přesunuto do M17B (okrajové stavy tam).
- **Files editor:** nikdy nezapisovat mimo `StacksDir/<stack>`; při souběžné úpravě mimo Moorage se před uložením provede `syncExternal` do `history.git`.
- **VPN přepnutí portů:** pokud na hostu neběží Tailscale/NetBird (chybí IP rozhraní), tlačítko pro omezení na VPN zůstane neaktivní s vysvětlením.
- **Bulk actions:** při selhání jednoho stacku pokračovat u dalších nebo zastavit podle volby, na konci ukázat souhrn úspěchů/chyb.

---

## M16 – Opravy, Coolify & UI kvalita (Vlna A) + Headless CLI & interaktivní TUI (Vlna B) (implementováno 2026-11-25)

**Cíl:** Opravit klíčové UI/self-update/Coolify nedostatky, doplnit režimy Začátečník/Pokročilý/Vlastní, bezztrátový formulář + auto-odsazení Compose, 1-klik tipy linteru a dokumentaci (Vlna A), a následně zpřístupnit celé jádro Moorage přes skriptovatelné `moorage` CLI, volitelný headless režim bez web portu a interaktivní TUI `moorage cli` (Vlna B).

### Hlavní uživatelské toky
1. **Vlna A (Web UI, opravy, Coolify, režimy a editor Compose, dokumentace):** Nový uživatel si při prvním přihlášení (Setup, Invite, OIDC i Forward-Auth) jedním klikem vybere režim **Začátečník** (formulářové pole Compose, `.env` jako Variables, rozbalené tipy linteru s 1-klikem, Templates/Image jako výchozí cesta, rozšířené `<SetupHint>` průvodce, potvrzování i pro Stop/Restart/Down) nebo **Pokročilý** (YAML + Raw `.env`, kompaktní nápovědy, potvrzování jen pro destruktivní akce), případně si v **Account → Preferences** (s odkazem ze Settings) zapne režim **Vlastní**. V Compose editoru vloží neodsazený YAML a editor ho automaticky odsadí s možností `Ctrl+Z`; v linteru jedním klikem doplní `mem_limit`, chytrý `healthcheck`, konkrétní stabilní tag místo `latest` nebo chybějící proměnnou do `.env`. Admin otevře `Settings → General`, `Backups` či `Templates` bez rozbití nebo přetečení; v Clean modu smaže staré prázdné řádky při zachování vlastních nově přidaných řádků i původních komentářů; v celém UI vidí čitelné názvy Coolify stacků místo surových UUID a po self-updatu či restartu se stránka spolehlivě sama obnoví bez falešného rollbacku.
2. **Vlna B (Headless CLI & TUI):** Uživatel spustí `moorage` bez `MOORAGE_LISTEN` (nebo s `MOORAGE_WEB_UI=false`), takže proces naslouchá jen na lokálním Unix socketu `<data>/moorage.sock` (`0600`) bez otevřeného web portu; odkudkoli spouští `moorage stacks ...`, `moorage rollback`, `moorage restore`, `moorage templates`, `moorage convert`, `moorage checks` (lokálně i vzdáleně přes `--url` + API token), nebo otevře `moorage cli` pro živý interaktivní TUI layout ve stylu `lazydocker`.

### Rozdělení na vlny realizace
- **M16A (Vlna A — Hotovo):** Opravy UI/API (`settings/general`, `BackupsView`, `TemplatesView`), self-update/restart spolehlivost (`bootId`, `stackapply.go`, `container_helper.go`), globální Coolify názvy, odebrání Coolify/OIDC/Grafana tokenů, Clean mode prázdných řádků, `Dependencies` na hostu, režimy **Začátečník / Pokročilý / Vlastní** + onboarding, bezztrátový formulář Compose (`yaml` AST), 1-klik tipy linteru, auto-odsazení při vložení YAML, jednotné komponenty `Select.svelte` / `MultiSelect.svelte` napříč celým UI, `<AddonGate>` (1-klik povolení / instalace + manual override), kontextové průvodce `<SetupHint>`, dokumentace v repu (`docs/*.md`, `README.md`) i v aplikaci.
- **M16B (Vlna B — Hotovo):** `MOORAGE_WEB_UI` + Unix socket server (`<data>/moorage.sock`, `0600`), `api_tokens` v SQLite + `Bearer` auth + UI správa tokenů (`AccountView.svelte`), sdílený Go klient (`internal/cli/client`), všechny CLI příkazy jádra (`cobra` + `--json`), interaktivní TUI (`moorage cli` nad `moby/term` + `go-runewidth` bez nových externích závislostí) a dokumentace [`docs/cli.md`](docs/cli.md).

---

### Rozhodnutí (Vlna A i Vlna B)

#### Uživatelská rozhodnutí `[U]`
- **[U] Režim `MOORAGE_WEB_UI` bez web portu (Vlna B):** Web UI se automaticky vypne (`MOORAGE_WEB_UI=false`), pokud není nastaven `MOORAGE_LISTEN` (nebo je prázdný / `"off"`), případně když je `MOORAGE_WEB_UI=false` explicitně. `Dockerfile` a `compose.yaml` dál nastavují `MOORAGE_LISTEN=:5080`, takže stávající Docker/Compose instalace při self-updatu nikdy neztratí `:5080`.
- **[U] Jednotná architektura CLI a TUI ve dvou vlnách (Vlna B):** CLI i TUI komunikují s běžícím hubem/all-in-one procesem přes jednotné API: lokálně automaticky přes Unix socket (`<data>/moorage.sock`, práva `0600`), vzdáleně přes HTTPS `--url` + `Bearer` API token (`MOORAGE_TOKEN` / `--token`). Realizace je rozdělena na **Vlnu A** (nejdříve) a **Vlnu B** (následně).
- **[U] Compose Clean mode a prázdné řádky (Vlna A):** Clean mode v editoru skryje komentáře i původní prázdné řádky. Při úpravě a uložení z Clean modu se původní prázdné řádky trvale odstraní, zatímco prázdné řádky, které uživatel nově dopsal během editace, zůstanou zachované. Prázdné řádky uvnitř YAML blokových skalárů (`|` / `>`) se nikdy nemažou; nezměněné řádky si své komentáře ponechají.
- **[U] Zobrazení stacků v `Dependencies` (Vlna A):** Na stránce `Dependencies` se uvnitř boxu příslušného hosta zobrazují ve výchozím stavu **všechny stacky** včetně samostatných (bez hran, `showStandalone = true` jako výchozí), přičemž výchozí filtr rozsahu zůstává **Current host** (přepínač **All hosts** zůstává zachován).
- **[U] Režimy Začátečník / Pokročilý / Vlastní (Vlna A):** Režim řídí 6 přepínačů UX:
  1. Výchozí zobrazení Compose editoru (`form` vs. `yaml`),
  2. Výchozí zobrazení `.env` editoru (`vars` vs. `raw`),
  3. Linter tipy (`expanded` s 1-klikem vs. `collapsed`),
  4. Výchozí cesta pro nový stack (`guided` – Templates/Image vs. `yaml` – prázdný compose),
  5. Kontextové průvodce `<SetupHint>` a vysvětlivky (`extended` vs. `compact`),
  6. Potvrzování akcí (`all` – potvrzovat i Stop/Restart/Down vs. `destructive` – jen Delete/Rollback).
  Přepnutím kteréhokoli jednotlivého přepínače se režim automaticky přepne na **Vlastní** (`custom`).
- **[U] Bezztrátový formulář Compose pro režim Začátečník (Vlna A):** Záložka **Form** v `ComposeEditor.svelte` (`Form | Comments | Clean`) zobrazuje přehledná pole pro hlavní vlastnosti každé služby (`image`, `ports`, `volumes`, `environment`, `restart`, `networks`, `depends_on`, `mem_limit`, `healthcheck`, `labels` / `moorage.url`) + přidání/odebrání služby. Úpravy se promítají do YAML dokumentu cílenou aktualizací příslušných větví, takže pokročilá pole (`deploy`, `logging`, `cap_add`, `x-*`, komentáře…) zůstávají v souboru bezztrátově nedotčena (s informativním štítkem a odkazem na záložku YAML).
- **[U] Uložení a onboarding režimu per-user (Vlna A):** Preference se ukládají per-user v SQLite (`users.preferences_json`, endpoint `PUT /api/account/preferences`) a jsou kdykoli upravitelné v **Account → Preferences** (s rychlým odkazem ze **Settings**). Každý nový uživatel bez nastaveného profilu dostane po prvním přihlášení (Setup, Invite, OIDC i Forward-Auth) jednokrokovou obrazovku/modal pro volbu **Začátečník vs. Pokročilý**.
- **[U] 1-klik tipy v Compose linteru (Vlna A):** Rozšíření `internal/lint/fix.go` o jedno-klikové opravy tipů (a `missing-var`):
  - `no-mem-limit` → doplní `mem_limit: 512m` pod příslušnou službu;
  - `no-healthcheck` → doplní chytrou šablonu `healthcheck:` podle image/portu služby (PostgreSQL `pg_isready`, MySQL/MariaDB `mysqladmin`, Redis/Valkey `redis-cli ping`, HTTP/web `wget`/`curl` na exponovaný port, nebo obecný základ);
  - `latest-tag` → `GET /api/lint/image-tags` (přes `updater` logiku) dohledá nejnovější stabilní semver tag v registru a nabídne tlačítko **Pin to `<verze>`** (při nedostupném registru vysvětlující hláška bez selhání linteru);
  - `missing-var` (`deploy_error`) → jedním klikem přidá chybějící klíč do `.env` a přepne pozornost na jeho hodnotu.
- **[U] Jednotný `Select.svelte` a `MultiSelect.svelte` napříč celou aplikací (Vlna A):** Vytvořit jedno-výběrový protějšek `Select.svelte` ve stylu `MultiSelect.svelte` (klávesová navigace, vyhledávání od 8+ položek nebo volitelně vždy, podtitulek `meta` např. pro Coolify UID, tmavý/světlý motiv Moorage, volitelně `allowCustom` pro zadání vlastní hodnoty místo `<datalist>`) a nahradit **všechny** nativní `<select>`, `<datalist>` (`DepsView`, `StackDeps`, `MaintenanceForm`) i nekonzistentní ruční multi-výběry (`UpdatesView` notifikační kanály, `PromGenPanel` stacky → `MultiSelect`).
- **[U] Komponenta `<AddonGate>` – přímé povolení / instalace add-onu + Manual override (Vlna A):** Na každém místě, kde funkce vyžaduje vestavěný nebo sidecar add-on (`ConnectionsView` Coolify/Grafana, `BackupsView` Restic volumes, `TemplatesView` `official-templates`, `StackDetail` Coolify/proxy-labels, `AddonsView`), zobrazit `<AddonGate>` s 1-klik tlačítkem **Enable add-on** (u sidecarů **Install & enable** s `MultiSelect` výběrem hostů) a sekundární volbou **Manual override** tam, kde dává smysl ruční/externí konfigurace (vlastní Restic/zálohy, vlastní Prometheus/Grafana, vlastní Coolify/external řízení, vlastní zdroje šablon, ruční `moorage.url`), přičemž volba manual override se pamatuje a odemkne příslušnou manuální konfiguraci bez nucení instalovat add-on.

#### Architektonická rozhodnutí a opravy `[A]`
- **[A] Oprava `Settings → General` (`settings/general`) (Vlna A):**
  - `buildDiagnostics` v `internal/api/settings.go` aktuálně vrací `var out []diagnosticItem`, což se při nulovém počtu varování serializuje jako `"diagnostics": null` a `GeneralSettingsView.svelte` spadne na `cfg.diagnostics.length`.
  - Oprava: v Go vždy inicializovat `out := []diagnosticItem{}` (a stejná kontrola pro `HostLocks`) a v `GeneralSettingsView.svelte` přidat defenzivní `cfg.diagnostics?.length`.
- **[A] Opravy UI ze screenshotů (Vlna A):**
  - **Backups (`BackupsView.svelte`):** `.block` (i ostatní grid kontejnery) dostanou `grid-template-columns: minmax(0, 1fr)` a `min-width: 0`; nativní `<input type="file">` (jehož tlačítko „Procházet… Soubor nevybrán.“ roztahuje flex řádek přes pravý okraj karty) nahradíme stylovaným tlačítkem pro výběr `.mbk` souboru v design systému Moorage se zkrácením dlouhého názvu (`text-overflow: ellipsis`).
  - **Templates (`TemplatesView.svelte`):** ikona lupy má `position: absolute` a překrývá placeholder `Search templates` (protože chybí `padding-left` s vyšší prioritou / flex wrapper). Přepíšeme search pole na stejný flex container jako v `Shell.svelte` (ikona + `<input>` vedle sebe). U upozornění `Local folder` zobrazíme konkrétní cestu složky (`MOORAGE_TEMPLATES_DIR`) a odkaz do `Settings → Templates`.
- **[A] Spolehlivost self-updatu a restartu Moorage (Vlna A):**
  - **Falešný rollback při `Apply & restart` (`internal/selfupdate/stackapply.go`):** `waitStackHealthy` dnes předává do `pollContainerSessionHealth` původní `o.Version` – jakmile uživatel v compose změní tag image na novější verzi, kontrola verze selže a po 120 s provede rollback. Pro `self-stack-helper` nesmí selhat na neshodě staré verze (ověřuje se HTTP 200 `/api/session` a stabilita kontejneru `StableFor`).
  - **Falešný rollback přes síť (`internal/selfupdate/container_helper.go`):** helper u hubu ověřoval jen HTTP `/api/session` zevnitř jedné sítě; v multi-network/proxy/headless konfiguraci mohl selhat. Rozšíříme kontrolu tak, aby přijala jak HTTP `/api/session`, tak souborové potvrzení `/update-data/self-update-healthy.json` zapsané novým procesem po startu HTTP/socket serveru.
  - **Spolehlivý auto-reload UI (`Shell.svelte` & `StackDetail.svelte`):** Dnes UI čeká na zachycení výpadku (`sawDown`). Pokud se kontejner restartuje rychleji než za 2s poll interval, `sawDown` se nikdy nezachytí a stránka se neobnoví. Přidáme `bootId` (náhodné UUID/hex generované při startu procesu) do odpovědi `GET /api/session`; jakmile UI uvidí změněné `bootId` při HTTP 200 (nebo po `sawDown`), provede okamžitý reload.
- **[A] Odebrání výchozího Coolify tokenu (Vlna A):**
  - Backend v `internal/api/connections.go` už podporuje `clearToken: true` (`clearClientSecret`, `clearGrafanaToken`), ale `ConnectionsView.svelte` ho neposílá. Přidáme tlačítko **Clear token** (pro default Coolify token, per-host Coolify přepisy, OIDC Client Secret i Grafana token), které při uložení pošle příznak smazání.
- **[A] Globální dekódování Coolify UID → čitelná jména (Vlna A):**
  - Přidáme jednotný resolver v `coolifyhub.go` (a fallback z labelů `coolify.resourceName`/`coolify.name`), který obohatí `displayName` (a `coolifyProject`/`coolifyUuid`) nejen v seznamu stacků, ale ve všech API DTO a UI pohledech: **Image updates** (`UpdatesView.svelte`), **Dependencies** (uzly grafu, boční panel, selecty při přidávání hrany), **Monitoring** (`CheckForm` výběr stacku, `ChecksView`, `CheckDetail`, alerty), **Logs**, **Audit**, **Maintenance**, **Status pages**, **Scheduler/Webhooks/MCP** v `BuiltinAddonPanel` a globální hledání **`Ctrl+K`** (`CommandPalette.svelte`).
- **[A] Kontextové průvodce `<SetupHint>` a srozumitelné akční hlášky (Vlna A):**
  - Jednotná znovupoužitelná komponenta `<SetupHint>` pro stavy, kdy funkce není nakonfigurována (např. chybějící notifikační kanál při zakládání kontroly, chybějící Coolify URL/token, prázdný `MOORAGE_TEMPLATES_DIR`, vypnutý terminál/host-terminal/exec, chybějící veřejná URL nebo agent watchdog adresa). Každá hláška jasně řekne **co chybí**, **kde to nastavit** a nabídne přímý odkaz (`#/settings/...`) nebo zkopírovatelnou `MOORAGE_*` proměnnou u env zámků.
- **[A] Dokumentace (Vlna A):**
  - Aktualizace `README.md` (M10–M16A funkce) a vytvoření strukturovaných návodů v `docs/` (Coolify integrace, Connections & OIDC, Backups `.mbk` & S3/WebDAV, Add-ons: Scheduler, Socket Guard, Registry Webhooks, MCP Server, Prometheus/Grafana, Terminal & Host locks).
  - Doplnění pole `docsUrl` / in-app náhledu dokumentace ke všem vestavěným add-onům v `addons.go` a `AddonsView.svelte` (včetně Coolify integrace), aby žádný add-on nebyl bez dokumentace.
- **[A] Auto-odsazení neodsazeného Compose při vložení (`composeIndent.ts` + `CodeEditor.svelte`) (Vlna A):**
  - Při vložení (`paste`) v YAML režimu detektor ověří, zda vložený text není rozbitý / neodsazený (známé top-level a service klíče Compose Spec na sloupci 0 bez odpovídajícího vnoření, případně tabulátory `\t`). Již validně odsazený YAML se nikdy neupravuje.
  - Neodsazený text se zrekonstruuje podle hierarchie Compose Spec (`services` → `<service>` → `image`, `ports`, `volumes`, `environment`, `healthcheck`, `deploy`… → položky seznamů `- ...` a vnořené mapy) s respektem k odsazení řádku na pozici kurzoru (pro vložení jedné služby).
  - Změna proběhne jako standardní CodeMirror transakce: **`Ctrl+Z` vrátí původní surově vložený text** a pod editorem se zobrazí diskrétní lišta *„Auto-indented pasted YAML · Undo“*. V hlavičce editoru bude i ruční tlačítko **Fix indent**.
- **[A] CLI + TUI vrstva (`M16B` – Vlna B):**
  - **Transport & Auth:** Když běží lokální instance na stejném stroji, hub/all-in-one otevře Unix domain socket `<data>/moorage.sock` (`0600`). Vzdálené volání (`--url https://...`) používá dlouhodobé **API tokeny** uložené v SQLite tabulce `api_tokens` (hashované SHA-256, volitelný scope `read` / `deploy` / `admin`, expirace, správa v `Settings → Account / Connections` i přes `moorage token`).
  - **CLI příkazy (`github.com/spf13/cobra`, již v `go.mod`):** Plné pokrytí jádra včetně `--host`, `--json`, `--quiet`, `-y`:
    - `moorage stacks list|status|up|down|restart|stop|pull|update|delete|logs|stats`
    - `moorage compose get|edit|validate|lint|clean`
    - `moorage history list|diff|rollback|restore` (vč. obnovy smazaného stacku)
    - `moorage convert <docker run ...>` (funguje i offline bez běžícího hubu)
    - `moorage templates list|inspect|deploy`
    - `moorage checks list|test|pause|resume` & `moorage maintenance list|create|delete`
    - `moorage hosts list|pair|remove|prune`
    - `moorage backup export|preview|restore` & `moorage system update|rollback|status`
  - **Interaktivní TUI (`moorage cli`):** Nad `charmbracelet/bubbletea` + `lipgloss` v barevné paletě Moorage („klidný přístav“). Panely: Hosté/Stacky vlevo (s Coolify skupinami a dekódovanými jmény), vpravo přepínatelné záložky Kontejnery & Stats / Živé logy (regex filtr + follow) / Compose & Env / Historie & Rollback / Kontroly & Alerting; horní lišta s globálním stavem, klávesové zkratky (`?` nápověda, `/` filtr, `u/d/r` akce, `l` logy, `t` exec shell, `Tab` přepínání panelů, `q` konec).
  - **Pravidlo udržitelnosti:** Nové funkce v jádru Moorage se od M16B navrhují jako API-first endpointy sdílené mezi Web UI, CLI a TUI (žádná duplicitní byznys logika v CLI).

---

### Okrajové stavy
- **Clean mode prázdné řádky:** `mergeClean` rozlišuje původní skryté prázdné řádky (které se při úpravě z Clean modu nevrací zpět) od nových prázdných řádků zadaných uživatelem v editoru (ty se zapíší). Blokové skaláry (`|`/`>`) se nesmí ořezávat, aby se nezměnil obsah konfiguračního souboru nebo certifikátu.
- **Formulář Compose & neznámá pole:** Pokud `compose.yaml` obsahuje YAML kotvy (`&`/`*`), nevalidní syntaxi nebo pokročilé sekce mimo formulář, záložka **Form** zachová nedotčenou strukturu při úpravě podporovaných polí, a při nevalidním YAML zobrazí banner s přepnutím do YAML záložky.
- **Auto-odsazení při paste:** Pokud uživatel vloží platný už odsazený YAML nebo víceřádkový blokový skalár, auto-indenter nezasahuje; při nechtěné úpravě funguje okamžitý `Ctrl+Z` / tlačítko *Undo*.
- **1-klik `latest-tag` bez přístupu k registru:** Pokud registr neodpovídá nebo vyžaduje přihlášení, dotaz vrátí srozumitelnou hlášku bez blokování editoru a linteru.
- **Onboarding u stávajících uživatelů po upgradu:** Stávající uživatelé s prázdným `preferences_json` po upgradu mohou dostat jednorázový výběr režimu (nebo plynule pokračovat výchozím nastavením a změnit si ho v `Account → Preferences`).
- **Rychlý restart (< 2 s) při self-updatu:** Pokud nový kontejner naběhne mezi dvěma HTTP polly, UI díky změně `bootId` pozná nový proces i bez zachycení výpadku sítě a provede reload.
- **Headless mód (`MOORAGE_WEB_UI=false`):** Self-update helper nemůže volat HTTP `/api/session`; potvrzuje zdraví přes `<data>/self-update-healthy.json` a `moorage.sock`. V tomto režimu jsou `MOORAGE_OIDC_*` a status page port neaktivní (pokud není výslovně nastaven `MOORAGE_STATUS_LISTEN`).
- **Offline CLI příkazy:** `moorage convert`, `moorage compose validate --local` a `moorage version` fungují i bez běžícího hubu/socketu.
- **Read-only host & host locks:** Všechny CLI/TUI akce i Coolify akce respektují `readOnly` a `selfStack` na straně serveru i agenta (CLI vypíše srozumitelnou chybu a nenulový exit code).
- **Coolify API nedostupné:** Pokud je Coolify API offline nebo bez tokenu, resolver plynule spadne na labely kontejnerů (`coolify.resourceName` / `coolify.name`), takže se neztratí jména běžících stacků.
- **TUI terminál & rozměry:** Při malém okně terminálu TUI přepne na kompaktní rozložení jednoho aktivního panelu; při odpojení socketu zobrazí stavový banner a automaticky se znovu připojí.
- **RAM limit (< 100 MB PSS):** Přidání `bubbletea`/`lipgloss` do binárky nesmí zvýšit klidovou RAM serveru (TUI se inicializuje výhradně při spuštění příkazu `moorage cli`).

---

### Databázové migrace, API a pořadí vydání

1. **Vlna A (DB migrace `preferences_json` + UI/API opravy):**
   - DB migrace (aditivní): sloupec `preferences_json TEXT NOT NULL DEFAULT ''` v tabulce `users` (případné prázdné `''` znamená nevybraný onboarding profil).
   - API: `PUT /api/account/preferences` + rozšíření `/api/session` a `/api/account` o `preferences` a `bootId`; non-nil `diagnostics` & `hostLocks` v `/api/settings/config`, globální `displayName` / `coolifyProject` / `coolifyUuid` v DTO endpointů (updates, deps, monitoring, logs, audit, maintenance, status-pages, addons, search), `docsUrl` v `/api/addons`, rozšíření `internal/lint/fix.go` (`add-mem-limit`, `add-healthcheck`, `pin-tag`, `add-env-var`) a lookup stabilního tagu z registru.
   - Self-update: oprava `stackapply.go` a `container_helper.go`.
   - Frontend: onboarding výběr **Začátečník / Pokročilý** + **Account → Preferences** (režim `Vlastní`), bezztrátový **Form** tab v `ComposeEditor.svelte`, `composeIndent.ts` auto-odsazení v `CodeEditor.svelte` (`Ctrl+Z` + Undo bar + **Fix indent**), nový `Select.svelte` a sjednocení všech `<select>`/`<datalist>`/multi-výběrů na `Select`/`MultiSelect`, `<AddonGate>` (1-klik Enable/Install & Manual override), oprava `GeneralSettingsView`, `BackupsView`, `TemplatesView`, `ConnectionsView` (Clear token), `yamlcomments.ts` (Clean mode), `DepsView` (`showStandalone = true`), `Shell.svelte`/`StackDetail.svelte` (`bootId` reload), `<SetupHint>` napříč obrazovkami, `AddonsView` dokumentace.
   - Docs: aktualizace `README.md` + `docs/*.md`.
   - **Ověření & vydání A:** `go test ./...`, `svelte-check`, `vite build`, kontrola v prohlížeči (desktop i mobilní šířka) → vydání verze po schválení.
2. **Vlna B (DB migrace + CLI/TUI):**
   - DB migrace: nová tabulka `api_tokens (id, name, prefix, token_hash, role, scopes_json, created_by, created_at, expires_at, last_used_at)`.
   - Backend: Unix socket listener `<data>/moorage.sock` (`0600`), middleware pro `Bearer` API tokeny, `GET/POST/DELETE /api/tokens`, `MOORAGE_WEB_UI` v `config.go`.
   - CLI/TUI: `internal/cli/client`, `internal/cli/cmd/*`, `internal/cli/tui/*`, zapojení do `cmd/moorage/main.go`.
   - **Ověření & vydání B:** unit a integrační testy CLI přes Unix socket i token, testy TUI modelu, `go test ./...`, kontrola binární velikosti a klidové RAM (`< 100 MB PSS`).

---

### Mimo rozsah (odloženo do `ROADMAP.md`)
- Automatické generování shell completions (`bash` / `zsh` / `fish`) a multi-profilový správce připojení (`~/.config/moorage/config.json`) → `ROADMAP.md`.
- Lokální CLI přes Windows Named Pipe (na Windows zatím přes `--url` + API token) → `ROADMAP.md`.
- Vlastní uživatelské barevné motivy a remapování kláves v TUI → `ROADMAP.md`.

### Otevřené otázky
- Žádné blokující otázky pro **Vlnu A**.
- Pro **Vlnu B** při zahájení případně upřesníme preferované klávesové zkratky v TUI (`lazydocker` styl vs. vim `j/k` navigace – navrhneme obojí současně).

---

## M17 – Spolehlivost, bezpečnost, Host CLI, Zero-config Add-ons, Port map, Multi-IP URL a bohatá oznámení (návrh 2026-11-25, rozšířeno 2026-11-26)

**Cíl:** Opravit nahlášené chyby v UI a provozu (Logs dropdown, přidání checku, výchozí `hideIgnored` v Dependencies, automatické složky v `/data`, webový terminál, falešný update `moorage` image, párování dalších hostů, nápověda k externím compose složkám a hostovské CLI), zpevnit bezpečnost WebSocketů a relací, hlídat paměťový limit v CI. Následně dotáhnout zero-config sidecar add-ony s automatickou aktualizací na `:latest`, přehled obsazených portů s kontrolou kolizí v linteru (převzato z M15), multi-IP adresy hosta (Ethernet / Wi-Fi / VPN) pro odkazy na služby a bohatá oznámení (embedy, předvolby obsahu, odesílatel `<host> • Moorage <role>` s logem Moorage).

### Hlavní uživatelské toky
1. **Vlna A (Opravy UI, stability, `/data`, terminálu, image updatů, párování hostů a Host CLI):**
   Uživatel otevře `Logs` → dropdown `All host containers` bez překrývajících se řádků; v `Monitoring` bez chyby přidá nový check; v `Dependencies` má automaticky skryté ignorované hrany. Moorage si při startu sám vytvoří `/data/templates` i ostatní datové podsložky, po self-updatu nehlásí falešnou aktualizaci image `:latest`, spolehlivě připojí další hosty i při opakovaném párování, přes reverse proxy otevře webový terminál (nebo ukáže jasný důvod, proč nejde), u externích stacků mimo bind-mount napoví přesnou cestu k připojení a z terminálu hosta spustí `moorage` příkazy jednorázovým instalačním skriptem. Terminál se otevírá přes jednorázový tiket, odhlášení nebo změna hesla okamžitě ukončí otevřená živá spojení a CI hlídá, že Moorage nepřekročí 100 MB PSS.
2. **Vlna B (Zero-config Add-ons, Port map, Multi-IP URL a bohatá oznámení):**
   Uživatel klikne na sidecar add-on a rovnou ho spustí bez ručního vyplňování cest nebo `.env` — stack `add-ons` má automaticky zapnuté aktualizace (`policy=auto`) a používá `:latest` bez zbytečných varování linteru; built-in addony se aktualizují automaticky s Moorage. Na hostu otevře přehled **Ports** se všemi obsazenými porty a linter již při psaní `compose.yaml` upozorní na kolizi publikovaných portů proti ostatním stackům, standalone kontejnerům i nativním portům hosta. U každého hosta lze mít více pojmenovaných adres (Ethernet, Wi-Fi, VPN) s volbou výchozí adresy pro hlavní odkaz služby a rychlým výběrem ostatních adres. Alert v Discordu přijde jako přehledný embed od odesílatele `nas • Moorage agent` s logem Moorage a u každého kanálu jde zvolit, kolik toho zpráva obsahuje (Minimal / Standard / Detailed / Custom).

### Rozdělení na vlny realizace
- **M17A (Vlna A – Hotovo 2026-11-26, commit 97d65a2):** všech 12 bodů níže implementováno; ověřeno `go test ./...` (na Windows jen stejné platformní chyby jako čistý `HEAD`), `svelte-check`, `vite build`, `{@html}` pojistka (bash i PowerShell). PSS gate na Windows nejde spustit (hlásí technickou chybu, exit 2), poběží v CI na Linux runneru. Ruční ověření v prohlížeči / na reálných hostech zbývá (párování, terminál za reverse proxy, Logs dropdown, přidání checku).
- **M17A (Vlna A – Opravy, stabilita, bezpečnost & Host CLI):** `MultiSelect.svelte` + `LogViewer.svelte` dropdown, `StatusCodes.svelte` + `checks.ts` + `CheckForm.svelte` (blank page fix), `DepsView.svelte` (`hideIgnored = true`), automatické vytváření `/data/*` složek (`config.go`, `templates/service.go`, `TemplatesView.svelte`), oprava webového terminálu (`api/terminal.go`, `host_terminal.go`, `TerminalPanel.svelte`), oprava false update u self-updated image (`selfupdate/container.go`, `engine/images.go`), spolehlivé párování hostů (`tunnel/agent.go`, `tunnel/hub.go`, `store/agents.go`, `HostsView.svelte`), `<SetupHint>` pro externí compose složky (`StackDetail.svelte`), Host CLI (`Dockerfile` `/usr/local/bin/moorage` symlink, `scripts/install-cli.sh`, `docs/cli.md`, `README.md`, `AccountView.svelte`), bezpečnostní balík WebSocketů a relací (tiket pro terminál, ukončení živých spojení při revokaci, regresní testy cookies/CSP/Origin), CI pojistka proti `{@html}`, CI PSS Memory Gate.
- **M17B (Vlna B – Hotovo 2026-11-26, sloučeno z větve `m17b`):** všech 7 částí implementováno a ověřeno testy; ruční ověření reálných oznámení (Discord / Telegram / ntfy) a panelů Ports / Addresses na reálných hostech zbývá.
- **M17B (Vlna B – Add-ons, Port map, Multi-IP URL & oznámení):** Auto-update pro `add-ons` + potlačení `latest-tag` + zero-config instalace v `Moorage` i `moorage-addons` (výchozí cíl záloh `/data/backups`), Port map převzatá z M15 (`GET /api/hosts/{host}/ports` + karta **Ports**), zachování `HostIP` v portech kontejnerů + standalone/host port kolize už při živé editaci v linteru, multi-IP adresy hosta (`net.Interfaces()` pro nativní/host-network běh + ruční pojmenované adresy v `HostsView`) a víceadresové odkazy na služby, bohatá oznámení (embedy, předvolby obsahu, identita odesílatele s logem Moorage).

---

### Rozhodnutí

#### Uživatelská rozhodnutí `[U]`
- **[U] Automatické vytváření složek v `/data` (Vlna A):** `internal/config/config.go` (`FromEnv`) automaticky vytvoří nejen `DataDir` a `StacksDir`, ale i `TemplatesDir` (a případné další interní podsložky v `/data`) pomocí `os.MkdirAll(..., 0o750)`. Prázdná složka `/data/templates` je normální stav bez chybového nebo varovného banneru.
- **[U] Používání `moorage` CLI na hostu mimo kontejner (Vlna A):**
  1. V `Dockerfile` doplnit symlink `/usr/local/bin/moorage -> /moorage` (a `PATH`), aby fungovalo `docker exec -it moorage moorage ...`.
  2. Přidat lehký instalační skript `scripts/install-cli.sh` + návod v `docs/cli.md`, `README.md` a in-app nápovědu v `AccountView` pro dva režimy:
     - **Docker wrapper** (`/usr/local/bin/moorage` delegující na `docker exec` s automatickou detekcí TTY `-it` vs. neinteraktivní stdin — funguje ihned bez bind-mountu socketu a bez API tokenu na lokálním hostu),
     - **Nativní binárka** z GitHub Releases (připojení přes bind-mountovaný `/data/moorage.sock` nebo vzdáleně přes `MOORAGE_URL` + `MOORAGE_TOKEN`).
- **[U] Externí compose složky bez přístupu (`MOORAGE_MANAGE_EXTERNAL`) (Vlna A):** Moorage respektuje sandbox kontejneru a nedovolí upravovat compose soubory ze složky, ke které nemá v kontejneru přístup. Pokud je `MOORAGE_MANAGE_EXTERNAL=true` a projekt zůstane na Level 1 (read-only Docker API akce), zobrazí `StackDetail.svelte` srozumitelný `<SetupHint>` s konkrétní `working_dir` cestou z labelu a zkopírovatelným bind-mount řádkem (`- <working_dir>:<working_dir>`) pro `compose.yaml` Moorage.
- **[U] Add-ons: auto-update, `:latest` a zero-config sidecary (Vlna B):**
  - Built-in addony jsou součástí binárky Moorage a aktualizují se automaticky s ní (v UI bez matoucího ručního verzování).
  - Při prvním vytvoření stacku `add-ons` se mu automaticky nastaví update policy na `auto` (`store.PolicyAuto`) s možností přepnout na `notify` v UI. Oficiální katalogové sidecary používají tag `:latest` a linter pro stack `add-ons` potlačí informační tip `latest-tag`.
  - Zero-config instalace: `installAddon` automaticky předvyplní host-specifické proměnné do `add-ons/.env` (`MOORAGE_STACKS_DIR`, `TZ`, výchozí `HOST_IP`, výchozí lokální volumes/porty) a šablony v `moorage-addons` (`restic-backup`, `trivy-scanner` a další) budou mít rozumné funkční výchozí nastavení ihned po přidání a spuštění (pokročilé cíle jako S3/B2 zůstanou volitelným rozšířením).
  - **(2026-11-26)** Výchozí lokální cíl záloh `restic-backup` je podsložka **`/data/backups`** (na hostu ve stejném bind-mountu jako ostatní data Moorage, takže je vidět a jde snadno zkopírovat jinam). Moorage ji vytváří spolu s ostatními podsložkami v `/data`. Named volume zůstává jako volba v `.env`.
- **[U] Detekce síťových rozhraní, hostovských portů a více adres hosta (Vlna B):**
  - Automatická detekce rozhraní hosta (`net.Interfaces()`) a nativních naslouchajících portů (`/proc/net/{tcp,tcp6,udp,udp6}`) běží tehdy, když Moorage nebo agent běží jako nativní binárka nebo v `network_mode: host`.
  - Pro standardní Docker bridge kontejner je v `HostsView.svelte` k dispozici ruční správa pojmenovaných adres hosta (Ethernet / Wi-Fi / VPN / doména / vlastní) s označením jedné výchozí adresy.
- **[U] Překryv s M15 (2026-11-26):** Přehled obsazených portů (Port map) se přesouvá z M15 do M17B, protože sdílí zdroje dat s kontrolou kolizí v linteru. Detekce VPN rozhraní pro `vpn-expose` (M15) znovu použije model adres hosta z M17B.
- **[U] Bezpečnostní balík, CI pojistky a PSS gate do M17A (2026-11-26):** Převzato z doporučení po analýze konkurence. Konkrétní podoba je v architektonických rozhodnutích níže.
- **[U] Platformní digesty pro falešný update (Vlna A, 2026-11-26):** Docker `DistributionInspect` vrací v `Platforms` jen os/arch bez digestů, proto Moorage při neshodě s index digestem jednou (cache podle index digestu) stáhne z registru image index a přijme shodu s kterýmkoli platformním manifestem; selhání registru = dnešní srovnání (bezpečný fallback).
- **[U] Bohatá oznámení (Vlna B, 2026-11-26):**
  - **Odesílatel = dotčený host + jeho role:** jméno webhooku je `<host> • Moorage agent` (událost na hostu s agentem), `<host> • Moorage hub` (událost na hostu hubu nebo událost celého hubu) a `<agent> • Moorage watchdog` (watchdog hlásí výpadek hubu). Jméno hosta je jeho název v Moorage (výchozí = hostname).
  - **Ikona = jen logo Moorage**, ve variantách hub / agent / watchdog. Ikony jednotlivých služeb se nepoužívají.
  - **Čtyři předvolby obsahu na každém kanálu:** Minimal / Standard / Detailed / **Custom**. Custom odemkne přepínače jednotlivých částí zprávy. Texty zůstávají pevně v kódu (žádné uživatelské šablony).
  - **Embed tam, kde to služba umí**, s volbou „prostý text“ u kanálu.

- **[U] (2026-11-26, Vlna B)** Zero-config `restic-backup`: chybějící `RESTIC_PASSWORD` se při instalaci vygeneruje do `add-ons/.env` (nikdy se nepřepíše), UI upozorní na zálohu hesla.
- **[A] (Vlna B, odchylky)** Generický webhook už klíč `event` používal pro stav (down/up/test), proto je druh události v novém klíči `eventKind`. Telegram dává tlačítko s odkazem jen u doménových URL (localhost / IP by Telegram odmítl), jinak odkaz v textu. Discord escapuje `-`/`#`/`+` jen na začátku řádku. Port map a linter sdílí sběr s 5s cache na hosta. Výchozí logo URL = `cdn.jsdelivr.net/gh/Literally-davca/moorage@main/docs/assets/notify`; přepsání je v kartě „Sender logo“ na stránce Notifications. Ports a Addresses jsou panely pod seznamem hostů (`HostPorts.svelte`, `HostAddresses.svelte`).

#### Architektonická rozhodnutí a opravy `[A]`
- **[A] Logs dropdown (`MultiSelect.svelte` + `LogViewer.svelte`) (Vlna A):** Přepnout `.opts` z `display: grid` na `display: flex; flex-direction: column; gap: 1px;` (opraví kolaps výšky řádků v Chromiu při zalamování textu), dát `.opt` `flex-shrink: 0` a dvousloupcový layout (checkbox vlevo, `label` + `meta` v pravém sloupci) a povolit širší rozbalovací panel (`min-width: 280px; width: max-content; max-width: min(420px, calc(100vw - 32px))`).
- **[A] Prázdná stránka při přidání checku (`StatusCodes.svelte`, `checks.ts`, `CheckForm.svelte`) (Vlna A):** Odebrat výchozí hodnotu z `$bindable()` v `StatusCodes.svelte` (`value = $bindable()`), aby Svelte 5 nevyhazoval fatální chybu `props_invalid_value` při `bind:value={undefined}`, a navíc inicializovat `status: ''` v `emptyCheck()`, `toInput()` i `setKind()`.
- **[A] Dependencies výchozí filtr (Vlna A):** Změnit výchozí hodnotu `hideIgnored` v `DepsView.svelte` na `$state(true)`.
- **[A] Webový terminál – spolehlivost a diagnostika (Vlna A):**
  - V `internal/api/terminal.go` při `websocket.Accept` korektně ověřovat `Origin` i za reverse proxy (`X-Forwarded-Host` / povolené původy bez oslabení ochrany proti cross-site WebSocket hijacking). **Výchozí kontrola knihovny `coder/websocket` (Origin host == `Host`) zůstává.** Navíc se povolí jen host z `MOORAGE_PUBLIC_URL`; `X-Forwarded-Host` se bere v úvahu jen od důvěryhodné proxy (stejné pravidlo jako u forward-auth). Nikdy ne `InsecureSkipVerify` ani `*`.
  - V `TerminalPanel.svelte` zobrazovat přesný důvod selhání spojení (zámek `MOORAGE_TERMINAL` / `MOORAGE_HOST_TERMINAL`, `MOORAGE_READ_ONLY`, ochrana vlastního kontejneru `ErrSelfStack`, proxy `403`, chybějící `/bin/sh` ve `scratch`/`distroless` image).
  - V `internal/engine/host_terminal.go` odebrat slepé `AutoRemove: true` u helper kontejneru a před jeho explicitním smazáním přečíst exit code / stderr, aby se neztrácela skutečná příčina pádu.
  - Opravit periodickou kontrolu session v `serveTerminalSession`, aby nerozpojovala forward-auth relace bez cookie.
- **[A] Falešná aktualizace u `moorage` image (Vlna A):**
  - Po úspěšném self-updatu v `internal/selfupdate/container.go` aktualizovat/re-tagnout lokální tag nebo vyhodnocovat stav přímo proti běžícímu kontejneru (`Container.ImageID` / `RepoDigests` běžícího image).
  - V `internal/engine/images.go` (`InspectRemote` a `hasMatchingDigest`) porovnávat vedle manifest list / OCI index digestu také dílčí platform-specific digesty z `res.Platforms`, aby image pullnutý přes `@sha256:<platform-digest>` nevykazoval falešnou aktualizaci vůči multi-arch `:latest`.
- **[A] Spolehlivé přidávání dalších hostů (Vlna A):**
  - `agent.enroll()` v `internal/tunnel/agent.go` dostane retry smyčku s exponenciálním backoffem místo okamžitého ukončení procesu při přechodné chybě sítě/DNS/TLS.
  - `RedeemJoinToken` v `internal/store/agents.go` a `hub.go` zajistí bezpečnější párování (token s krátkou tolerancí opakovaného pokusu do prvního navázání mTLS spojení nebo jasné oddělení stavu `pending` / `paired`), aby výpadek odpovědi nenechal v DB mrtvý záznam se spáleným tokenem.
  - Pokud má agent v named volume starý `agent.crt` (odstraněný z hubu nebo z jiného hubu), ale má nastavený nový `MOORAGE_JOIN_TOKEN`, při `403` automaticky přepíše certifikát novým párováním.
  - Sanitizovat `MOORAGE_HUB` (ořez mezer, odstranění zdvojeného `https://` / `wss://` a koncového `/`), opravit reconnect race v `hub.go` (nevolat `OnDisconnect(id)`, pokud se mezitím připojila nová session), a v `HostsView.svelte` zobrazit seznam čekajících join tokenů s možností zrušení i upozornění na publikaci portu `5081` / proxy adresu.
- **[A] Bezpečnostní balík WebSocketů a relací (Vlna A, 2026-11-26):** Navazuje na úpravy terminálu; vychází z poučení z CVE konkurence (CSWSH u Dozzle a Nginx-UI).
  - **Jednorázový tiket pro terminál:** `POST /api/terminal/ticket` (běžné autentizované API volání, takže funguje s cookie, OIDC i forward-auth) vrátí náhodný tiket platný 30 s, použitelný jednou a vázaný na uživatele, relaci a cíl (kontejner / host). Upgrade `/api/.../terminal` vyžaduje `?ticket=` **a** projde kontrolou `Origin`. Tím odpadá i problém periodické kontroly cookie u forward-auth relací: po otevření se relace ověřuje přes uloženého uživatele (zda nebyl odebrán / degradován na viewer), ne přes cookie.
  - **Ukončení živých spojení při revokaci:** registr otevřených WebSocketů (`/api/ws` události, logy, terminál, host terminál) podle relace a uživatele. Změna hesla, odhlášení, odebrání uživatele, změna role na `viewer`, revokace API tokenu nebo relace v UI okamžitě zavře dotčená spojení (close kód `4401`, UI ukáže „Session ended, sign in again“).
  - **Regresní testy místo přepisu:** session cookie už je `HttpOnly; SameSite=Strict; Secure` (za HTTPS) a CSP už existuje (`api.go`). Přidají se testy, které tyto hlavičky a kontrolu `Origin` u všech `websocket.Accept` v `internal/api` hlídají, aby je budoucí změna potichu neoslabila. OIDC state cookie (`users.go`) zůstává `Lax` kvůli redirectu z IdP.
- **[A] Upřesnění při implementaci Vlny A (2026-11-26):**
  - Origin: host z efektivní veřejné URL (`MOORAGE_PUBLIC_URL` nebo hodnota ze Settings), `X-Forwarded-Host` jen od `MOORAGE_TRUSTED_PROXIES` (forward-auth pravidlo); všechny upgrady v `internal/api` jdou přes `acceptWS` (`api/wslive.go`), hlídá test.
  - Terminálový upgrade se přihlašuje jen tiketem (cookie samotná ho neotevře), neplatný tiket = `401` před upgradem; `POST /api/terminal/ticket` zároveň předem ověří host (neznámý / offline jako běžná HTTP chyba).
  - Revokace: po každé úspěšné revokující akci (odhlášení, heslo, TOTP, role, zakázání/smazání uživatele, relace, API token, dokončený reset) se všechna živá spojení znovu ověří proti DB a neplatná zavřou `4401`; běžící terminál navíc dál periodicky (bez cookie, přes uložené přihlášení).
  - `/data`: vytváří se `templates` a `backups` (`Config.BackupsDir`); selhání podsložky je varování v logu, ne pád startu.
  - Párování: `join_tokens` + `agent_id`, `redeemed_at` (aditivní migrace); opakovaný pokus týmž tokenem do 15 min, dokud se agent poprvé nepřipojí, pak se token smaže; nové `GET` / `DELETE /api/agents/join-tokens`; zrušení smaže i nikdy nepřipojeného agenta. `MOORAGE_HUB` bez portu dostane `:5081`. Re-pair i při tokenu pro jiný hub (jiný CA otisk než uložený `ca.crt`).
  - Self-update přetaguje jen plovoucí tag (`latest`, `stable` …) stejného repozitáře, nikdy verzi; kontrola updatu bere v úvahu i image běžícího kontejneru.
  - Host shell helper už není `AutoRemove`; před dalším startem se uklidí skončené helpery (`moorage.role=host-shell`, `exited`).
  - Host CLI: výchozí `compose.yaml` nemá `container_name`, proto návody používají `docker compose exec moorage moorage …` a wrapper hledá kontejner podle compose služby `moorage`.
  - PSS gate běží i v `release.yml` (build job) a výsledek jde do release notes (sekce Memory); MB = MiB.
- **[A] CI pojistka proti `{@html}` (Vlna A):** Krok v `.github/workflows/ci.yml` (a `scripts/dev/check.*`) selže, pokud se `{@html` objeví v jiném souboru než `web/src/lib/Icon.svelte` (statické SVG ikony). Text z kontejnerů, logů a labelů se vždy vykresluje jako text.
- **[A] CI PSS Memory Gate (Vlna A):** Nový CI job spustí Moorage nad testovacím Docker daemonem se simulovanou zátěží (např. 15 fake agentů přes tunel, 100 kontrol, aktivní log stream do 2 klientů), po ustálení přečte `/proc/<pid>/smaps_rollup` a selže nad **100 MB PSS** (varování nad 80 MB). Naměřená hodnota se zapíše do souhrnu jobu a k release notes. Měřicí skript v `scripts/dev/pss-gate.sh`, zátěž v `internal/...` jako `go test -tags pssgate`.
- **[A] Port map a kontrola kolize portů (Vlna B; Port map převzata z M15):**
  - Nový endpoint `GET /api/hosts/{host}/ports` a v UI karta/modal **Ports** na hostu. Jeden sdílený sběr dat (`engine/ports.go`) vrací publikované porty všech běžících i zastavených kontejnerů (stacky i standalone, s `HostIP`), porty deklarované v `compose.yaml` spravovaných stacků (i zastavených) a nativní porty hosta z `/proc/net/*` (jen v host-network / nativním režimu). Každá položka nese zdroj (`stack`, `container`, `compose`, `host`), protokol, bind IP a stav. Data se čtou na vyžádání, bez smyčky na pozadí (princip M15).
  - Stejný sběr používá linter (`port-taken`), takže přehled a varování nikdy neukazují rozdílné výsledky.
  - V editoru `compose.yaml` i při vytváření stacku (`NewStack`) se kolize (`host_ip:port/proto`) ukáže ještě před `up`.
  - V `internal/engine/containers.go` (`toContainer()`) přestat zahazovat `p.IP` z Docker `PortSummary` (zachovat bind IP např. `127.0.0.1:8080:80/tcp`).
  - V `internal/api/lint.go` (`runningPorts`) zahrnout i standalone kontejnery (ne jen compose stacky) a při běhu mimo bridge / s host netns načíst i nativní porty z `/proc/net/{tcp,tcp6,udp,udp6}`.
  - Spouštět `port-taken` kontrolu už při živé validaci v `ComposeEditor.svelte` (`deploy=false`), nejen při kliknutí na Deploy.
- **[A] Multi-IP adresy hosta a získávání URL (Vlna B):**
  - Rozšířit model sítě hosta o seznam adres `{id, label, kind: ethernet|wifi|vpn|domain|custom, address, isDefault, autoDetected}` při plném zachování zpětné kompatibility s `MOORAGE_HOST_IP` a `host_ip:<hostID>`.
  - Při automatické detekci filtrovat loopback (`127.0.0.0/8`, `::1`), link-local i virtuální Docker rozhraní (`docker0`, `br-*`, `veth*`) a klasifikovat `eth*/en*` (Ethernet), `wl*` (Wi-Fi), `tailscale*/wg*/wt*/tun*` (VPN).
  - V `hostPortURLs` (`internal/api/settings.go`) generovat primární URL s výchozí adresou hosta a přikládat alternativní URL pro ostatní adresy hosta (UI nabídne výchozí odkaz na klik a rozbalovací/popover seznam ostatních adres). Zachování `HostIP` v portech zároveň zprovozní přeskočení `127.0.0.1` portů.
- **[A] Bohatá oznámení (Vlna B):**
  - **Strukturovaná zpráva:** `notify.Message` dostane strukturované části vedle dnešních `Title`/`Text`: `Event` (checks / auto_update / rollback / deploy_failed / agents / watchdog / test), `Sender {Host, Role: hub|agent|watchdog}`, `Stack`, `Host`, `Cause`, `Duration`, `Since`, `Logs`, `Image {From, To}`, `CheckDetails` (kód, latence, hlasy agentů) a `URL`. Skládání zpráv v `monitor.compose`, `updater`, `tunnel/watchdog` a `api/events` je plní; `Text` zůstává pro prostý text a zpětnou kompatibilitu.
  - **Předvolby (`Config.Preset`, `Config.Include`):**
    - **Minimal:** stav, název, host, odkaz. Vhodné pro Pushover / telefon.
    - **Standard (výchozí, odpovídá dnešnímu obsahu):** + stack, příčina, trvání, posledních 20 řádků logů.
    - **Detailed:** + změna image (digest / tag), detaily kontroly (HTTP kód, latence, hlasy agentů), čas začátku a konce.
    - **Custom:** přepínače `host`, `stack`, `cause`, `duration`, `logs`, `image`, `checkDetails`, `link`, `timestamps`. Stav a název jsou vždy zapnuté.
    - Existující kanály dostanou `Standard`, takže se jim obsah nezmění.
  - **Formát (`Config.Format: rich | plain`, výchozí `rich`)** a co který kanál umí:

    | Kanál | Embed / formátování | Jméno odesílatele | Ikona |
    |---|---|---|---|
    | Discord | `embeds` (barva podle stavu, pole, patička, timestamp, logy v code blocku) | `username` | `avatar_url` |
    | Slack | `attachments` s poli a barvou (Block Kit) | jen legacy webhooky (nové Slack apps přepsání ignorují) | jen legacy webhooky |
    | Telegram | `parse_mode: HTML` s escapováním, tlačítko s odkazem | ne (jméno bota); host tučně na prvním řádku | ne |
    | Matrix | `formatted_body` (escapované HTML) + `body` | ne (účet bota); host na prvním řádku | ne |
    | Gotify | Markdown (`client::display`), `client::notification.click.url` | ne (jméno aplikace) | ne |
    | ntfy | `Markdown: yes`, `Click`, akce „Open“ | v titulku | `Icon` |
    | Pushover | `html=1` s escapováním, `url_title` | v titulku | ne |
    | E-mail | `multipart/alternative` text + escapované HTML | zobrazované jméno v `From` (`nas • Moorage agent <alerts@…>`) | ne |
    | Webhook (generic) | JSON se strukturovanými poli | pole `sender` | pole `sender.iconUrl` |

    Generický webhook jen **přidává** pole (`sender`, `preset`, `fields`, `event`), stávající klíče zůstávají kvůli existujícím automatizacím.
  - **Logo Moorage:** PNG 256×256 ve třech variantách (`hub`, `agent`, `watchdog`) v repozitáři Moorage (`docs/assets/notify/`). Výchozí URL míří na veřejný GitHub raw / jsDelivr; v **Settings → Notifications** jde přepsat vlastní základní URL (např. vlastní hosting nebo po rebrandingu). Discord neumí SVG, proto PNG.
  - **Bezpečnost obsahu:** všechen text z kontejnerů, logů a labelů se escapuje podle cílového formátu (Discord Markdown, HTML pro Telegram / Matrix / Pushover / e-mail, Markdown pro Gotify / ntfy). `allowed_mentions` v Discordu zůstává prázdné, takže logy nikdy nikoho neoznačí. Uživatelské šablony se nezavádějí (zachovává rozhodnutí v `notify.go`).
  - **Limity:** ořez podle limitů služby (Discord: popis 4096, pole 1024, celkem 6000 znaků, max. 25 polí; `username` max. 80 znaků a nesmí obsahovat „discord“ ani „clyde“, proto se jméno hosta sanitizuje; Telegram 4096; Slack pole 2000).
  - **UI (`NotificationsView.svelte`):** u kanálu výběr předvolby (segmentový přepínač), při Custom přepínače částí, přepínač Rich / Plain (jen u kanálů, které embed umí) a **náhled** zprávy pro daný kanál. „Send test“ pošle testovací zprávu ve zvoleném formátu a předvolbě jako `<hub> • Moorage hub`.

---

### Okrajové stavy
- **Bridge kontejner vs. host rozhraní a porty:** Pokud Moorage/agent běží v izolovaném Docker bridge kontejneru, UI u automatické detekce adres a nativních `/proc/net` portů jasně uvede, že v bridge režimu vidí jen Docker porty a ručně zadané adresy (s odkazem na `network_mode: host` jako volitelnou možnost).
- **Chybějící práva nebo Read-only `/data`:** Pokud uživatel namountuje `/data/templates` jako `:ro`, startup nezhavaruje na `os.MkdirAll` pokud složka už existuje (a při reálné chybě zápisu vypíše jasnou diagnostiku).
- **Multi-arch `:latest` a vlastní registry:** Pokud soukromý registr nevrací platformní digesty nebo má rate-limit, `hasMatchingDigest` nesmí falešně hlásit chybu ani spadnout — zachová stávající bezpečný fallback.
- **Zero-config sidecar bez přístupu ke stacks složce:** Pokud `restic-backup` dostane výchozí cestu ze `MOORAGE_STACKS_DIR`, na agentovi v kontejneru může být cesta jiná na hostu vs. uvnitř kontejneru — `installAddon` předvyplní hostovskou cestu z mountu nebo bezpečný named volume pro data záloh.
- **Změna výchozí adresy hosta:** Změna výchozí adresy v `HostsView` okamžitě přepočítá primární URL stacků bez zásahu do `compose.yaml` (explicitní `moorage.url` labely a proxy URL mají vždy přednost podle stávajících pravidel).
- **Port map – host network:** kontejnery s `network_mode: host` nemají mapování `Ports`; zobrazí se s poznámkou `host network` (a jejich porty jsou vidět jen přes `/proc/net` v host-network režimu Moorage).
- **Port map – zastavený stack:** porty z `compose.yaml` zastaveného stacku se ukazují jako „reserved (stopped)“, linter u nich hlásí varování, ne chybu.
- **Terminálový tiket:** vypršelý nebo už použitý tiket → upgrade odmítnut s `401`; `TerminalPanel` si automaticky vyžádá nový jednou, při druhém selhání ukáže důvod. Otevření terminálu ve dvou panelech = dva různé tikety.
- **Revokace během práce v terminálu:** spojení se zavře, výstup zůstane na obrazovce, panel ukáže „Session ended“ a nabídne přihlášení.
- **PSS gate na CI runneru bez Dockeru / jiném jádře:** job se spouští jen na Linux runneru s Dockerem; když měření selže technicky (ne překročením), job skončí chybou s jasnou hláškou, nepovažuje se za „prošlo“.
- **Oznámení o více hostech najednou** (seskupené „DOWN: 3 checks“ přes různé hosty, výpadek více agentů): odesílatel je `<hub> • Moorage hub`, hosté jsou v polích embedu.
- **Watchdog z agenta:** agent nemá nastavení předvoleb z hubu, pokud hub nikdy nebyl dostupný. Předvolba a formát kanálu se replikují do `watchdog.json` spolu s konfigurací kanálu; starý `watchdog.json` bez nich = `Standard` + `rich`.
- **Logo nedostupné** (soukromý repozitář, výpadek CDN, vlastní URL špatně): Discord zobrazí výchozí avatar, zpráva se doručí. Moorage URL loga neověřuje při každém odeslání.
- **Kanál neumí rich formát** (Pushover bez HTML, starý Gotify klient): přepínač Rich / Plain se u něj nezobrazí nebo se automaticky použije prostý text.
- **Embed přesáhne limit** (hodně skupinových alertů, dlouhé logy): nejdřív se zkracují logy, pak počet polí („…and 12 more“), zpráva se nikdy neodmítne kvůli délce.

---

### Databázové migrace, API a pořadí vydání

1. **Vlna A (Opravy, `/data`, terminál, image updater, párování hostů, Host CLI):**
   - **DB migrace:** Žádná nebo minimální aditivní úprava `join_tokens` (stav/tolerance opakovaného doručení certifikátu při ztrátě odpovědi).
   - **Backend / Docker / Skripty:** `config.go`, `templates/service.go`, `api/terminal.go` (+ `POST /api/terminal/ticket`), `api/ws.go` (registr živých spojení), `api/users.go` / `api/api.go` (revokace → zavření spojení), `engine/host_terminal.go`, `selfupdate/container.go`, `engine/images.go`, `tunnel/agent.go`, `tunnel/hub.go`, `store/agents.go`, `Dockerfile`, `scripts/install-cli.sh`, `scripts/dev/pss-gate.sh`, `.github/workflows/ci.yml` (`{@html}` pojistka, PSS gate).
   - **Frontend & Docs:** `MultiSelect.svelte`, `LogViewer.svelte`, `StatusCodes.svelte`, `checks.ts`, `CheckForm.svelte`, `DepsView.svelte`, `TemplatesView.svelte`, `TerminalPanel.svelte`, `HostsView.svelte`, `StackDetail.svelte`, `AccountView.svelte`, `docs/cli.md`, `README.md`.
   - **Ověření A:** `go test ./...` (včetně testů tiketu, revokace a hlaviček cookies/CSP/Origin), `svelte-check`, `vite build`, zelený PSS gate, kontrola v prohlížeči a manuální ověření párování, terminálu (i za reverse proxy), `Logs` dropdownu a přidání checku.
2. **Vlna B (Add-ons, Linter portů, Multi-IP URL):**
   - **DB migrace (aditivní):** Rozšíření nastavení sítě hosta v `app_settings` (`host_addresses:<hostID>` JSON se zpětným fallbackem na `host_ip:<hostID>`), případně pomocný flag pro první inicializaci `add-ons` stacku s `policy=auto`.
   - **Oznámení:** bez DB migrace – `preset`, `include` a `format` jsou nová volitelná pole v existujícím JSON konfiguraci kanálu (chybějící = `standard` + `rich`).
   - **Backend & `moorage-addons`:** `api/addons.go`, `lint/lint.go`, `api/lint.go`, `engine/containers.go`, `engine/ports.go` (sdílený sběr portů) + `GET /api/hosts/{host}/ports`, `engine/hostnet.go` (`net.Interfaces` + `/proc/net` čtečka), `api/settings.go`, `notify/notify.go` (+ rozdělení na `notify/render_*.go` podle kanálu), `monitor/monitor.go`, `updater/updater.go`, `tunnel/watchdog.go`, `api/events.go`, `docs/assets/notify/*.png`, aktualizace `moorage-addons/*` (`restic-backup` → `/data/backups`).
   - **Frontend:** `AddonsView.svelte`, `ComposeEditor.svelte`, `HostsView.svelte` (+ karta **Ports**), `NewStack`, `Home.svelte`, `StackDetail.svelte`, `CommandPalette.svelte`, `NotificationsView.svelte` (předvolby, formát, náhled).
   - **Ověření B:** Unit testy pro multi-arch `hasMatchingDigest`, `port-taken` (s IP overlapem i `/proc/net` parserem), sdílený sběr portů, `net.Interfaces` klasifikaci a `hostPortURLs`; golden testy JSON / HTML výstupu každého kanálu pro všechny 4 předvolby, escapování (log s `@everyone`, `<script>`, Markdown znaky) a ořez limitů; `go test ./...`, `svelte-check`, `vite build`; ruční test do skutečného Discordu, Telegramu a ntfy.

---

### Mimo rozsah (odloženo do `ROADMAP.md`)
- Automatické zakládání S3/B2/WebDAV vzdálených repozitářů mimo lokální zero-config defaulty v `restic-backup` (zůstává jako volitelná konfigurace).
- Rozsáhlé přepínání bind portů existujících stacků na VPN rozhraní na jeden klik (řešeno samostatně v M15 `vpn-expose`).
- Ikony jednotlivých služeb v oznámeních (rozhodnuto: jen logo Moorage).
- Uživatelské šablony textu oznámení (pevné texty v kódu zůstávají kvůli bezpečnosti).
- Označování rolí / uživatelů (`@role`) v Discordu při DOWN → `ROADMAP.md`.
- Strategické směry z analýzy konkurence (Rollback & Verify, statický status, importéry, Passkeys/SBOM, transport) → **M18**.

### Otevřené otázky
- Žádné blokující otázky pro **Vlnu A** ani **Vlnu B**.

---

## M18 – Incidenty s Rollback & Verify, důvěra, migrace z konkurence a odolnost status stránky (návrh 2026-11-26, čeká na detailní návrh)

**Cíl:** Proměnit to, že Moorage vlastní řízení stacků, logy, aktualizace i externí sondy v jednom uzlu, ve funkce, které poskládaná sada nástrojů (Arcane / Dockhand + Dozzle + Uptime Kuma) neumí. K tomu doplnit pilíře důvěry pro veřejné vydání a usnadnit přechod z Portaineru, Dockge a Uptime Kumy.

> Zdroj: analýza konkurence z listopadu 2026 (`Analýza Aplikace Moorage a Konkurence.md`), ověřená proti současnému kódu. Body, které už Moorage má (SSRF ochrana `netguard`, oddělený `MOORAGE_STATUS_LISTEN`, MCP Server add-on, WebSocket transport hub–agent, watchdog hubu, auto-rollback, Logbook, sondy z více míst), se sem nepřebírají. Každá vlna před implementací projde vlastním feature designem (kolize, okrajové stavy, rozhodnutí).

### Navrhované vlny
1. **M18A – Kauzální časová osa incidentu a Rollback & Verify**
   - Incident jako objekt: při DOWN kontroly se automaticky sestaví časová osa z existujících zdrojů v okně např. 15 minut zpět – změna `compose.yaml` (commit v `history.git`), auto-update image (digest před / po), Docker eventy (restart, `OOMKilled` / exit 137, unhealthy), shody log alertů a selhání sond (včetně hlasů agentů z R6).
   - Zobrazení v detailu incidentu a v Logbooku stacku; v oznámení (Detailed předvolba z M17B) zkrácená osa „co se změnilo před výpadkem“.
   - **Rollback & Verify:** jedno tlačítko (nebo politika u stacku) vrátí předchozí digest image nebo revizi `compose.yaml`, sleduje healthcheck, logy a externí sondu a incident uzavře, až sonda znovu vrátí úspěch. Při neúspěchu verifikace se nic dalšího automaticky nedělá a přijde eskalace. Staví na auto-rollbacku z M14 a `history.git` z M2.
   - Uzavření incidentu se propíše i na veřejnou status stránku.
   - **Stav (2026-11-26, necommitováno):** implementováno (první verze z opencode, opravená a přepsaná po kontrole). Časová osa se skládá přes registr hostů (funguje pro agenty i hub bez lokálního Dockeru), s timeoutem 5 s, a její zkrácená verze jde do předvolby **Detailed** („Changed before“). Rollback & Verify vrací **verzi před poslední změnou** (odmítne, když se stack od začátku výpadku změnil), ověření je **výsledek kontroly z monitoru** (monitor sám zavře incident, pošle UP a aktualizuje status stránku; Moorage ho jen nechá hned sondovat) + běžící a zdravé kontejnery stacku do 10 min. Při neúspěchu přijde alert „STILL DOWN after Rollback & Verify“ a záznam v auditu; rollback přerušený restartem se po startu označí jako neúspěšný. **Odchylky:** záznamník událostí kontejnerů zatím sleduje změny seznamu kontejnerů stacků s kontrolou každých 30 s (funguje pro všechny verze agentů; OOM se pozná jen jako exit 137) místo streamu Docker events; politika Rollback & Verify u stacku (automatické spuštění) zatím chybí. Ruční ověření v prohlížeči na reálném stacku zbývá.
2. **M18B – Pilíře důvěry pro veřejné vydání**
   - **Passkeys (WebAuthn)** jako druhý faktor vedle TOTP, volitelně i přihlášení bez hesla.
   - **SBOM** (SPDX / CycloneDX přes `syft`) ke každému release a Docker image, **podpis image přes cosign** (keyless přes GitHub OIDC) a attestace; doplňuje stávající Ed25519 podpis binárek z M12.
   - **Veřejný threat model** (`docs/security/threat-model.md`) a `SECURITY.md`: Docker socket, CSWSH, SSRF, path traversal, status stránka, agenti, MCP; s odkazy na testy, které jednotlivé ochrany hlídají.
   - **Stav (2026-11-26, necommitováno):** implementováno. **Passkeys** přes `github.com/go-webauthn/webauthn` v0.18.2 (tabulka `webauthn_credentials`, migrace 32): passkey je druhý faktor **vedle** TOTP, nikdy místo něj – TOTP zůstává povinné, takže pravidlo break-glass adminů, reset link i „ztratil jsem passkey“ fungují beze změny a admin nemůže přijít o přístup potichu. Přihlášení: heslo + passkey místo kódu (`POST /api/login` s `passkey`), nebo **bez hesla** jen u passkey, kterou si uživatel výslovně označí jako passwordless, a jen když autentikátor ověřil uživatele (PIN / biometrie, UV flag). Přidání passkey chce heslo **a** aktuální TOTP kód (ukradená session nepřidá trvalý faktor), odebrání jen session. Výzvy jsou discoverable (neprozradí účty ani passkeys), drží se jen v paměti 5 min a jdou použít jednou; návrat sign counteru (klon) přihlášení odmítne. Dokončený **reset link** passkeys uživatele smaže. RP ID / origin se odvozuje **jen z nastavené veřejné URL** (nikdy z hlavičky `Host`): musí být nastavená, `https://` (nebo `http://` jen pro `localhost` / `*.localhost`), doménové jméno (ne IP); RP ID = hostname bez portu, jediný povolený origin = schéma + host + port; bez veřejné URL jsou passkeys vypnuté a UI řekne proč. Změna hostu veřejné URL passkeys zneplatní (Account je označí, TOTP jede dál). Audit `account.passkey_add` / `_save` / `_remove`, `auth.login` / `auth.login_failed` s cílem `passkey` / `passwordless passkey`; seznam uživatelů ukazuje počet passkeys. **Release:** nový job `attest` (OIDC token, nikdy Ed25519 klíč) – Syft SBOM SPDX + CycloneDX pro binárky (jeden pro všechny architektury) a pro každou platformu image, `cosign sign --recursive` keyless, `cosign attest` SBOM na image, `actions/attest-build-provenance` pro image a binárky, `actions/attest-sbom` pro binárky; SBOM soubory jdou do GitHub Release, release notes obsahují příkazy `cosign verify` / `gh attestation verify`, `publish` (a posun `:latest`) čeká i na `attest`. Ed25519 podpis `SHA256SUMS` zůstal beze změny (SBOM do něj nepřidávám, aby se nezměnilo, co ověřuje self-updater). Akce připnuté na commit SHA. **Dokumentace:** `docs/security/threat-model.md` (každá ochrana s odkazem na existující testy) a `SECURITY.md` (hlášení přes GitHub private vulnerability reporting), odkazy v README. **Odchylky / mimo rozsah:** samostatné recovery kódy nevznikly (záchranou zůstává TOTP + admin reset link); passkeys jen pro lokální účty (OIDC / proxy mají faktory u IdP); přejmenování passkey v UI chybí (API ho umí); pravidla pracovních složek externích stacků a add-on socket proxy nemají v threat modelu vlastní test (uvedeno). Workflow nešlo spustit, ověřeno jen parsováním YAML; passkeys ověřeny Go testy se softwarovým autentikátorem, **ruční test v prohlížeči se skutečným autentikátorem zbývá**. Mimochodem opraven flaky test `TestSystemUpdatesRBACGatesAuditAndReconcile` na Windows (hrubý tik hodin vs. `StatusTTL = 1 ns`).
3. **M18C – Migrace z konkurence a demo**
   - **Import z Portaineru CE:** přes Portainer API (URL + token) stáhnout stacky a uložit je jako `compose.yaml` + `.env` do `StacksDir`, s náhledem a výběrem.
   - **Dockge:** ověřit a zdokumentovat převzetí existující složky `/opt/stacks` beze změny souborů (pravděpodobně už funguje přes `MOORAGE_MANAGE_EXTERNAL` / nastavení `StacksDir`); doplnit průvodce.
   - **Import monitorů a status stránek z Uptime Kuma (JSON záloha) a Gatus (YAML)** do kontrol, kanálů a status stránek, s reportem, co nešlo převést.
   - **Veřejná demo instance** v read-only demo režimu s ukázkovými daty.
   - **Stav (2026-11-26, necommitováno):** implementováno bez detailního feature designu, rozhodnutí konzervativně. Vše je v **Settings → Import** (jen admin), každý import má náhled a **nic se nenasazuje ani nespouští**. **Portainer CE:** nový balík `internal/migrate` – klient jen pro čtení (`GET /api/stacks`, `/api/stacks/{id}/file`, `/api/endpoints?excludeSnapshots=true`, hlavička `X-API-Key`) přes `netguard` (LAN povolená, link-local / metadata ne), **bez sledování přesměrování** (token by odešel jinam), limity velikosti odpovědí, volitelně „Accept a self-signed certificate“. Token jde jen v těle požadavku náhledu / importu, nikde se neukládá, neloguje, není v chybách ani v auditu (audit `import.portainer` s cílem „N stacks on <host>: …“). Náhled: Swarm, Kubernetes, Git stack bez souboru, prázdný nebo > 512 KB soubor = *Not supported*; kolize jména se spravovaným stackem nebo uvnitř seznamu = *Name taken* (admin zadá jiné jméno); compose neprojde validací / blokujícím pravidlem linteru = *Needs attention*. Jména se převádí na platná jména projektu (`My App` → `my-app`). Uložení přes sdílenou save cestu hostu (`CreateNoted`, jedna verze „Imported from Portainer stack …“), proměnné stacku z Portaineru → `.env`. Varování (ne blokace) u `build:`, `env_file:` (zvlášť `stack.env`), relativních bind mountů, Git stacků a běžícího projektu stejného jména („Stop the stack in Portainer before you deploy it from Moorage“). **Dockge:** ověřeno, že převzetí `/opt/stacks` funguje dnes beze změny kódu – stejná struktura složek, jen mount na **stejnou cestu** a `MOORAGE_STACKS_DIR` (jiná složka přes `MOORAGE_EXTRA_STACKS_DIRS` + `MOORAGE_MANAGE_EXTERNAL`, jen namountovaná, sandbox se neobchází); nový test `TestTakeOverDockgeFolder` hlídá, že start a historie nezmění ve složce ani bajt ani čas změny. Dockge `global.env` a `compose.override.yml` Moorage nečte (zdokumentováno). Průvodce `docs/migration.md`, karta „Coming from Dockge?“ v Settings → Import. **Uptime Kuma (JSON záloha 1.x) a Gatus (YAML):** převod na kontroly HTTP/keyword/JSON (jen prostá cesta), TCP, ping, DNS a na kanály Discord, Slack, Telegram, ntfy (token), Gotify, Pushover, Matrix, SMTP, webhook (Gatus `custom`); z Gatusu navíc jedna **nepublikovaná** status stránka (slug `gatus`, sekce podle skupin). Co nejde (typy monitorů, hlavičky / tělo / autentizace, jiné metody než GET/HEAD, upside-down, poskytovatelé, `${VAR}`, podmínky RESPONSE_TIME / CERTIFICATE_EXPIRATION…), je v reportu s důvodem; status stránky Kuma záloha neobsahuje (řečeno v náhledu). Upload max 5 MB, striktní parsování (JSON jeden dokument s kontrolou typů, YAML jeden dokument, duplicitní klíče = chyba, limity 500 kontrol / 100 kanálů), soubor se neukládá – prohlížeč ho pošle znovu při importu. Každá položka prochází stejnou validací jako formuláře (`validateCheck`, `notify.Validate`, `validateStatusPage`), tajemství jdou jen do konfigurace kanálu (náhled je nevrací). **Opakovaný import je idempotentní:** kontroly a kanály se párují podle jména, stránky podle slugu; existující se nikdy nemění ani neduplikují (existující kanál stejného typu se znovu použije pro nové kontroly, stránka odkazuje na existující kontroly stejného jména); běh, který nic nevytvoří, vrátí 422 a do auditu se nezapíše (`import.monitors`). Volba „Create all checks paused“ proti dvojím alertům při souběhu se starým nástrojem. **Demo:** `MOORAGE_DEMO=true` – falešný host `demo-host` s ukázkovými stacky, kontejnery, logy, statistikami a historií (Docker socket se vůbec neotevře), každý `POST/PUT/PATCH/DELETE` (API, CLI socket, MCP, webhooky) odmítne 403 s jasnou zprávou ještě před handlery, návštěvník je bez přihlášení uživatel `demo` s rolí admin (vše vidí, nic nezmění), banner v UI, skrytý Account a Sign out; demo zapne i `MOORAGE_READ_ONLY`, vypne port agentů, terminály a self-update, jako agent nenastartuje. Ukázková data (kontroly stacků čtou falešného hosta – žádné sondy ven, vypnutá HTTP kontrola, vypnutý ntfy kanál na `ntfy.invalid`, publikovaná stránka `/status/demo`) se vytvoří jen do prázdné DB. Návod `docs/demo.md`, nic veřejného nasazeno nebylo. **Odchylky / mimo rozsah:** z Portaineru se nepřenáší soubory vedle compose (build kontexty, `env_file`, data relativních bind mountů – jen varování), Git napojení ani Swarm; Kuma 2.x už JSON zálohu nemá; monitory Kuma typu docker se nepřevádí na kontrolu stacku (nejde spolehlivě spárovat kontejner se stackem); Gatus `overrides` a `default-alert` kromě prahu se nepřenáší. Ruční test proti skutečnému Portaineru, Kumě a Gatusu a vizuální kontrola nové stránky v prohlížeči zbývá (Portainer klient ověřen httptest serverem, převodníky fixture soubory).
4. **M18D – Odolnost status stránky a transportu**
   - **Statický snapshot status stránky** (add-on): po každé změně stavu a periodicky publikuje HTML + JSON na S3 / R2, WebDAV nebo GitHub Pages, takže veřejná stránka přežije úplný výpadek serveru. Snapshot nese čas poslední aktualizace a při zastarání sám ukáže „data may be out of date“.
   - **Watchdog jako externí sonda:** ověřit, co z „Mutual-Watch“ už pokrývá watchdog (M6) a sondy z více míst (R6); doplnit jen chybějící (např. DNS a latence měřené z agenta mimo domácí síť).
   - **Oddělení logů od řídicího provozu v tunelu hub–agent:** nejdřív změřit head-of-line blocking při záplavě logů; pokud se potvrdí, priorita nebo samostatná yamux relace pro bulk logy.
   **Stav (2026-11-26, necommitováno):** Hotovo. (1) Vestavěný add-on `status-snapshot` (`internal/snapshot`, `internal/status/snapshot.go`, `internal/api/addon_snapshot.go`, panel `StatusSnapshotPanel.svelte`): S3-kompatibilní úložiště (SigV4), WebDAV a GitHub Pages přes Git Data API (jeden commit na publikaci); publikuje se s debounce po změně stavu kontroly, periodicky (výchozí 5 min, GitHub min. 15 min) a ručně, s backoffem při chybě. Každá stránka má `index.html` + `status.json`, čas poslední aktualizace a sama v prohlížeči ukáže „data may be out of date“ po nastavené době. Úložiště se nikdy nevytváří, tlačítko Test jen ověří zápis. Tajné údaje se nevracejí ani nelogují, akce jsou v auditu, v demo režimu add-on nejde zapnout. (2) Watchdog/Mutual-Watch: M6 + sondy z více míst (R6) už pokrývají dostupnost z agenta mimo domácí síť včetně DNS (druh `dns` běží i na agentovi); doplněn jen chybějící **limit latence** (`maxLatencyMs` u http/tcp/ping/dns, každé místo ho vyhodnotí na své odpovědi, hub ho uplatní i pro starší agenty). (3) Head-of-line blocking změřen (`internal/tunnel/hol_test.go`, simulovaná linka 16 Mbit/s): běžný yamux p50 ~423 ms latence řídicího požadavku během záplavy logů → potvrzeno. Oprava bez nové relace a bez změny protokolu: zápisy každého streamu se dělí na rámce ≤ 16 KiB (`bounded.go`), takže se proudy prokládají; p50 během záplavy ~6 ms. Kompatibilní se staršími agenty i huby. Neověřeno proti skutečnému S3/R2/WebDAV/GitHubu (jen testovací servery v testech).
5. **M18E – Generované hodnoty, volné porty a „co služba potřebuje“ (návrh 2026-11-26)**

   **Cíl:** Při psaní `compose.yaml` / `.env` stačí napsat klíčové slovo a Moorage při uložení vygeneruje heslo, klíč, uživatele nebo volný port a zapíše ho do `.env` jako obyčejnou proměnnou. Linter navíc podle image napoví, co služba potřebuje (povinné proměnné, trvalé volumes, porty, healthcheck), a nabídne opravu na klik.

   **Hlavní tok:** Uživatel do compose napíše `POSTGRES_PASSWORD: "@generate(password, 32)"` a do druhé služby `DB_PASS: ${POSTGRES_PASSWORD}`, k portům `"${SERVICE_PORT_WEB:-8080}:80"` → klikne **Save**. V náhledu změn vidí „Generated: `POSTGRES_PASSWORD` (password, 32) · `SERVICE_PORT_WEB` = 8081 (8080 is taken by stack *jellyfin*)“ → potvrdí. Compose teď obsahuje `POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}`, `.env` skutečné hodnoty, vše je jedna git verze a stack jde spustit i obyčejným `docker compose`.

   **Rozhodnutí uživatele `[U]`:**
   - **Tři zápisy klíčových slov:**
     1. **Coolify jména** v compose: `${SERVICE_PASSWORD_<NAME>}`, `${SERVICE_USER_<NAME>}`, `${SERVICE_BASE64_<NAME>}` a jejich délkové varianty (přesný podporovaný seznam se při implementaci ověří proti dokumentaci Coolify a zapíše do `docs/generated-values.md`). Díky tomu fungují i šablony služeb z Coolify.
     2. **Vlastní zápis v `.env`:** `KEY=@generate(<kind>[, <param>])`.
     3. **Vlastní zápis v compose u proměnné prostředí:** `KEY: "@generate(...)"` (mapa) nebo `- KEY=@generate(...)` (seznam). Při uložení se hodnota vygeneruje do `.env` a v compose se nahradí `${KEY}` – stejný mechanismus jako stávající oprava „Move values to .env“ (`lint.MoveEnv`).
   - **Volný port:** klíčové slovo (`${SERVICE_PORT_<NAME>:-<preferovaný>}` v compose, `KEY=@generate(port, 8080)` v `.env`, `"@generate(port, 8080):80"` přímo v `ports:`) **i** rychlé opravy v editoru: akce **Add port** (nabídne porty z `EXPOSE` image a volný host port) a u varování `port-taken` oprava „Use free port 8081“.
   - **Co služba potřebuje:** obecně z **metadat image** (`EXPOSE`, `VOLUME`, `HEALTHCHECK`, `USER`) **plus katalog „receptů“** pro populární image v repozitáři `moorage-templates`, obnovovaný stejně jako katalog šablon (add-on `official-templates`).

   **Architektonická rozhodnutí `[A]`:**
   - **Generuje se jednou, při uložení**, ve sdílené cestě pro ukládání stacku (UI editor, `NewStack`, šablony, CLI/API, MCP `create_stack`/`update_stack`). Hodnota vznikne jen tehdy, když klíč v `.env` chybí nebo obsahuje ještě klíčové slovo. Existující neprázdná hodnota se **nikdy nepřepíše** a deploy nic negeneruje (změna hesla by rozbila databázi).
   - **Jeden generátor pro všechno:** rozšířit `templates.Generate` (crypto/rand, bez modulo biasu) a použít ho pro šablony (`generate:` v `template.yaml` zůstává), MCP `generateSecrets` i klíčová slova. Druhy: `password[, délka][, symbols]` (výchozí 32 znaků `[A-Za-z0-9]`, protože speciální znaky rozbíjí řadu služeb a URL), `alnum`, `hex[, bajty]`, `base64[, bajty]`, `base64key` (s prefixem `base64:` pro Laravel `APP_KEY`), `uuid`, `user` (náhodné `u_xxxxxxxx`), `port[, preferovaný]`. Délky omezené (8–256), neznámý druh = chyba linteru.
   - **Stejný klíč = stejná hodnota:** `@generate` se stejným jménem proměnné ve více službách vytvoří jednu proměnnou (sémantika `MoveEnv`); sdílení mezi službami jinak přes `${KEY}`.
   - **Volné porty:** výběr nejbližšího volného portu od preferovaného podle sdíleného sběru portů z M17B (`engine/ports.go`: běžící kontejnery, porty deklarované ve všech spravovaných stacích včetně zastavených, nativní porty hosta, kde jsou vidět), vynechá porty < 1024, pokud preferovaný port sám není < 1024. Výběr probíhá pod zámkem pro daný host, aby dvě současná uložení nedostala stejný port; uložený port se díky Port map hned počítá jako obsazený.
   - **`SERVICE_URL_*` / `SERVICE_FQDN_*` (Coolify):** doplní se z výchozí adresy hosta (M17B) + portu, nebo z proxy URL, pokud ji Moorage zná. Uloží se jednou jako ostatní hodnoty; pozdější změna výchozí adresy je nepřepíše (linter jen upozorní na nesoulad).
   - **Metadata image:** z lokálně staženého image, jinak z config blobu v registru přes stávající klienta s `netguard` (bez stahování vrstev); výsledek se cachuje podle digestu. Nové informační nálezy linteru s rychlými opravami: „Image exposes 5432/tcp – add port?“, „Image declares volume `/var/lib/postgresql/data` but the service has no volume: data is lost on recreate“ (varování), „Image has a healthcheck“ / návrh healthchecku.
   - **Katalog receptů:** jen data (YAML), nic se nevyhodnocuje ani nespouští, stejný princip jako šablony. Recept = vzor jména image (např. `postgres`, `*/postgres`, `lscr.io/linuxserver/*`) + povinné proměnné s generátorem nebo výchozí hodnotou (`POSTGRES_PASSWORD: password`, `PUID: 1000`, `TZ: <host TZ>`), doporučené volumes, porty, healthcheck a krátká poznámka. Nálezy z receptu mají vždy opravu na klik, která vloží klíčová slova `@generate`, takže se recept a generátory nikdy nerozcházejí. Validace jako u `template.yaml` (neznámé klíče = chyba, limity velikosti).
   - **Stacky spravované Coolify** (labely `coolify.*`) se nematerializují: Coolify si `SERVICE_*` generuje sám a Moorage by mu přepsala `.env`.

   **Okrajové stavy:**
   - **Read-only agent / externí stack na Level 1 / vlastní stack Moorage:** nic se negeneruje; linter ukáže, že klíčová slova zůstala nevyřešená, a proč.
   - **Stack upravený mimo Moorage (souborový editor, git, ruční úprava):** klíčové slovo zůstane v souboru; při příštím deployi přes Moorage se nejdřív materializuje (s vlastní git verzí) a teprve pak `up`. Ruční `docker compose up` by dostal doslovný text `@generate(...)` nebo prázdnou proměnnou – proto linter chyba a varování v **Diagnostics**.
   - **Klíčové slovo jinde než u proměnné** (např. v `command:`, `labels:`): chyba linteru „`@generate` works only in environment values and `ports:`“.
   - **Žádný volný port v rozsahu** (preferovaný až +1000): uložení se neprovede, chyba s odkazem na kartu **Ports**.
   - **Registr nedostupný nebo private bez přihlášení:** analýza metadat se tiše přeskočí (info „Image metadata unavailable“), generování a porty fungují dál.
   - **Recept vs. vlastní nastavení uživatele:** když služba proměnnou už má (jakoukoli hodnotou), recept ji nenavrhuje. Nálezy z receptů jde u stacku ignorovat stejně jako ostatní nálezy linteru.
   - **Tajemství v historii:** vygenerované hodnoty jsou v `.env`, a tedy v `history.git`, stejně jako dnes ručně zadaná hesla (šifrování `.env` v historii zůstává v `ROADMAP.md`). Náhled změn ukáže jen jména a druhy vygenerovaných hodnot, ne hodnoty.

   **Data a API:** bez DB migrace (cache metadat image v paměti / malá tabulka podle digestu, rozhodne se při implementaci). Nové `lint` pravidla a opravy, rozšířený `templates.Generate`, materializace ve sdílené save cestě (`api/stacks`), endpoint pro recepty jako součást katalogu šablon. Závisí na M17B (Port map, adresy hosta).
   **Stav (2026-11-26, necommitováno):** Hotovo jádro. Sdílený generátor `internal/gen` (crypto/rand, druhy password/alnum/hex/base64/base64key/uuid/user/port, limity 8–256) používají šablony (`generate:` přijímá i nové druhy), MCP `generateSecrets` i klíčová slova. Materializace `lint.Materialize` + `api/generate.go` běží při vytvoření i uložení stacku (UI, NewStack, šablony), u MCP `create_stack`/`update_stack` i při schválení draftu, a před deployem stacku upraveného mimo Moorage (vlastní verze). Coolify jména ověřena proti dokumentaci Coolify a zapsána do `docs/generated-values.md`; stacky s labely `coolify.*` se nematerializují, vlastní stack Moorage a read-only hosty taky ne. Volné porty: nejbližší volný od preferovaného podle mapy portů (M17B), zámek na host, porty < 1024 se přeskočí, bez volného portu chyba s odkazem na Ports, cache mapy portů se po uložení zahodí. Linter: `generate-invalid`, `generate-misplaced` (chyba), `generate-pending` (info), Coolify proměnné neblokují deploy jako `missing-var`; rychlé opravy **Use a free port** (u `port-taken`) a **Add variables** (recepty). Recepty: data v `internal/lint/recipes.yaml` (Postgres, MariaDB/MySQL, Mongo, MinIO, LinuxServer, authentik), validace s neznámými klíči = chyba. **Odchylky:** místo potvrzovacího náhledu před uložením se po uložení v editoru ukáže řádek „Generated: …“ (jen jména a druhy, porty a URL s hodnotou); recepty jsou zatím jen vestavěné (načítání z `moorage-templates` přes katalog šablon není hotové); **metadata image** (`EXPOSE`/`VOLUME`/`HEALTHCHECK` z registru, akce *Add port*) nejsou realizovaná – zůstávají jako další krok M18E. Neověřeno klikáním v UI.

   **Mimo rozsah M18E:** generování TLS certifikátů a SSH klíčů; automatická rotace hesel; zápis vygenerovaných hodnot do externích správců tajemství.
6. **M18F – Karta Problems u kontejnerů a komprimované snapshoty stacků a kontejnerů (návrh 2026-11-26)**

   **Cíl:** U každého kontejneru Moorage konkrétně řekne, co je špatně, ukáže důkaz (exit kód, řádek logu, výstup healthchecku) a nabídne opravu na klik. Před riskantní změnou si uživatel uloží komprimovaný snapshot dat a konfigurace stacku nebo jednoho kontejneru a umí se k němu vrátit.

   **Hlavní tok – Problems:** Stack `immich` je *Degraded* → v seznamu kontejnerů má `immich-server` odznak **1 problem** → karta **Problems** nad seznamem: „**Out of memory** – the container was killed by the kernel 3× in the last 10 min (exit 137, limit 512 MiB, peak 498 MiB)“ → tlačítko **Raise memory limit to 1 GiB** otevře náhled změny `compose.yaml` → **Save & redeploy**. Druhý příklad: „**Port 8080 is already in use** by stack *jellyfin*“ → **Use free port 8081** (M18E) / **Open Ports**.

   **Hlavní tok – snapshoty:** Stack → záložka **Snapshots** → **Take snapshot** → dialog ukáže, co se uloží (volumes, složka stacku, externí bind mounty s velikostí – odškrtnuté), odhad velikosti a volné místo, režim konzistence (**Pause during copy**, výchozí) → hotovo za pár sekund, v seznamu snapshot s velikostí, časem a revizí `compose.yaml`. Později **Restore** → náhled (co se přepíše, diff konfigurace) → Moorage nejdřív uloží snapshot *pre-restore*, zastaví stack, obnoví data a konfiguraci a stack spustí.

   **Rozhodnutí uživatele `[U]` (2026-11-26):**
   - **Karta se jmenuje „Problems“**, ne „Incidents“: ukazuje současné problémy kontejneru a nedávné pády i bez kontroly. Záznamník Docker událostí (pády, OOM, změny healthchecku, restarty) se postaví **jednou a sdílí ho M18A**; incident z M18A převezme diagnózu dotčených kontejnerů. Karta nepřidává nové alerty.
   - **Snapshoty jsou v jádru** jako lokální body obnovy (komprimované `.tar.zst` na hostu, ruční vytvoření, obnova i stažení v UI). `restic-backup` zůstává pro dlouhodobé zálohy mimo server.
   - **Konzistence:** výchozí režim je **pozastavení kontejnerů na dobu kopie** (`docker pause`, konzistence jako po výpadku proudu, databáze se zotaví přes WAL, požadavky čekají). Dále lze zvolit **Stop** (zastavit, zkopírovat, spustit) a **Live (no pause)** – u něj dialog **důrazně varuje**, že snapshot databáze nemusí jít obnovit, a vyžaduje potvrzení zaškrtnutím.
   - **Snapshot kontejneru = jeho volumes + konfigurace** (u služby stacku `compose.yaml` + `.env`, u samostatného kontejneru jeho nastavení). Zapisovatelná vrstva (`docker commit`) se neukládá.
   - **Automatické snapshoty jsou ve výchozím stavu vypnuté.** U stacku jde zapnout **Snapshot before updates** (auto-update i ruční update image); bez zapnutí vznikají snapshoty jen ručně.
   - **Rozsah dat:** vždy named a anonymní volumes a bind mounty uvnitř složky stacku; bind mounty mimo ni (např. `/mnt/media`) dialog ukáže s velikostí, ale **odškrtnuté**, a jde je přidat jednotlivě. Výběr se pamatuje u stacku.
   - **Uchovávání:** posledních **5 automatických** snapshotů na stack; ruční se mažou jen ručně (jdou připnout). Snapshot se nespustí, kdyby po něm zbylo **méně než 10 %** volného místa. Obě hodnoty jdou změnit v **Settings → General**.

   **Architektonická rozhodnutí `[A]` – Problems:**
   - **Záznamník událostí kontejnerů (sdílený s M18A):** agent i hub odebírají Docker events (`die` s exit kódem, `oom`, `health_status`, `restart`) a ukládají je do tabulky `container_events` (host, stack, služba, kontejner, druh, detail, čas; retence 30 dní). Crash loop se slučuje do jednoho řádku s počítadlem, aby záplava restartů nenafoukla DB. Bez přístupu k events (socket proxy bez `EVENTS`) náhradní polling `inspect` po 30 s (rozdíl `RestartCount`). Události jsou vidět i v Logbooku.
   - **Diagnóza běží na hostu, kde kontejner je** (nový balíček `internal/diagnose`, čisté funkce testované na fixturách): vstupem je `inspect` (`State.Status`, `ExitCode`, `OOMKilled`, `Error`, `RestartCount`, posledních 5 výstupů `Health.Log`, `RestartPolicy`, limit paměti), posledních 200 řádků / 64 KiB logu (jen u kontejnerů, které nejsou zdravé), chyba poslední operace stacku (výstup `compose up`) a záznamy z `container_events`. Hub výsledek doplní o to, co zná jen on: Port map (M17B), graf závislostí (R5), volný disk (M15), auto-update a commity v `history.git` z posledních 15 minut. Počítá se na vyžádání (otevření stacku) a po události, výsledek se cachuje do další události, takže PSS gate (100 MB) se nemění.
   - **Pravidla v kódu (obecná)**, každé má závažnost, titulek „co je špatně“, vysvětlení, důkaz a opravy. Opravy jsou jen existující akce s náhledem: restart, rollback image (M8/M14), rollback verze `compose.yaml` (M2), rychlé opravy linteru (M16/M18E), otevření Ports, Logs na daném řádku, terminálu, grafu závislostí nebo úklidu disku. První sada:
     1. **Out of memory** (`OOMKilled` / exit 137 s limitem) → zvednout `mem_limit` (návrh podle špičky ze stats).
     2. **Crash loop** (≥ 3 restarty za 10 min) → první chybový řádek posledního běhu a příčina podle dalších pravidel.
     3. **Command not found / wrong architecture** (exit 126/127, `exec format error`; platforma image vs. host) → správný příkaz nebo varianta image pro architekturu hosta.
     4. **Finished with exit 0 but restart policy restarts it** → u jednorázové úlohy nastavit `restart: "no"`.
     5. **Unhealthy** → poslední výstup healthchecku; když v image chybí nástroj healthchecku (`curl: not found`), návrh náhradního healthchecku.
     6. **Port already in use** → kdo port drží (stack, kontejner, nativní proces) + „Use free port“ (M18E).
     7. **Missing variable** (`:?` v compose, známé hlášky) → doplnění do `.env` (oprava linteru).
     8. **Permission denied** na cestě volume → cesta, vlastník, návrh `user:` / `PUID`/`PGID` podle receptu (M18E).
     9. **Image can't be pulled** (neexistující tag → výběr existujícího přes `imagefind`; `unauthorized` → přihlášení k registru; `toomanyrequests` → limit Docker Hubu).
     10. **No space left on device** → úklid disku s náhledem (M14).
     11. **Dependency is down** (`depends_on` / sdílená síť, `connection refused` / DNS chyba na jinou službu v logu) → odkaz na problém závislosti, ne na tento kontejner.
     12. **Never started / dead** (`created`, `dead`) → chyba z `State.Error`.
     13. **Started after a change** (problém začal do 15 min po commitu nebo updatu image) → „Started after update to `postgres:17.2` at 14:02“ + **Roll back** (po M18A přes Rollback & Verify).
   - **Známé problémy konkrétních image** (např. nekompatibilní data po major upgradu Postgresu) jsou **data v katalogu receptů M18E** (`moorage-templates`): vzor image + RE2 vzor logu (limit délky a počtu) + text a odkaz. Nic se nevyhodnocuje ani nespouští.
   - **Oznámení:** žádný nový druh alertu. Předvolba **Detailed** (M17B) a incident M18A dostanou řádek „Likely cause: Out of memory (exit 137)“ – bez řádků logu. Status stránka diagnózy nikdy nezobrazuje.
   - **UI:** jedna komponenta `ProblemsCard.svelte` pro stack (`StackDetail` → Overview nad seznamem kontejnerů, odznak u řádku kontejneru) i pro samostatné kontejnery (`Home`). Problém jde u kontejneru **ignorovat** (stejně jako nálezy linteru), např. úmyslně jednorázová úloha.

   **Architektonická rozhodnutí `[A]` – snapshoty:**
   - **Formát:** jeden soubor `<data>/snapshots/<stack>/<čas>-<id>.tar.zst` (zstd úroveň 3, `klauspost/compress`, už je v závislostech nepřímo) s `manifest.json` uvnitř i vedle (stack, host, služba u snapshotu kontejneru, čas, verze Moorage, commit `history.git`, digesty image po službách, seznam volumes a mountů s velikostí, režim konzistence, SHA-256 částí). Zápis do `.partial` a přejmenování na konci; zbytky `.partial` se při startu smažou. Soubory `0600`.
   - **Zdroj pravdy je host:** soubory a manifesty na hostu, kde stack běží (agent v `/data` agenta); hub je čte přes agent API a nic necachuje, takže snapshot přežije i obnovu hubu. Nastavení u stacku (auto snapshot, vybrané externí mounty) jsou v hub tabulce nastavení stacku.
   - **Kopírování dělá pomocný kontejner z image Moorage stejné verze** (stejný vzor jako pomocník self-stack apply a host terminálu): podpříkaz `moorage snapshot-helper` připojí volumes jen pro čtení a složku snapshotů a streamuje tar + zstd. Hub/agent nic nekopíruje sám, paměť hlídaná PSS gate zůstává stejná. U nativní instalace se image stejné verze stáhne; offline bez image = srozumitelná chyba.
   - **Pauza je vždy ukončena:** `unpause` v `defer` i při chybě a zrušení; seznam pozastavených kontejnerů se zapíše do DB a po pádu nebo restartu Moorage je ihned odpauzuje. Na dobu snapshotu se použije stejné ztišení alertů jako při deployi (M4), takže probe během pauzy nepošle DOWN.
   - **Anonymní volumes se mapují podle služby a cílové cesty** (jejich jména se po `down`/recreate mění); sdílený volume (`external: true`, více služeb) se uloží jednou a dialog řekne, kdo ho ještě používá. `tmpfs` se ignoruje.
   - **Obnova:** náhled (co se přepíše, velikosti, diff `compose.yaml`/`.env` proti současné verzi) → volba **Data + configuration / Data only / Configuration only** → automaticky snapshot *pre-restore* (lze vypnout; při nedostatku místa varování) → zastavení dotčených služeb → vyprázdnění a rozbalení volumes → konfigurace se uloží jako nová verze v `history.git` → `up`. Obnova snapshotu kontejneru vrací jen data té služby; konfiguraci nabídne jen jako diff (`compose.yaml` je společný pro celý stack). U samostatného kontejneru se obnovují jen data, nastavení je k dispozici jako příkaz `docker run`.
   - **Snapshot before updates (když je zapnutý):** selže-li snapshot (místo, chyba), update se **odloží** a přijde oznámení – uživatel tuto pojistku zapnul úmyslně. Rollback & Verify (M18A) pak nabídne vrátit image **i data** ze snapshotu před updatem.
   - **Stažení:** admin, streamovaně, v auditu, s upozorněním, že soubor obsahuje `.env` i data aplikace (nešifrováno, stejně jako data na disku hostu). Stažení z agenta jde tunelem s nižší prioritou a omezenou rychlostí (souvisí s měřením head-of-line blocking v M18D).
   - **Oprávnění a audit:** vytvoření, obnova, smazání a stažení = admin, záznam v auditu i Logbooku; viewer vidí jen seznam.

   **Okrajové stavy:**
   - **Problems – zdravý kontejner:** karta se nezobrazí, řádek bez odznaku. **Healthcheck `starting`** v rámci `start_period` = informace „Starting…“, ne problém. **Údržbové okno:** problémy se ukážou s označením „during maintenance“.
   - **Problems – starší agent** bez diagnostiky: základní diagnóza ze stavu, exit kódu a health ze seznamu kontejnerů + „Update the agent for a detailed diagnosis“. **Read-only agent:** diagnóza ano, zapisující opravy neaktivní s důvodem.
   - **Problems – kontejner mezitím zmizel / byl znovu vytvořen:** problém se uzavře, historie zůstane v Logbooku.
   - **Snapshot – nedostatek místa:** dialog ukáže odhad a volné místo, tlačítko neaktivní s důvodem; automatický snapshot odloží update (viz výše). **Velký odhad (> 5 min kopie):** dialog upozorní na délku pauzy a nabídne Stop / Live.
   - **Snapshot – read-only agent, vlastní stack Moorage** (ten pokrývá záloha `.mbk`), **externí stack na Level 1 a stack spravovaný Coolify:** u prvních dvou nedostupné s vysvětlením; u externích a Coolify stacků jen data (konfiguraci Moorage nevlastní).
   - **Snapshot – starší agent:** záložka Snapshots ukáže „Update the agent to use snapshots“.
   - **Obnova, když image z manifestu už v registru není:** varování v náhledu; obnoví se data a konfigurace, `up` použije aktuálně dostupný tag.
   - **Souběh:** na stacku běží vždy jen jedna operace (snapshot, obnova, deploy, update) – stávající zámek operací stacku.

   **Data a API:** migrace tabulky `container_events` (+ index host/stack/čas) a záznamu pozastavených kontejnerů; nastavení snapshotů v nastavení stacku a v General. Agent API: `GET /containers/{id}/diagnose`, `GET /stacks/{name}/snapshots`, `POST /stacks/{name}/snapshots` (operace s průběhem), `POST /stacks/{name}/snapshots/{id}/restore`, `GET …/{id}/download`, `DELETE …/{id}` a stream událostí. Hub API zrcadlí cesty pod `/api/hosts/{host}/…`, CLI a MCP dostanou `snapshot create/list/restore`.

   **Pořadí realizace:** (1) záznamník událostí kontejnerů – společně s M18A, (2) diagnóza a karta Problems, (3) ruční snapshoty a obnova, (4) *Snapshot before updates* a napojení na Rollback & Verify (po M18A). Recepty známých problémů image až s katalogem receptů M18E.

   **Mimo rozsah M18F:** ukládání image (`docker save`) do snapshotu; plánované snapshoty podle času (to řeší `restic-backup`); nahrávání snapshotů mimo server a jejich šifrování; databázově vědomé zálohy (`pg_dump` hooky); vysvětlení problému pomocí AI (řeší M18G).
   **Stav (2026-11-26, necommitováno):** Hotovo (2)–(4) nad záznamníkem z M18A. **Problems:** čisté funkce `internal/diagnose` (pravidla 1–13 s testy na fixturách), fakta z `docker inspect` přes `host.StackDiagnoser` (lokálně i agent `GET /stacks/{name}/diagnose`; starší agent → základní diagnóza ze seznamu kontejnerů s poznámkou), hub doplní ocas logu nezdravých kontejnerů (200 řádků / 64 KiB), události z `container_events`, mapu portů, verze z posledních 2 h a údržbu. API `GET …/problems`, ignorování `PUT …/problems/ignored` (nastavení, audit). UI `ProblemsCard.svelte` nad seznamem kontejnerů + odznak u řádku; opravy jsou rychlé opravy linteru s náhledem diffu (nové `set-mem-limit`, `set-restart-no`, `use-free-port`, `add-env-var`, `add-healthcheck`) → Save & redeploy, jinak odkazy (Logs, Compose, Dependencies, History, Hosts/Ports, General). „Likely cause“ jde do incidentu (`AlertContext.Cause`) a do předvolby Detailed. **Snapshoty:** `internal/stacksnap` (tar + zstd, manifest uvnitř i vedle, SHA-256 částí, bezpečné rozbalení), pomocný kontejner `moorage snapshot-helper` z vlastního image (volumes read-only, data dir přes `VolumesFrom`/bind), režimy pause/stop/live, pauza vždy ukončena (`defer` + `paused.json` obnovený při startu, úklid `.partial`), mapování anonymních volumes podle služby a cíle, sdílené volumes jednou, kontrola volného místa, retence 5 automatických, připnutí. Obnova: pre-restore snapshot, stop, rozbalení, konfigurace jako nová verze, up. Hub API zrcadlí agent API (`host.Snapshotter`), stažení streamovaně, přes tunel s omezením rychlosti, v auditu. *Snapshot before updates* v `Updater.StartUpdate` (chyba = update odložen + záznam v Logbooku). **Odchylky / nehotové:** záznamník událostí dál polluje jen stacky s kontrolami (ne Docker events všech stacků); karta Problems jen u stacků (ne u samostatných kontejnerů na Home); Logbook události kontejnerů nezobrazuje; ztišení alertů během pauzy není napojené; při ručním updatu se zapnutým snapshotem čeká HTTP požadavek na dokončení snapshotu; CLI/MCP `snapshot …` nejsou; Rollback & Verify zatím nenabízí vrácení dat ze snapshotu; recepty známých problémů image chybí (čekají na katalog receptů). Volné místo se počítá jen na Linuxu. Neověřeno proti skutečnému Dockeru (helper kontejner, pauza, obnova) ani klikáním v UI.
7. **M18G – Built-in add-on „AI assistant“ (návrh 2026-11-26)**

   **Cíl:** Panel vpravo s jednorázovým chatem, který ví, co kde v Moorage je, zná adresu, na které má uživatel Moorage otevřenou, dává přesné odkazy a kroky a podle nastavené míry oprávnění připraví nebo sám provede změnu.

   **Hlavní tok:** Uživatel na stránce stacku `immich` otevře panel (ikona v hlavičce, `Ctrl+J`) a napíše „proč mi padá immich?“ → asistent si přečte stav a kartu Problems (M18F) → „Kontejner `immich-server` zabíjí OOM (limit 512 MiB). Navrhuji zvýšit `mem_limit` na 1 GiB.“ → karta akce s diffem `compose.yaml` → **Apply** → uložení verze + redeploy, záznam v auditu „via assistant“. Na otázku „kde zapnu Discord alerty?“ odpoví krokem a odkazem `http://192.168.1.10:8080/#/settings/notifications`.

   **Rozhodnutí uživatele `[U]` (2026-11-26):**
   - **Jednorázový chat:** konverzace žije jen v prohlížeči (panel), po zavření nebo obnovení stránky zmizí; server konverzace neukládá.
   - **Míru oprávnění volí uživatel** (admin v nastavení add-onu), zvlášť pro každou kategorii akcí, úrovně **Off / Advise** (jen vysvětlí a dá odkaz) / **Propose** (karta akce, provede až Apply) / **Auto** (provede sám, výsledek ukáže v chatu, akce jde vrátit tam, kde Moorage umí rollback). Výchozí úroveň všech kategorií je **Propose**.
   - **Kategorie akcí, které AI smí:** (1) start / restart / stop / pull stacku (nikdy `down`, delete ani vlastní stack Moorage, jako MCP `stack_action`); (2) úpravy `compose.yaml` jako nová verze s diffem a deploy, vytvoření stacku ze šablony; (3) **vygenerování vlastního `compose.yaml`** pro nový stack a použití `@generate` (M18E) – nové hodnoty v `.env` vznikají jen generátorem, **existující hesla a hodnoty `.env` AI nikdy nezmění ani neuvidí**; (4) kontroly, údržbová okna a notifikační kanály (bez čtení tajných tokenů kanálů) a jejich test; (5) zapnutí vestavěného add-onu a instalace sidecaru.
   - **Poskytovatelé:** Anthropic (Claude), OpenAI (ChatGPT), Google Gemini, Ollama a obecný **OpenAI-kompatibilní endpoint** (pokryje FreeLLMAPI, OpenRouter, Groq, Mistral, DeepSeek, LM Studio, vLLM, LocalAI). Agent navrhuje ještě Azure OpenAI a Amazon Bedrock jako pozdější rozšíření (→ `ROADMAP.md`).
   - **Přístup k datům nastavuje uživatel** zvlášť pro každý druh dat, úrovně **Never / Ask in chat** (asistent požádá, uživatel povolí pro tento chat) / **Allowed**.

   **Architektonická rozhodnutí `[A]`:**
   - **Pevný zákaz bez ohledu na nastavení:** mazání stacků, volumes a snapshotů, `down`, terminál, uživatelé, role, tokeny, přihlášení a SSO, self-update, obnova záloh a snapshotů, úklid disku, čtení a změna existujících hodnot `.env`. Asistent na ně dá jen postup a odkaz. Seznam je v kódu (nástroje pro tyto akce vůbec neexistují), ne v promptu.
   - **Strop role:** asistent jedná vždy s oprávněními přihlášeného uživatele. Viewer dostane jen čtecí nástroje bez ohledu na úrovně; read-only host = zapisující nástroje vypnuté s důvodem.
   - **Úroveň Auto a prompt injection:** logy, labely a odpovědi kontrol můžou obsahovat text, který vypadá jako pokyn. Proto (a) prompt i výstupy nástrojů jasně oddělují data od pokynů, (b) **akce v úrovni Auto, která navazuje na nástroj čtoucí logy nebo cizí obsah ve stejném tahu, se automaticky převede na Propose**, (c) nastavení u Auto ukáže varování. Úrovně Auto pro „Compose edits & deploy“ a „Add-ons“ vyžadují potvrzení zaškrtnutím v nastavení.
   - **Data:** druhy dat a výchozí úrovně: stav hostů, stacků a kontejnerů + karta Problems = *Allowed*; `compose.yaml` (hodnoty `.env` vždy maskované, klíče viditelné) = *Allowed*; porty a adresy hostů = *Allowed*; **logy kontejnerů = *Ask in chat*** (max 200 řádků, maskování hodnot z `.env` a vzorů tajemství); Logbook a audit = *Ask in chat*; výstupy kontrol (tělo HTTP odpovědi) = *Never*. Maskování se dělá na serveru před odesláním poskytovateli.
   - **Adresa uživatele:** posílá se `location.origin` z prohlížeče (adresa, kterou má uživatel otevřenou, i za reverse proxy) a pojmenované adresy hostů z M17B. Odkazy asistent skládá jen z generované mapy rout, takže nevymýšlí neexistující stránky.
   - **Architektura:** smyčka nástrojů běží na hubu (`internal/assistant`), prohlížeč posílá historii chatu a dostává stream (SSE přes stávající autentizovanou session, ne přes MCP tokeny). Nástroje jsou **společný registr s MCP serverem** (`addon_mcp.go`): stejné funkce, stejné ACL a stejné **drafty** (MCP drafty = karty akcí asistenta), takže pravidla nejdou rozjet. Každá provedená akce jde do auditu s `actor = <uživatel> via assistant` a do Logbooku.
   - **Adaptéry poskytovatelů** za jedním rozhraním (`Chat(ctx, system, messages, tools) → stream`): Anthropic přes oficiální Go SDK (`anthropic-sdk-go/v2`, ruční tool loop kvůli kartám akcí, cachování statického system promptu, výchozí model `claude-opus-5-5` se zapnutým server-side fallbackem při refusal, model jde v nastavení změnit), OpenAI (Responses/Chat Completions s function calling), Gemini (function calling) a OpenAI-kompatibilní endpoint (URL + volitelný klíč + model). Nástroje se posílají jen modelům, které function calling umí; jinak asistent běží v úrovni Advise a nastavení to řekne. Odchozí spojení přes `netguard` (LAN povolená, takže lokální Ollama funguje).
   - **System prompt** je v `docs/design/assistant-system-prompt.md`: statická část (role, mapa rout, pravidla, styl) se cachuje, kontext relace (verze, `origin`, role, aktuální stránka, host, add-ony, titulky Problems) se posílá za cache breakpointem. Mapa rout a popis oprávnění se generují při buildu z routeru a registru nástrojů; test selže, když chybí.
   - **API klíče** se ukládají jako ostatní tajemství Moorage (write-only v UI, nikdy se nevrací do prohlížeče, nejsou v `.mbk` exportu v čitelné podobě – stejně jako tokeny kanálů).
   - **Náklady:** u každé odpovědi počet tokenů, souhrn za měsíc v nastavení add-onu a volitelný měsíční limit tokenů (po dosažení asistent odpoví, že limit vypršel). Limit délky konverzace (např. 40 zpráv / 200k tokenů) – pak nabídne začít znovu.
   - **[U] Bez označování AI (2026-11-26):** Moorage je lokálně hostovaná aplikace, panel ani karty akcí nepotřebují štítek „AI“. Add-on je ve výchozím stavu vypnutý a bez klíče nic neodchází.
   - **Paměť (PSS gate):** konverzace drží prohlížeč, hub jen streamuje; žádné vlastní embeddingy ani index dokumentace.

   **Okrajové stavy:**
   - **Add-on vypnutý / bez klíče:** ikona panelu se nezobrazí adminovi jinde než v Settings → Add-ons (`<AddonGate>`); ostatní ji nevidí.
   - **Poskytovatel nedostupný, 401, 429, vyčerpaný kredit:** chybová bublina s důvodem a odkazem do nastavení add-onu; konverzace zůstane, jde zkusit znovu.
   - **Model bez function calling:** jen rady a odkazy (Advise), upozornění v nastavení i v panelu.
   - **Návrh zastará** (mezitím jiná změna stacku, jiná verze `compose.yaml`): Apply odmítne a asistent návrh přepočítá – stejná kontrola verze jako u MCP draftů.
   - **Uživatel změní stránku:** panel zůstane otevřený, kontext relace se aktualizuje s další zprávou.
   - **Více uživatelů současně:** každý má vlastní chat v prohlížeči; akce se zamykají stávajícím zámkem operací stacku.
   - **Odpověď nedorazí celá** (výpadek spojení): co dorazilo, zůstane označené „incomplete“; žádná akce se neprovede bez dokončeného výsledku nástroje.

   **Data a API:** bez nové tabulky pro konverzace; nastavení add-onu (poskytovatel, model, šifrovaný klíč, úrovně akcí a dat, limity, měsíční spotřeba) v tabulce nastavení. Endpointy `POST /api/assistant/chat` (SSE), `POST /api/assistant/actions/{draftId}/apply|reject` (sdílí logiku MCP draftů), `GET/PUT /api/addons/assistant/settings`. Frontend `AssistantPanel.svelte` + nastavení v `BuiltinAddonPanel`.

   **Pořadí realizace:** (1) společný registr nástrojů a draftů s MCP, (2) adaptér Anthropic + panel v úrovni Advise/Propose, (3) úrovně Auto a nastavení dat, (4) adaptéry OpenAI, Gemini a OpenAI-kompatibilní. Nejlépe po M18F (karta Problems je hlavní zdroj diagnózy) a M18E (`@generate`).

   **Mimo rozsah M18G:** ukládání historie chatů; hlasové ovládání; asistent běžící sám na pozadí (bez otevřeného panelu) nebo reagující na alerty; vlastní fine-tuning a RAG nad dokumentací; Azure OpenAI a Bedrock.

   **Otevřené otázky (M18G):** žádné blokující. Přesný seznam modelů ve výběru se ověří při implementaci (modely se mění; výběr bude načítat seznam z API poskytovatele, kde to jde).
   **Stav (2026-11-26, necommitováno):** Hotovo (1)–(3) a adaptéry. `internal/assistant`: rozhraní `Session` + smyčka nástrojů (max 12 kroků, výsledky z cizích zdrojů obalené jako nedůvěryhodná data a nastavující příznak, po kterém se Auto mění na Propose), adaptér **Anthropic přes oficiální `anthropic-sdk-go/v2`** (beta Messages, cachovaný statický system prompt + kontext relace za breakpointem, `fallbacks: default` u Opus 5.x/Fable/Sonnet 5.5, ochrana proti `max_tokens` a refusal) a **OpenAI-kompatibilní** adaptér (Chat Completions s function calling) pro OpenAI, Gemini (jeho OpenAI endpoint), Ollama a vlastní URL; odchozí spojení přes `netguard` (LAN povolená). System prompt `internal/assistant/system_prompt.md` + mapa rout s testem proti routeru. API: `POST /api/assistant/chat` (SSE, stav jen v prohlížeči, limit 40 zpráv, měsíční limit tokenů), `GET /api/assistant/status`, `GET/PUT /api/addons/assistant/settings` (klíč write-only), `POST /api/assistant/actions/{id}/apply|reject`. Nástroje: čtecí `list_stacks`, `get_stack` (Problems, compose s maskovanými hodnotami `.env`, jen jména klíčů), `get_logs` (Never/Ask/Allowed, max 200 řádků, maskování na serveru); akční (jen admin, read-only host a vlastní/add-ons stack odmítnuty) `stack_action`, `propose_compose`, `create_stack` s úrovněmi Off/Advise/Propose/Auto (Auto u compose vyžaduje potvrzení). Návrhy jsou MCP drafty (`TokenName assistant:<user>`), aplikace sdílí `applyDraft` s MCP; zastaralý návrh (jiný compose/.env) se odmítne. Audit „<user> via assistant“ + Logbook. UI: `AssistantPanel.svelte` (Ctrl+J, ikona v hlavičce jen když je add-on zapnutý a nakonfigurovaný, karty akcí s diffem, tlačítko Allow pro logy) a `AssistantSettingsPanel.svelte`. **Nehotové:** kategorie (4) kontroly/okna/kanály a (5) add-ony; data Logbook/audit; samostatný adaptér Gemini (místo něj jeho OpenAI-kompatibilní endpoint); načítání seznamu modelů z API; tokenový limit délky konverzace (jen počet zpráv); MCP server zatím nepoužívá stejný registr nástrojů (sdílí jen drafty a jejich aplikaci); odpovědi se streamují po krocích, ne po tokenech. Neověřeno proti skutečným poskytovatelům.
8. **M18H – Spolehlivý rollback „Apply & restart“ vlastního stacku a linter pro `network_mode` (návrh 2026-11-26, oprava)**

   **Cíl:** Když nová konfigurace vlastního stacku Moorage odřízne uživatele od UI (ztracený port, změněná síť, proxy), helper ji vrátí a UI to po návratu jasně oznámí. Linter na takovou konfiguraci upozorní ještě před uložením.

   **Zjištěná příčina:** Helper (`selfupdate/stackapply.go`) vrací zpět jen při chybě `compose up` nebo když kontejner Moorage nenaběhne. Zdraví ověřuje jen zevnitř Dockeru (IP kontejneru + `/api/session` nebo značka zdraví v `/data`). Příklad: `network_mode: host` + `ports: ["8080:5080"]` → Docker porty tiše zahodí, Moorage běží na portu hosta 5080 a je „zdravá“ → **Applied**, ale UI na `:8080` už neodpovídá. Rollback navíc obnoví jen soubory, ne verzi v `history.git` (poslední verze v historii zůstane ta chybná), a log helperu zmizí s kontejnerem (`AutoRemove`).

   **Rozhodnutí uživatele `[U]` (2026-11-26):**
   - **Vylepšená sonda helperu** (bez potvrzování z prohlížeče): helper a Moorage běží na stejném hostu, helper ověří, že Moorage odpovídá na adresách, kudy k ní uživatelé chodí.
   - **Po rollbacku se v UI zobrazí výrazné upozornění**, že proběhl rollback a proč.

   **Architektonická rozhodnutí `[A]`:**
   - **Helper poběží v síti hosta** (`network_mode: host`) místo sítě kontejneru Moorage. Z hosta dosáhne na publikované porty (`127.0.0.1:<port>`) i na IP kontejnerů v bridge sítích. Odpadá i závislost helperu na síti, kterou může nová konfigurace odstranit. Docker socket a mounty zůstávají stejné.
   - **Co sonda ověří** (vše musí odpovědět na `/api/session`, resp. u agenta na health endpoint, po dobu `StableFor` 15 s, celkově do 120 s):
     1. **Každý publikovaný TCP port služby Moorage z nové konfigurace** na `127.0.0.1` (u bindu na konkrétní IP na té IP). Zahozené porty u `network_mode: host` tím selžou → rollback s důvodem „Moorage doesn't answer on port 8080 (network_mode: host ignores ports:)“.
     2. **U `network_mode: host`** port, na kterém Moorage poslouchá (`MOORAGE_LISTEN`, výchozí 5080).
     3. **Přístup přes reverse proxy:** sítě, které starý kontejner Moorage sdílel s kontejnerem proxy (Traefik, Caddy a další podle labelů z add-onu `proxy-labels`), musí nový kontejner mít také a odpovídat na své IP v té síti. Jinak rollback s důvodem „Moorage left network `proxy` that Traefik uses“.
     4. **Port, přes který uživatel změnu odeslal** (`Host` požadavku, když je to přímý port tohoto hosta): když ho nová konfigurace záměrně mění, UI před uložením upozorní („You're connected on :8080, after the restart Moorage will be on :9090“) a po restartu nabídne odkaz na novou adresu. Záměrná změna portu není důvod k rollbacku.
   - **Mimo Linux** (Docker Desktop, kde host nevidí IP kontejnerů) a u Podmanu rootless zůstává dnešní kontrola (značka zdraví v `/data`), v náhledu Apply je to uvedeno.
   - **Rollback se zapíše do historie:** po obnově souborů helper zapíše novou verzi „Rolled back: <důvod>“ do `history.git` (stejná cesta jako obnova verze v M2), takže diff i Logbook odpovídají tomu, co běží.
   - **Log helperu přežije:** výstup helperu (`compose up`, sonda, rollback) se zapisuje do `<data>/self-stack-apply.log` (poslední běh, max 256 KiB) a je dostupný z upozornění i ze stránky stacku. Když selže i `compose up` se starými soubory, výsledek a log zůstanou v `/data` a dokumentace (`docs/`) popíše ruční obnovu `docker compose up -d` ve složce stacku.
   - **Upozornění v UI:** po návratu Moorage uvidí každý admin **globální banner** v `Shell` (ne jen na stránce stacku): „Apply & restart was rolled back – Moorage didn't answer on port 8080 (network_mode: host ignores ports:). The previous configuration is running again.“ s odkazy **Show log** a **Open stack** a tlačítkem **Acknowledge** (stejný vzor jako banner auto-rollbacku v `StackDetail`). Záznam jde také do auditu, Logbooku a oznámení jako druh události, kterým se dnes hlásí rollback self-update.
   - **Apply & restart odmítne konfiguraci s chybou linteru** (`DeployError`) u služby Moorage, například `ports:` s `network_mode: container:`, nebo stejnou kombinaci host + ports, která by sondu stejně neprošla. U ostatních stacků zůstává varování.

   **Linter – nová pravidla:**
   - **`ports-ignored`** (varování): služba s `network_mode: host` má `ports:` → „`ports:` has no effect with network_mode: host: the service listens directly on the host's ports.“ Rychlá oprava **Remove ports:**. U vlastního stacku Moorage chyba, viz výše.
   - **`ports-conflict-network`** (`DeployError`): `ports:` u `network_mode: container:<x>`, `service:<x>` nebo `none` (Docker odmítne spustit) → stejná rychlá oprava.
   - U služeb s `network_mode: host` se publikované porty **nezapočítávají** do kontrol `port-taken`, `duplicate-port` a `db-port-public` ani do Port map (M17B). Jinak by zahozené porty hlásily falešné kolize.

   **Okrajové stavy:**
   - **Moorage poslouchá jen na `127.0.0.1` uvnitř hostu, bez publikovaných portů** (přístup jen přes proxy): ověří se jen bod 3.
   - **Agent (ne hub):** stejná sonda na health endpoint agenta. Ověří se i to, že se agent po restartu znovu připojil tunelem k hubu, jinak rollback.
   - **Helper nejde spustit v síti hosta** (politika, rootless): použije se dnešní síť kontejneru a kontrola jako dnes, v náhledu s vysvětlením.

   **Data:** bez migrace. Nový soubor `<data>/self-stack-apply.log`, rozšířený `self-stack-result.json` (důvod, kontrolované adresy, odkaz na log, potvrzení banneru).

   **Mimo rozsah M18H:** potvrzování konfigurace z prohlížeče; sondy přes veřejnou doménu zvenčí.
   **Stav (2026-11-26, necommitováno):** Hotovo. Linter: `ports-ignored` (varování, `network_mode: host` + `ports:`) a `ports-conflict-network` (`DeployError`, `container:`/`service:`/`none`), oprava **Remove ports:**, porty těchto služeb se nezapočítávají do `port-taken`/`duplicate-port`/`db-port-public` (Port map je vynechávala už dřív). Apply & restart odmítne konfiguraci vlastního stacku s kterýmkoli z těch nálezů (422). Helper (`selfupdate/stackapply.go`): na Linux Dockeru (ne Docker Desktop, ne rootless – podle `docker info`) běží v síti hosta (`--host-net`), při odmítnutí se spustí v síti kontejneru jako dřív. Sonda: všechny publikované TCP porty nového kontejneru na 127.0.0.1 / vázané IP, u `network_mode: host` listen port, a sítě sdílené před `up` s kontejnerem proxy (podle image: Traefik, Caddy, nginx-proxy, HAProxy, Envoy, SWAG, cloudflared, Pangolin, Zoraxy) – nový kontejner v nich musí být a odpovídat na své IP; `ports:` ignorované kvůli `network_mode: host` = okamžitý rollback s důvodem. Log helperu do `<data>/self-stack-apply.log` (max 256 KiB), výsledek rozšířen o důvod, kontrolované adresy, log a potvrzení. Po obnově souborů zapíše helper verzi „Rolled back: <důvod>“ do `history.git`. Hub: `GET /api/self-apply/notices`, `POST /api/hosts/{host}/self-apply/ack`, `GET …/self-apply/log` (agent API zrcadlí), globální banner pro adminy v `Shell` (Show log / Open stack / Acknowledge). Dokumentace ruční obnovy `docs/self-stack.md`. **Nehotové:** upozornění v UI před uložením při záměrné změně portu (bod 4) a odkaz na novou adresu; u agenta kontrola znovupřipojení tunelu; záznam rollbacku do auditu a oznámení (jen historie + banner); proxy se poznává jen podle image, ne podle labelů `proxy-labels`. Neověřeno proti skutečnému Dockeru.

9. **M18I – Logování: nastavení, chybějící události a pravidla (návrh 2026-11-26)**

   **Cíl:** Log Moorage (`docker logs`) řekne adminovi a nástrojům jako CrowdSec/fail2ban nebo Loki všechno podstatné o provozu a bezpečnosti, a nikdy v něm nebudou tajné údaje.

   **Výchozí stav (audit 2026-11-26):** ~180 zápisů přes `slog`, textový formát na stderr, pevně úroveň Info (4 zápisy na úrovni Debug se nikdy nevypíší). Error 18 (start a pád procesu, OIDC, chyby Dockeru v API, šablony, notifikace), Warn 79 (výpadky kontrol, ztráta spojení agent–hub, selhání zálohy, úklidu, auto-update, rollbacku a web push, chyby DB, varování ke konfiguraci, setup token), Info 58 (spuštění, přihlášení, změny stacků, kontrol, kanálů a status stránek, terminály, agenti, compose operace, notifikace, promazání). Chybí: neúspěšná přihlášení a rate limit, kontext u chyb `engine`/`auth`, nové funkce M18 a access log.

   **Rozhodnutí uživatele `[U]` (2026-11-26):**
   - **Neúspěšné přihlášení:** Warn `sign-in failed` s IP, metodou (password, totp, passkey, oidc, invite) a důvodem; uživatelské jméno jen tehdy, když účet existuje (do pole jména se omylem píše heslo). Rate limit: Warn `sign-in rate limited` s IP.
   - **Access log:** jen odpovědi 5xx (metoda, vzor routy bez query, status, doba); všechny požadavky na úrovni Debug.
   - **Formát:** výchozí text, `MOORAGE_LOG_FORMAT=json` pro Loki/Promtail.
   - **Úrovně Warn → Error:** selhání zálohy (export, export historie, plánovaná vzdálená záloha); ztráta auditu nebo historie (nezapsaný audit včetně terminálů, párování agenta, self-update a auto-update, `history: commit`); ztráta dat monitoringu (uložení výsledků, otevření/změna/uzavření incidentu, časová osa incidentu); nezdařené automatické opravy (image rollback, auto-rollback, Rollback & Verify, auto-update se nespustil, self-update vrácený při bootu); klonovaný passkey (čítač podpisů šel zpět); snapshot status stránky od 3. selhání za sebou. Nedoručené alerty a rozbitý watchdog zůstávají Warn (uživatel zvažoval Debug; agent nedoporučil, protože by špatně nastavený kanál nebyl ve výchozím logu vidět).

   **Architektonická rozhodnutí `[A]`:**
   - `MOORAGE_LOG_LEVEL` (debug, info, warn, error; výchozí info) a `MOORAGE_LOG_FORMAT` (text, json) v `config`; neznámá hodnota = varování při startu a výchozí hodnota. Stejné nastavení dostane i helper Apply & restart.
   - Chyby `engine` a `auth` v API dostanou metodu, vzor routy, host a stack.
   - Neúspěšná compose operace: Warn `compose operation failed` s chybou (dnes Info s `ok=false`).
   - **Nové události (Info, při selhání Warn):** vytvoření, obnova a selhání snapshotu (stack, id, režim, velikost, doba); asistent (akce navržená, aplikovaná nebo provedená v Auto, chyba poskytovatele se stavovým kódem, vyčerpaný měsíční limit – nikdy obsah chatu); vygenerované hodnoty (stack a jména klíčů, nikdy hodnoty); rollback Apply & restart zjištěný při startu (Warn s důvodem); volání nástrojů MCP (token, nástroj, host, stack – nikdy argumenty).
   - `closing live connection` se sníží na Debug.
   - **Nikdy do logu:** hodnoty `.env`, hesla, tokeny, API klíče, join tokeny, obsah chatu asistenta, řádky logů kontejnerů, těla HTTP odpovědí kontrol. Hlídá to test, který spustí typické toky s rozpoznatelnými tajnými hodnotami a projde zachycený log.
   - Dokumentace `docs/logging.md`: proměnné, význam úrovní, seznam událostí bezpečnosti, ukázka filtru pro CrowdSec/fail2ban.

   **Okrajové stavy:** agent i hub používají stejné nastavení; v demo režimu se odmítnuté změny nelogují (jsou očekávané); access log ignoruje zrušené požadavky klienta (499), aby SSE a WebSocket neplnily log.

   **Data a API:** bez migrace a bez změn API.

   **Mimo rozsah M18I:** zápis logu do souboru nebo jeho rotace (řeší Docker); posílání logu přímo do Loki/syslog; zobrazení logu Moorage v UI.
   **Stav (2026-11-27, necommitováno):** Hotovo. `config.NewLogger` (`MOORAGE_LOG_LEVEL`, `MOORAGE_LOG_FORMAT`, neznámá hodnota = varování při startu), stejný logger i pro helper Apply & restart. Neúspěšná přihlášení: `logSignInFailure` u hesla, hesla+passkey, passkey a odkazů (IP, metoda, důvod, jméno jen u existujícího účtu), rate limit zvlášť. Access log `accessLog` uvnitř auditu: každý požadavek na Debug, 5xx jako Error `request failed` (vzor routy, host, stack, status, ms, příčina z `engErr`/chyby auth přes `reportErr`), zrušené požadavky se nelogují, wrapper zachovává Flush i Hijack (SSE, WebSockety). Compose operace s chybou = Warn. Nové události: snapshot vytvořen/selhal/obnoven/obnova selhala (Error), asistent (návrh, provedení, aplikace, chyba poskytovatele jen se stavem, vyčerpaný limit), vygenerované hodnoty (jen klíče), volání nástrojů MCP (bez argumentů), rollback Apply & restart při startu. Přeřazení Warn → Error podle rozhodnutí; `closing live connection` na Debug. Test `TestLogNeverContainsSecrets`, dokumentace `docs/logging.md`.

### Mimo rozsah M18 (→ `ROADMAP.md`)
- Wasm (`wazero`) sandbox pro uživatelské add-ony.
- Hluboká hardwarová telemetrie (S.M.A.R.T., GPU, teploty) – pokrývá sidecar `node-exporter` z M15.
- Plná podpora Podman Quadletů (Podman jako takový je v „Schválené návrhy“).
- LDAP a externí správci tajemství.

### Otevřené otázky
- **Pořadí vln M18** – doporučení: A (největší odlišení) spolu se záznamníkem událostí a kartou Problems z F, pak B (nutné před veřejným vydáním), E (rychlá denní užitečnost, staví na M17B), snapshoty z F, C, D.
- **Název projektu před veřejným vydáním:** analýza upozorňuje na kolizi názvu „Moorage“ (moorage.dev, github.com/moorage, macOS utilita Moorage, Docker PaaS Mooring). Rozhodnutí ovlivní URL loga v oznámeních (M17B), názvy image a dokumentaci; mělo by padnout před M18B.

## M19 – Aktualizace a Apply & restart vlastního stacku Moorage (schváleno a implementováno 2026-11-27, necommitnuto)

### Stav implementace (2026-11-27)
- **Hotovo A–M**, `bash scripts/dev/check.sh` prošel celý. Podman 5.4.2 ověřen na WSL (Debian 13): `podman-smoke.sh` rootful i rootless ALL PASS, nový `scripts/dev/podman-selfstack.sh` (Moorage v kontejneru, detekce vlastního stacku, Saved not applied, Apply & restart přes pomocníka) rootless ALL PASS. Tabulka „ověřeno / omezení“ je v README.
- **Odchylky od návrhu (vědomé, ke kontrole):**
  - M19E: Společný registr nástrojů `internal/tools` pro MCP i asistenta nevznikl. Asistent dostal nové čtecí nástroje (`list_hosts`, `get_service_links`, `get_problems`, `get_ports`, `ask_choice`) a MCP nové nástroje, prompts a resources, ale definice zůstávají ve dvou souborech. `check_images` a `list_checks`/`list_deps`/`lint_compose` asistent zatím nemá.
  - M19E: Kód chyby vrací nabídku „Try with <poskytovatel · model>“ jen nepřímo (admin přepne model v panelu); samostatné tlačítko chybí.
  - M19F: Předvolby **Add host defaults** (uložená viditelná pole a výchozí hodnoty) neukládají se; pole jsou v Add host vždy všechna. Kompaktní graf je předvolba `depsView`, přepíná se přímo v grafu.
  - M19F: Odkaz z `EXPOSE` image u host sítě: použije se jen port deklarovaný v compose (`ports`), ne `EXPOSE` image.
  - M19I: Explain this alert je i u řádku incidentu v Logbooku stacku (doplněno 2026-11-27).
  - M17B: Náhled log odesílatele v Settings → Notifications jde přes `GET /api/notifications/logo` (CSP stránky blokuje cizí obrázky); CSP se neuvolňuje.
  - M19J: Odkazy z ruční (ne-Moorage) konfigurace Tailscale Serve se u služeb neukazují jako odkaz „Tailscale“; Serve vytvořený Moorage se uloží jako Set link.
  - M19L: Varování kanálu po neúspěšném označení smazané role chybí (Discord webhook chybu nevrací).
  - M19M: Předchozí verze souborů z Files tab jsou v `<data>/file-history` (posledních 10), ne v `history.git` – historie verzuje jen compose a `.env` a obnova verze by jinak mohla jiné soubory smazat. Čtení obsahu souborů je jen pro admina (konfigurace můžou obsahovat tajné hodnoty).
  - M19M: Diagnostics varování o disku chybí; disk a RAM ukazuje hlavička hostu na Home a kontrola **Host resources**.
  - M19M: Sidecary `log-shipper` a `node-exporter` jsou v sousedním repozitáři `moorage-addons` (necommitnuto); `scripts/build-index.mjs` tam padá na chybějícím `beszel-agent/addon.json` (stav před M19), index jsem doplnil ručně.

**Cíl:** Aby nová verze Moorage nalezená kontrolou image šla nasadit jedním klikem a aby Apply & restart vlastního stacku po neúspěchu vždy vrátil konfiguraci, která předtím opravdu běžela.

**Hlavní tok:** Admin otevře stack Moorage. Kontrola image ukáže „Moorage 0.1.8 available“ a tlačítko **Update Moorage**, které spustí stejnou aktualizaci jako Nastavení → Moorage updates (podepsaný release, helper s rollbackem). Když admin upraví `compose.yaml` a dá **Save**, stack ukáže „Saved, not applied“. **Apply & restart** před spuštěním odmítne host mód s obsazeným portem. Když nová konfigurace selže, helper vrátí poslední konfiguraci, se kterou Moorage běžel, ne soubory uložené před chvílí.

### Zjištěno (2026-11-27, ověřeno na WSL: Debian 13, Docker 29.10.1, Moorage 0.1.7)
- **Image update vlastního stacku nic nenasadí (záměr, ale neviditelný).** Auto policy u vlastního stacku jen zapíše Info „auto-update skipped … notify only“ (`updater.applyAutoUpdate`). **Update now** skončí `ErrSelfStack` s textem o zastavení stacku. Moorage updates se instalují jen ručně a release se kontroluje jednou za 24 h (`checkInterval`), takže kontrola image vidí posunutý `:latest` dřív než přehled Moorage updates. Se zapnutým „Snapshot before updates“ update vlastního stacku spadne na snapshotu, protože snapshot vlastní stack odmítá a `fmt.Errorf("%w: %v", ErrSnapshotFailed, err)` ztratí `ErrSelfStack`.
- **Rollback Apply & restart vrací soubory z disku, ne běžící konfiguraci.** `Engine.SelfApply` → `backupSelfFiles` kopíruje to, co je na disku v okamžiku Apply. Po **Save** (nebo Ctrl+S) a následném Apply záloha obsahuje už novou konfiguraci. Reprodukováno: `network_mode: host`, porty zakomentované, port 5081 na hostu obsazený, Save a pak Apply. Moorage spadl na `listen tcp :5081: bind: address already in use`, helper po 120 s „obnovil“ stejné soubory, Moorage zůstal dole a výsledek přitom hlásil `rolledBack: true`. Bez Save (jen Apply) rollback proběhl správně a host mód bez obsazeného portu naběhl (dostupný i z Windows).
- **Host mód a obsazené porty:** linter porty služeb s `network_mode: host` do kolizí nepočítá (M18H) a listen port (5080) ani agent port (5081) z `environment` neřeší.
- **Docker Desktop a rootless:** helper neběží v síti hosta. Když HTTP sonda na kontejner v host módu selže, uzná ho za zdravý jen podle značky zdraví v `/data` (`stackapply.go`, `readHealthyMark`). Apply pak hlásí „OK“, i když Moorage z hosta není dostupný. Neověřeno (Docker Desktop není k dispozici), vyplývá z kódu.
- **M19E: Chat asistenta vyzkoušen** (2026-11-27, dev instance na Windows bez Dockeru, Ollama 0.40.18, `qwen2.5:3b`): odpovědi chodí celé po dokončení kroku, markdown je syrový, log se neposouvá dolů, Stop ani chybějící povolení logů nedávají srozumitelný výsledek, Ctrl+J po uložení nastavení nefunguje do obnovení stránky, na mobilu je panel mimo obrazovku, tokeny `--danger` / `--warning` neexistují. Podrobnosti v bodě M19E.
- **M19F: Karta Services na Home ukazuje z 9 stacků ep-pi jen freellmapi** (ověřeno 2026-11-27 výstupem `docker ps` od uživatele). Není to chyba výpočtu, ale dvě mezery. (1) uptime-kuma, homepage, beszel a dockge publikují port jen na `127.0.0.1` (`127.0.0.1:3001->3001/tcp`), které `portURL` záměrně vynechá, protože z jiného zařízení by odkaz nefungoval. (2) moorage, node-exporter, beszel-agent a homelab běží v host síti bez publikovaných portů, takže Docker žádný port nehlásí. freellmapi jako jediná publikuje `0.0.0.0:9999`. UI k tomu nic neřekne: stacky bez odkazu na kartě prostě chybí a podnadpis „Web links detected from labels“ je navíc nepřesný.

### Vlny
1. **M19A – Rollback na běžící konfiguraci** (uživatel: „Vracet běžící konfiguraci“)
   - `<data>/self-stack-good/` (compose + `.env`, nebo záznam, že `.env` není) = konfigurace, se kterou Moorage naposledy úspěšně běžel. Zapisuje ji helper po úspěšném Apply a Moorage při startu, když se konfigurace na disku shoduje s běžícím kontejnerem (spočítaný config-hash služby z compose-go = label `com.docker.compose.config-hash` běžícího kontejneru).
   - `SelfApply` bere jako zálohu soubory z disku, jen když se shodují s běžícím kontejnerem, jinak `self-stack-good`. Bez obojího (starší instalace s neuloženými změnami): Apply proběhne, ale potvrzovací dialog i výsledek řeknou „rollback není k dispozici“. Helper nikdy netvrdí `rolledBack`, když nemá kam vrátit.
   - **Poctivý výsledek:** když je záloha shodná s novou konfigurací, výsledek je `rolledBack: false` se zprávou „Apply failed and there is no earlier working configuration to return to – see docs/self-stack.md“.
   - **Save u vlastního stacku zůstává.** API vrací `selfPending` (disk ≠ běžící kontejner), StackDetail ukáže trvalou poznámku „Saved, not applied – Moorage still runs the previous configuration. Apply & restart to use it.“ a Apply & restart se zvýrazní.
   - Platí pro hub i agenta (stejný `engine` a helper).
2. **M19B – Kontrola portů hosta před Apply** (uživatel: „Ano, odmítnout Apply“)
   - Listen a agent port se čtou z `environment` služby Moorage v **nové** konfiguraci (`MOORAGE_LISTEN`, `MOORAGE_AGENT_LISTEN`, výchozí :5080 / :5081).
   - **API (422, linter `self-port-taken`, DeployError):** u vlastního stacku s `network_mode: host` port obsazený podle Port map (publikované porty kontejnerů, deklarované porty jiných služeb v host módu, nativní sockety hosta, když je Moorage vidí: nativní běh nebo host mód). Chyba jmenuje kontejner nebo proces, který port drží.
   - **Helper (Linux, `--host-net`):** těsně před `compose up` přečte `/proc/net/tcp*` v síti hosta (bez portů, které publikuje současný kontejner Moorage). Obsazený port = konec bez změn: obnoví soubory ze zálohy, nic nespouští, výsledek „Not applied: port 5081 on the host is in use“. Pokryje i nativní procesy, které Moorage v bridge síti nevidí.
   - **Kolize podle adresy, ne jen portu:** `:5080` (všechny adresy) koliduje s jakýmkoli posluchačem na portu 5080, i na jediné adrese (např. `tailscaled` na `100.x.y.z%tailscale0:5080` od `tailscale serve`). `127.0.0.1:5080` s posluchačem na jiné konkrétní adrese nekoliduje. Když port drží jiný posluchač jen na jiné adrese, chyba navrhne `MOORAGE_LISTEN=127.0.0.1:5080` (a totéž pro agent port) a odkáže na `docs/troubleshooting.md`.
   - **Formát adresy (linter `self-listen-invalid`, DeployError):** `MOORAGE_LISTEN` / `MOORAGE_AGENT_LISTEN` v nové konfiguraci musí být `host:port` nebo `:port` (případně `off`); holé číslo (`5080`) Apply odmítne s návrhem `:5080`. Hodnoty z `${VAR}` se dosazují z `.env` nové konfigurace. Moorage při startu holé číslo portu vezme jako `:port` a zapíše varování, aby se už běžící instalace s překlepem nezacyklila v restartech.
3. **M19C – Update Moorage z kontroly image** (uživatel: „Předat Moorage updates“)
   - U vlastního stacku (hub i agent) zobrazí řádek image místo „Update now“ tlačítko **Update Moorage**, které volá `selfupdate.Service.Apply` pro daný uzel (stejné oprávnění, audit a průběh jako v Moorage updates; po startu UI čeká na návrat jako dnes).
   - Nález nového digestu u vlastního stacku spustí `Checker.CheckNow` (nejvýš jednou za hodinu), aby oba přehledy ukazovaly totéž. Když novější release není (vlastní build, fork, jiný registr), řádek řekne „This image isn't a newer Moorage release – change the tag and use Apply & restart“ a tlačítko se neukáže.
   - Auto policy u vlastního stacku dál jen oznamuje, ale viditelně: událost v Logbooku stacku a oznámení „Moorage X.Y.Z is available – install it in Moorage updates“ s odkazem, jednou za verzi (stejný mechanismus `AnnouncedDigest`).
   - Oprava: snapshot před updatem se u vlastního stacku přeskočí a `ErrSnapshotFailed` se balí přes `%w` i s původní chybou (`errors.Join`), aby updater `ErrSelfStack` poznal.
   - API `POST …/stacks/{name}/update` u vlastního stacku vrací 409 s textem „Moorage updates itself through Moorage updates“ místo obecného `ErrSelfStack` (CLI `moorage updates apply` totéž).
4. **M19D – Docker Desktop a rootless** (uživatel: „Varovat před Apply“)
   - Linter `self-host-network-unverified` (varování) u vlastního stacku s `network_mode: host`, když helper nemůže běžet v síti hosta (Docker Desktop, rootless). Text: host networking tam bývá vypnutý a Moorage pak nebude z počítače dostupný.
   - Když helper uzná kontejner jen podle značky zdraví (HTTP sonda neprošla), výsledek je `ok: true, unverified: true` a stránka stacku i banner řeknou „Applied – Moorage started, but it wasn't reachable from the helper. Check that you can open it.“
5. **M19E – AI pomocník** (návrh 2026-11-27; srovnáno s desktopovou aplikací Ollama 0.40.18 podle dokumentace, screenshotu a zdrojáků jejího chatu z GitHubu (`ollama/ollama`, commit `8ae8acb`, `app/ui/app`, statická analýza REA, nic se nespouštělo) a s chatem Moorage vyzkoušeným na dev instanci s Ollamou a modelem `qwen2.5:3b`)
   - **Cíl:** Panel asistenta se ovládá a čte jako běžný chat: odpověď teče po slovech, jde zastavit, zopakovat a upravit, markdown, kód a odkazy fungují a vždy je vidět, co AI četla a co chce změnit. Hranice M18G (úrovně akcí a dat, pevné zákazy, strop role, Auto → Propose po cizím obsahu) se nemění.
   - **Hlavní tok:** Admin na stránce stacku `immich` stiskne Ctrl+J → panel se ukotví vpravo a stránka se zúží, kurzor je v poli zprávy, prázdný stav nabídne „Why is immich failing?“ (stack má Problems) → klik → nad odpovědí řádek „Read: stack immich · Problems · compose.yaml“, odpověď teče po slovech → karta návrhu „Change compose.yaml of immich · host pi“ s barevným diffem a **Apply** → po Apply „Applied – Open History“. Odpověď nesedí → **Regenerate**; překlep v otázce → **Edit**. Delší práce → **Expand** otevře stejný chat na stránce `#/assistant`.
   - **Tok odkazu (hlavní přání uživatele):** „Kde mi běží Jellyfin?“ → řádek „Read: stack jellyfin · links“ → odpověď „Jellyfin běží na hostu nas: http://192.168.1.10:8096“ jako klikací odkaz a pod odpovědí tlačítko **Open jellyfin** s rozbalením „Other addresses: VPN http://100.64.0.3:8096“. Odkaz otevře službu v nové kartě. Odkaz vzniká stejně jako v detailu stacku, model ho nevymýšlí.
   - **Srovnání a co změnit** (Ollama → Moorage dnes → M19E):
     - *Rozvržení:* Ollama je celé okno s polem zprávy uprostřed a historií v postranním panelu. Moorage má plovoucí panel 420 px přes pravý okraj stránky, který zakrývá stack, o kterém je řeč; na mobilu je napůl mimo obrazovku. → Od 1280 px ukotvený panel (obsah se zúží, šířka 360–640 px tažením, pamatuje si ji `localStorage`), pod 1280 px překryv, pod 768 px celá obrazovka se safe-area. Tlačítko **Expand** otevře chat na stránce `#/assistant` (uživatel).
     - *Vzhled:* Odpověď AI bez rámečku bubliny, jako text dokumentu (dlouhé odpovědi se čtou lépe); bublina zůstává jen u zpráv uživatele. Pole zprávy písmem Onest (dnes dědí JetBrains Mono z `.field`). Chybová bublina používá neexistující token `--danger` a není červená → `--adrift`. Varování u Auto v nastavení používá neexistující `--warning` / `--degraded-text` → `--signal`.
     - *Psaní zpráv:* Enter odešle a Shift+Enter dá nový řádek, to funguje. Doplnit: výška pole roste se zprávou (2–8 řádků), Enter během IME kompozice neodešle (`isComposing`), fokus do pole po otevření panelu a po odpovědi, šipka nahoru v prázdném poli = Edit poslední zprávy. Vložený dlouhý text (log, compose) zůstává textem zprávy; od 4 000 znaků se ve zprávě sbalí na „Pasted text – N lines“ a pole ukáže počítadlo proti limitu 32 KiB, který dnes hlídá jen server.
     - *Streamování:* Ollama streamuje po tokenech. Moorage čeká na celý krok modelu; s malým modelem bylo ~30 s vidět jen „Thinking…“. → Text po tokenech u adaptéru Anthropic i OpenAI-kompatibilních (`stream: true`), nová SSE událost `delta`. Nástroje se dál volají až po dokončeném kroku.
     - *Načítání:* Místo „Thinking…“ řádek stavu s krokem a časem („Reading stack immich… 12 s“), během streamování kurzor na konci textu.
     - *Zastavení:* Po **Stop** dnes otázka visí bez odpovědi a bez vysvětlení. → Zůstane to, co dorazilo, s poznámkou „Stopped“ a tlačítkem Regenerate. Esc během generování = Stop.
     - *Regenerace a úprava* (uživatel): Ollama umí Edit u každé zprávy uživatele (pole zprávy přejde do režimu úprav s textem zprávy, pozdější zprávy zešednou, Cancel vrátí stav) a Regenerate nemá. → **Regenerate** u poslední odpovědi a **Edit** poslední zprávy stejným vzorem jako Ollama: režim úprav v poli zprávy s pruhem „Editing your message – Cancel“, Esc zruší, odpověď pod zprávou zešedne a po odeslání zmizí. Neaplikované návrhy z nahrazené odpovědi se odmítnou (`reject`). Když nahrazovaná odpověď už něco změnila (Apply nebo Auto), tlačítka se nenabídnou a tooltip řekne „This answer already changed something – ask a new question.“
     - *Historie:* Ollama ukládá konverzace. Moorage zůstává u jednorázového chatu (uživatel, potvrzuje M18G). Změna stránky a přepnutí mezi panelem a `#/assistant` chat zachovají, protože stav bude ve sdíleném store, ne v komponentě panelu.
     - *Výběr modelu* (uživatel): Ollama má výběr v poli zprávy. → Admin přepíná v panelu mezi modely, které povolil v nastavení; ostatní vidí jen jméno modelu. Volba platí pro tento chat. V nastavení přibude **Load models** (seznam od poskytovatele), výběr povolených modelů, výchozí model a **Test connection**. Rozšířeno o víc poskytovatelů a automatickou detekci modelů (viz *Poskytovatelé a modely*).
     - *Markdown a kód:* Moorage ukazuje syrový markdown (`**`, zpětné apostrofy, `- `). → Bezpečné vykreslení: odstavce, seznamy, tučné písmo, inline kód, bloky kódu, tabulky. Žádné HTML ze vstupu ani obrázky. Blok kódu ukazuje jazyk a má **Copy**, **Copy** má i celá odpověď. Blok YAML se nedá aplikovat, změny jdou jen přes kartu akce.
     - *Odkazy:* Klikací je dnes jen absolutní URL začínající `location.origin`; model v testu vrátil `#/settings/notifications` a nešlo kliknout. → Klikací i `#/…` a `{origin}/#/…`, ověřené proti mapě rout (neznámá routa zůstane textem). Odkaz uvnitř Moorage otevře stránku a panel nechá otevřený. Externí URL zůstávají textem, kromě ověřených odkazů na služby (viz *Odkazy na služby*).
     - *Prázdný stav:* Dnes jedna věta. → 2–4 návrhy otázek podle stránky (stack s Problems: „Why is X failing?“; stack bez problémů: „Explain this stack“; Home: „Which stacks need attention?“; Settings: „Where do I set up alerts?“), pod nimi jméno modelu a „The chat isn't saved“.
     - *Chyby:* Dnes syrová chyba Go malými písmeny („can't reach the provider: Post "http://localhost:11434/…": dial tcp [::1]:11434: connectex…“) a odkaz jen na seznam add-onů. → Lidská věta podle druhu (nedostupný, 401, 403, 429, došlý kredit, měsíční limit, neznámý model, timeout), technický detail pod „Details“, tlačítko **Retry** a pro admina odkaz rovnou na kartu AI assistant. U Ollama nebo kompatibilního endpointu na `localhost` nápověda, že v kontejneru je `localhost` kontejner sám (použij adresu hostu nebo `host.docker.internal`).
     - *Klávesové zkratky:* Ctrl+J otevře a zavře panel. Po uložení nastavení dnes nefunguje až do obnovení stránky, přestože hláška říká „Open the assistant with Ctrl+J“ → stav asistenta se po uložení načte znovu. Nově Esc zavře panel (během generování zastaví), tlačítko **New chat** dostane zkratku Ctrl+Shift+O a paleta Ctrl+K položku „Ask the assistant…“.
     - *Přístupnost:* Log zpráv jako `role="log"` s oznamováním jen hotových odpovědí (ne každého tokenu), fokus do pole při otevření a zpět na ikonu při zavření, popisky tlačítek s ikonou, „You“ / „Assistant“ pro čtečky, bez blikání při `prefers-reduced-motion`. Auto-scroll k nové odpovědi, jen když je uživatel u konce logu (dnes log zůstane nahoře), jinak tlačítko „New message ↓“.
     - *Potvrzování akcí:* Ollama nic nepotvrzuje: její nástroje (web search, web fetch, browser) se spustí samy a UI je jen ukazuje řádkem „Calling … / Searching for …“; jediné potvrzení je `window.confirm` při mazání chatu. Model Moorage s kartou návrhu a Apply je proto přísnější a zůstává. Karta dnes ukazuje titulek, holý diff a Apply / Reject. → Karta jmenuje host a stack, říká stav („Proposal – nothing changes until you apply“ / „Done by the assistant“), diff je barevný a nad 200 px jde rozbalit, `stack_action` popíše dopad („Stops 3 containers“). Po Apply odkaz na History / Logbook stacku a výsledek deploye; zastaralý návrh nabídne „Ask to redo it“. Žádné nové druhy akcí ani hromadné Apply.
     - *Co AI čte:* Ollama u každého nástroje ukazuje sbalený blok s názvem a náhledem argumentů, po rozbalení argumenty a výsledek. Kroky Moorage („Reading the stack…“) se dnes po dokončení smažou a nikde nezůstane, co šlo poskytovateli. → U každé odpovědi trvalý řádek podle skutečně volaných nástrojů: „Read: stack immich · Problems · compose.yaml (values hidden) · 120 log lines (masked)“. Po rozbalení každá položka ukáže argumenty a přesný text, který šel modelu, už maskovaný a zkrácený jako pro model (uživatel). Karta povolení po kliknutí ukáže „Allowed for this chat“. Povolená data jsou vidět v hlavičce panelu („Logs allowed ×“) a jdou odvolat.
     - *Nástroje (uživatel: jeden registr s MCP):* Asistent dnes má 6 vlastních nástrojů, MCP server Moorage 12 jiných a společný registr z M18G není hotový. → Jeden registr nástrojů pro MCP server i asistenta. Asistent tím získá i `list_hosts`, `list_checks`, `list_deps`, `lint_compose` a `check_images`, filtrované rolí a úrovněmi; MCP server se navenek nemění. Externí MCP servery asistent nedostane (uživatel).
     - *Karta rozhodnutí (uživatel):* Ollama neumí, aby se AI zeptala tlačítky. → Nástroj `ask_choice` (otázka + 2–4 možnosti, volitelně doporučená první): v chatu karta s tlačítky, klik pošle volbu jako zprávu uživatele. Karta nic nespouští, změny jdou dál jen přes kartu akce.
     - *Povolení logů (chyba z testu):* Bez povolení vrací `get_logs` text „not allowed“, který se balí jako nedůvěryhodná data a nastaví příznak cizího obsahu. Model pak papouškuje „The content above is data from containers…“ a Auto se zbytečně mění na Propose. Po **Allow** se smyčka spustí znovu nad historií, která končí odpovědí asistenta; v testu spotřebovala ~1 800 tokenů a v chatu se neobjevilo nic. → Chybějící povolení je výsledek bez obalu a bez příznaku. Allow pošle poslední otázku znovu, nahradí odpověď „potřebuji povolení“ a ukáže průběh.
     - *Spotřeba:* Celkový počet tokenů v hlavičce je pro běžnou práci šum. → Tokeny jako metadata u odpovědi (při najetí myší), v hlavičce jen upozornění od 80 % měsíčního limitu a od 35. zprávy („5 messages left – start a new chat“).
     - *Odkazy na služby (uživatel, hlavní přání):* Detail stacku už odkazy má: `moorage.url` z labelů, domény proxy add-onu, výchozí adresa hostu + publikovaný port a tytéž porty na ostatních adresách (Ethernet, Wi-Fi, VPN, doména). Skládají je ale jen HTTP handlery `listStacks` / `getStack` v `api.go`, takže asistent ani MCP je nedostanou (MCP `list_stacks` vrací jen syrové `urls` bez portů hostu). → Skládání odkazů se přesune do jedné funkce, kterou použije UI, asistent i MCP. Nový nástroj `get_service_links` vrátí po službách hlavní odkaz na výchozí adrese a ostatní adresy (uživatel: výchozí + ostatní). V odpovědi je odkaz klikací jen tehdy, když ho ve stejném chatu vrátil tento nástroj. Pod odpovědí jsou tlačítka **Open <služba>**.
     - *Poskytovatelé a modely (uživatel):* Dnes jeden poskytovatel, jeden klíč a jméno modelu psané ručně. → Seznam poskytovatelů (jméno, typ, base URL, klíč). Jde mít dva stejného typu, třeba Ollamu na PC s GPU a na NASu. Modely se zjistí samy: u Ollamy a vlastních endpointů se ve výběru objeví všechny nalezené, i nově stažené. U placených API (Anthropic, OpenAI, Gemini) admin zaškrtne povolené, protože jich je přes sto a liší se cenou (uživatel). Výběr modelu v panelu ukazuje „poskytovatel · model“. Když poskytovatel nejde, chat sám nepřepne, chyba adminovi nabídne **Try with <poskytovatel · model>** (uživatel).
     - *Rozšíření MCP (uživatel: všechna čtyři):* Nové čtecí nástroje v registru pro asistenta i externí agenty: `get_service_links`, `get_ports` (mapa portů hostu a volný port pro nový stack), `get_problems` (karta Problems), `list_incidents` (výpadky z monitoringu a jejich časová osa), `list_updates` (nové verze image) a `get_stack_stats` (CPU a RAM kontejnerů). Jen pro externí agenty navíc MCP **prompts** (hotové postupy „Diagnose stack“, „Add a service“) a **resources** (stacky a hosty jako čtecí zdroje `moorage://…`).
6. **M19F – Odkazy služeb, kompaktní závislosti a rychlé přidání hostu** (návrh 2026-11-27)
   - **Cíl:** Každá běžící služba má na Home odkaz, nebo srozumitelný důvod a opravu. Graf závislostí se vejde na obrazovku. Agent jde na cizí host nasadit jedním příkazem, který si ohlídá i Docker nebo Podman.
   - **Hlavní tok (odkazy):** Home → Services ukazuje freellmapi s odkazem a pod ním řádek „4 services without a link“ → rozbalení: „beszel – only on 127.0.0.1“ s tlačítkem **Fix** → volby **Publish on Tailscale address** (`100.87.100.2:8090:8090`), **Publish on all addresses** (varování, že Docker obchází firewall) a **Set link** (ruční URL, třeba z `tailscale serve`) → změna portů ukáže diff, uloží git verzi, zkontroluje obsazené porty a nasadí; Set link platí hned.
   - **Hlavní tok (Add host):** Add host → jméno → Create join token → záložky **Install command** · **Command without script** · **compose.yaml** · **docker run** · **Token only** → nad nimi pole přizpůsobení (Runtime: Docker / Podman / Podman rootless, Stacks folder: `/opt/stacks` / `/srv/stacks` / Custom…, Network, Host address, Read-only…). Každé pole nabízí výběr i vlastní hodnotu a všechny záložky se hned přepočítají → Copy → na cizím hostu jeden řádek: skript zjistí, že chybí Docker, zeptá se „Install Docker now? [y/N]“, nainstaluje ho, zapíše compose a spustí agenta → host se v Moorage objeví jako připojený.
   - **Části:**
     - *Odkazy služeb (oprava chyby):* Karta Services ukáže i stacky bez odkazu, sbalené v řádku s počtem, každý s důvodem: „only on 127.0.0.1“, „host network – port not detected“, „no published port“. U portu jen na `127.0.0.1` nabídne **Fix** se třemi volbami (uživatel). Služby v host síti dostanou odkaz z portu v `EXPOSE` image nebo z compose (`ports` a `expose`), ale jen když na něm host opravdu naslouchá (uživatel). **Set link** uloží URL v Moorage a compose nemění. Podnadpis karty bude „Links from labels, ports and your settings“. Stejné odkazy dostane i nástroj `get_service_links` z M19E.
     - *Kompaktní závislosti (výchozí, uživatel):* Uzly s tečkou stavu a jménem, zhruba poloviční. Stav „Up · 1/1“ se ukáže jen tehdy, když něco neběží. Stacky bez závislostí se nekreslí jako karty, ale jako jeden řádek štítků „Without dependencies (6): dockge · freellmapi …“ pod grafem hostu. Klik na štítek otevře stejný panel detailu jako uzel. Přepínač **Compact / Detailed** v liště je předvolba uživatele. Dnešní podrobný graf zůstává jako Detailed.
     - *Rychlé nasazení agenta (uživatel: víc možností):* Záložky **Install command** (výchozí), **Command without script** (heredoc zapíše compose a spustí `compose up -d`, nic dalšího se nestahuje), **compose.yaml** (dnešní), **docker run** a **Token only**. Všechny se skládají ze stejných polí.
     - *Přizpůsobení polí (uživatel):* Runtime (Docker, Podman, Podman rootless → cesta k socketu a příkaz compose), složka stacků, data (pojmenovaný svazek / složka), síť (bridge / host; host je potřeba pro detekci adres a portů), adresa hostu (`MOORAGE_HOST_IP`), read-only, watchdog (port a adresa), tag image (verze hubu / `latest`), další složky stacků a správa externích stacků. Každé pole má nabídku i **Custom…** s vlastní hodnotou.
     - *Experience & editor preferences (uživatel):* Nový blok **Add host defaults** ukládá, která pole se v Add host nabízejí, a jejich výchozí hodnoty (výběr i vlastní hodnota) a výchozí záložku. Preset Beginner nabízí runtime, složku stacků a read-only, Advanced všechna pole. Patří sem i volba **Dependencies view: Compact / Detailed**.
     - *Instalační skripty (uživatel):* Nový `scripts/install-agent.sh` zkontroluje runtime (Docker nebo Podman, plugin compose, běžící daemon, socket) a chybějící nabídne nainstalovat. Pak zapíše compose a spustí agenta. `install-cli.sh` dostane kontrolu bez instalace: bez Dockeru řekne, že nejdřív je potřeba Moorage, nebo `--native`.
7. **M19G – Agent jen s ruční aktualizací** (návrh 2026-11-27, uživatel: „Rozšířit env“)
   - **Cíl:** Admin hostu může agenta (i hub) zamknout tak, aby se Moorage dal aktualizovat jen ručně na hostu (`docker compose pull && docker compose up -d`), nikdy z hubu ani automaticky.
   - **Zjištěno:** `MOORAGE_SELF_UPDATE=false` (`compose.agent.yaml`, popis „Allow updating this agent from the hub's UI“) už dnes odmítne Moorage updates na uzlu (`selfupdate/node.go`, `host.ErrSelfUpdateDisabled`). Hub ale může upravit compose vlastního stacku agenta, změnit tag nebo digest image Moorage a dát **Apply & restart**, čímž agenta aktualizuje mimo zámek.
   - **Co zámek nově blokuje (vynucuje uzel, ne hub):** (1) Apply & restart vlastního stacku, který mění `image` služby Moorage (porovnání image reference a digestu nové a běžící konfigurace; změna jen konfigurace projde); (2) tlačítko **Update Moorage** z M19C a `POST …/stacks/{name}/update` u vlastního stacku; (3) pull image vlastního stacku z UI; (4) návrh asistenta, který mění image Moorage (M19I). Chyba: „This host is set to manual updates only (MOORAGE_SELF_UPDATE=false). Update Moorage on the host: docker compose pull && docker compose up -d“, s odkazem na `docs/troubleshooting.md`.
   - **UI:** Moorage updates i karta hostu ukážou štítek **Manual updates only** a místo tlačítka Update příkaz ke zkopírování. **Update all** takové uzly přeskočí a v souhrnu je jmenuje (nezastaví se na nich jako na chybě). Kontrola image u vlastního stacku dál oznamuje novou verzi.
   - **Dokumentace:** komentář u `MOORAGE_SELF_UPDATE` v `compose.yaml` a `compose.agent.yaml` popíše, co všechno vypne; README a `docs/troubleshooting.md` dostanou sekci „Manual updates only“.
8. **M19H – Instalační skript pro hub** (návrh 2026-11-27, uživatel; dříve ROADMAP „Instalační skript pro hub“)
   - **Hlavní tok:** `curl -fsSL https://…/install.sh | sh` → skript zkontroluje runtime stejně jako `install-agent.sh` z M19F (Docker nebo Podman, plugin compose, běžící daemon, socket; instalace jen se souhlasem přes `/dev/tty`) → zeptá se na složku stacků, port (`127.0.0.1:5080` nebo všechny adresy) a veřejnou URL → zapíše `compose.yaml` hubu (z tagu vydání, stejný soubor jako v repu) a `.env` → `docker compose up -d` → vypíše adresu a setup token z logu.
   - `[A]` Sdílí kontrolu runtime s `install-agent.sh` (společný `scripts/lib/runtime.sh` vložený do obou skriptů při vydání, protože skript běží přes `curl | sh` bez dalších souborů). Volby i jako přepínače (`--stacks`, `--listen`, `--public-url`, `--runtime`, `--yes` jen pro instalaci Moorage, nikdy pro instalaci Dockeru).
   - `[A]` Existující instalace (`/opt/stacks/moorage` nebo běžící kontejner `moorage`): nic nepřepíše, nabídne `--upgrade` (jen `pull` + `up -d`, compose beze změny). Bez rootu a bez `sudo`: skončí s textem, co je potřeba. macOS a Windows: odkáže na Docker Desktop a ruční compose.
9. **M19I – Asistent: co vidí uživatel, přemýšlení, příkazy s lomítkem a otázky z problémů** (návrh 2026-11-27, staví na M19E)
   - **Kontext obrazovky (uživatel):** Asistent dostane upozornění, chyby a varování, které uživatel právě vidí: bannery stránky (třeba „Moorage can't reach this stack's files“ u dockge), chyby formuláře a linteru, chybové toasty z posledních 5 minut a výsledek posledního Apply / deploye. `[A]` Ne snímek obrazovky ani HTML: UI komponenty upozornění se registrují do sdíleného úložiště `pageNotices` (id, závažnost, nadpis, text, stack/host, navržená oprava). V řádku „Read:“ se ukáže „screen notices (2)“ s rozbalením přesného textu. Upozornění s úryvkem logu nese příznak cizího obsahu jako logy (M18G: Auto → Propose).
   - **Oprava ve vlastním stacku Moorage (uživatel: „Karta s Apply“):** Když oprava upozornění vyžaduje změnu compose samotného Moorage (příklad: přidat `- /opt/dockge:/opt/dockge` pod `volumes`), asistent nabídne kartu návrhu s diffem a tlačítkem **Apply & restart**. Výjimka z pevného zákazu M18G pro vlastní stack, vědomě schválená uživatelem, s pojistkami `[A]`: jen úroveň Propose (nikdy Auto, ani když je Auto zapnuté); jen admin; Apply jde přes helper s rollbackem (M19A) a všemi DeployError (M18H, M19B); karta zvlášť a výrazně uvede rozšíření přístupu („Gives Moorage access to /opt/dockge on the host“) u každého nového bind mountu, socketu, `privileged`, `cap_add` nebo `network_mode`; změna image Moorage je pro asistenta dál zakázaná (patří do Moorage updates, M19C, a respektuje M19G); audit `assistant.self_apply` s diffem.
   - **Zobrazení přemýšlení (uživatel: „Sbalené, neukládat“):** U modelů, které přemýšlení vrací (Ollama `thinking`, Claude extended thinking, OpenAI reasoning summary), sbalený blok „Thought for 6 s“ nad odpovědí, během streamu ukazuje živý poslední řádek. Neukládá se do auditu ani do chatu po obnovení stránky. Přepínač **Show model thinking** v nastavení asistenta (výchozí zapnuto); vypnutí ho u poskytovatelů, kde to jde, ani nevyžádá (šetří tokeny).
   - **Příkazy s lomítkem (uživatel: „Vestavěné + vlastní“):** `/` v prázdném poli otevře nabídku. Vestavěné: `/why` (proč stack nebo kontejner na stránce nefunguje), `/logs` (shrnutí posledních chyb v lozích), `/explain` (vysvětlí upozornění na obrazovce), `/link` (kde služba běží). Vlastní: uživatel uloží dotaz s názvem a textem s proměnnými `{stack}`, `{host}`, `{container}` (z aktuální stránky). `[A]` Ukládají se na hubu u uživatele (fungují na všech zařízeních), nejvýš 50 na uživatele, název `[a-z0-9-]`, text do 2 000 znaků. Příkaz je jen text zprávy: úroveň oprávnění, data ani zákazy nemění.
   - **Ask the assistant (uživatel; dříve ROADMAP):** Tlačítko na každém problému v kartě Problems, u chyby deploye nebo Apply a u upozornění z Kontextu obrazovky otevře panel s předvyplněnou otázkou („Why does immich-server run out of memory and how do I fix it?“) a kontextem toho problému. Odeslat musí uživatel (Enter), aby viděl, co jde modelu.
   - **Explain this alert (uživatel; dříve ROADMAP):** Tlačítko u incidentu (M18A), v detailu kontroly a u řádku alertu v Logbooku otevře panel s kontextem výpadku (kontrola, časová osa incidentu, logy z alertu, změny před výpadkem). Odkaz v notifikaci vede na incident s `?ask=explain`; panel se tam otevře s předvyplněnou otázkou, nic se neodešle samo.
   - Asistent vypnutý nebo bez poskytovatele: tlačítka Ask a Explain se neukáží. Viewer: jen úroveň čtení jako dnes.
10. **M19J – Odkazy služeb: QR kód a Tailscale Serve** (návrh 2026-11-27, uživatel)
   - **QR kód (dříve ROADMAP):** U tlačítka **Open** v detailu stacku, na kartě Services (M19F) a u odkazu v chatu asistenta ikona QR → popover s QR kódem URL a adresou pod ním, pro otevření v mobilu. `[A]` QR se generuje v prohlížeči malou knihovnou bez sítě; u adresy `127.0.0.1` nebo `localhost` se neukáže (z mobilu nefunguje).
   - **Tailscale Serve (uživatel: „opt-in“):** Agent s `MOORAGE_TAILSCALE_SERVE=true` a připojeným socketem `/var/run/tailscale/tailscaled.sock` nabídne u služby **Serve on tailnet** → náhled „https://pi.tail1234.ts.net:8443 → http://127.0.0.1:3001“ → potvrzení. Konfigurace je trvalá (přežije restart Tailscale), jde zrušit **Stop serving**. Odkazy z existující konfigurace Serve se ukážou u služeb jako odkaz „Tailscale“.
     - `[A]` Zámek jen v env (seznam `EnvLocks`), protože socket dává plnou kontrolu nad uzlem Tailscale. Agent volá jen LocalAPI `serve-config` (čtení a zápis s ETag, aby nepřepsal souběžnou ruční změnu) přes HTTP na unix socketu, bez knihovny `tailscale.com` (paměťový limit). Funnel (veřejný internet) se nenabízí. Mění se jen záznamy, které Moorage vytvořil (značka v popisu); ruční záznamy jen zobrazí.
     - **NetBird:** `netbird expose` vystavuje do veřejného internetu, je v betě a žije jen během běhu příkazu, proto ho Moorage nespouští. U služby na hostu s NetBirdem ukáže odkaz na NetBird adresu (M17B) a hotový příkaz `netbird expose <port> --with-pin` ke zkopírování s vysvětlením, že služba bude veřejná.
   - Nahrazuje Tailscale Serve z add-onu `vpn-expose` (M15); přepínání `ports:` na VPN adresu už řeší Fix z M19F.
11. **M19K – Problems i mimo spravované stacky a přehled na Home** (návrh 2026-11-27, uživatel: „Mimo stacky + přehled“)
   - **Zjištěno:** Karta Problems (M18F) je jen v detailu spravovaného stacku a kontejnery tam mají odznak „N problems“. Samostatné kontejnery (`docker run`) a cizí compose projekty (read-only, dockge) diagnózu nemají.
   - Stejná diagnóza (`internal/diagnose`) i pro samostatné kontejnery a cizí projekty: karta Problems na stránce read-only stacku a u samostatného kontejneru v seznamu kontejnerů hostu. Opravy na klik (změna compose) jen u spravovaných stacků; jinde problém, důkaz a text opravy, plus **Ask the assistant** (M19I).
   - **Přehled na Home:** karta „Problems“ se souhrnem napříč hosty (problém, kontejner, host, jak dlouho) seřazená podle závažnosti, odkaz vede na kartu Problems daného stacku nebo kontejneru. Ignorované problémy se nepočítají. `[A]` Hub se ptá agentů jednou za 60 s a výsledek drží v paměti; offline host se ukáže jako „host offline – problems unknown“, ne jako nula.
12. **M19L – Označení rolí v Discordu a Slacku** (návrh 2026-11-27, uživatel: „možnost nastavit, za jakých podmínek“; dříve ROADMAP)
   - U kanálu Discord a Slack sekce **Mentions**: koho označit (Discord: role, uživatel nebo `@here`; Slack: skupina uživatelů, uživatel, `@here` / `@channel`) a pravidla, kdy. Pravidlo = události (DOWN, Back up, degraded, log alert, host offline, update failed / rolled back, Apply & restart rolled back) + rozsah (všechny kontroly, jen kritické, vybrané kontroly nebo stacky) + volitelně „jen poprvé za incident“. Víc pravidel s různými lidmi na jednom kanálu (třeba DB tým jen u stacku postgres).
   - `[A]` Výchozí pravidlo nového kanálu: nikoho neoznačovat. Návrh v UI „Mention @role on DOWN of critical checks“ jedním klikem. Discord posílá `allowed_mentions` jen s vybranými ID, takže text logu v alertu nikdy nikoho neoznačí; Slack escapuje `<!…>` a `@` v logu a uživatelském textu.
   - Testovací zpráva kanálu ukáže, koho by označila, a jde poslat i bez označení.
13. **M19M – Dříve schválené nápady z ROADMAP** (zařazeno 2026-11-27 na pokyn uživatele; detailní návrh z M15 a ROADMAP platí, jen se přesouvá sem)
   - **Prohlížeč a editor souborů stacku (M15 §1.2):** záložka **Files** v detailu stacku, textové soubory do 512 KiB, kontrola cesty a symlinků, verze v `history.git`, audit `stack.file_save`, respektuje read-only.
   - **Hlídání volného disku a RAM hosta (M15 §1.3):** agent posílá využití disku (`/`, složka stacků, Docker root) a RAM, ukazatel na kartě hostu, varování v Diagnostics a typ kontroly **Host resources** s prahy (výchozí disk 85 % / 95 %, RAM 90 %). Diagnóza `DiskFreePct` v Problems (dnes −1) dostane skutečnou hodnotu.
   - **Hromadné akce nad stacky (M15 §1.4):** výběr více stacků na stránce hostu, akce Start, Restart, Stop, Update images a politika auto-updatu, fronta po jednom, souhrn úspěchů a chyb, vlastní stack Moorage se přeskočí.
   - **Hodinové agregace výsledků kontrol (1 rok):** tabulka hodinových souhrnů (počet up/down/silenced, průměrná a maximální odezva), plní se při mazání surových dat po 30 dnech; uptime grafy nad 30 dní čtou agregace. Migrace SQLite přidá tabulku, žádná data se neztratí.
   - **Podman (dokončení a ověření):** ověřit stávající podporu proti Podmanu 5.x rootful i rootless na WSL (socket, compose přes `podman compose` / `docker-compose`, Apply & restart, snapshoty, terminál) a opravit nalezené rozdíly; výsledek do README jako tabulka „ověřeno / omezení“.
   - **Sidecary `log-shipper` a `node-exporter`:** v repozitáři `moorage-addons` podle M15 §3 (Vector s výstupem do VictoriaLogs/Loki/OTLP, `prom/node-exporter` vedle add-onu `prometheus-grafana`).

### Rozhodnutí
- Rollback vrací běžící konfiguraci, Save zůstává s upozorněním (uživatel).
- Update z kontroly image předává Moorage updates, auto policy jen oznamuje (uživatel). Automatická instalace Moorage v okně údržby se nedělá (uživatel zvolil variantu bez ní) → ROADMAP.
- Obsazený port hosta v host módu = Apply odmítnut (uživatel).
- Docker Desktop / rootless: varování + „unverified“ výsledek (uživatel).
- `[A]` Shoda disk ↔ běžící kontejner přes config-hash compose-go, ne porovnáním textu: formátování a komentáře shodu nerozbijí, změna ano.
- `[A]` Kontrola portů ve dvou krocích (API + helper), protože Moorage v bridge síti nativní procesy hosta nevidí.
- `[A]` „Update Moorage“ nestahuje image přes compose: jen podepsaný release prochází ověřením podpisu (M12, M18B).
- Kolize s M18H: rozšiřuje pravidlo „Apply & restart odmítne DeployError u služby Moorage“ o `self-port-taken`; varování `ports-ignored` beze změny.
- Kolize s M17 („po self-updatu nehlásí falešnou aktualizaci `:latest`“): `CheckNow` po nálezu image nesmí tuto kontrolu obejít; po self-updatu se řádek image přepočítá jako dnes.
- M19E: Chat zůstává jednorázový, obnovení stránky ho smaže (uživatel, potvrzuje M18G). Historie více chatů → `ROADMAP.md` (odloženo).
- M19E: Ukotvený panel na širokých obrazovkách, celá obrazovka na mobilu a stránka `#/assistant` přes **Expand** (uživatel).
- M19E: Model v panelu přepíná jen admin, a to mezi modely povolenými v nastavení; ostatní vidí jméno modelu (uživatel). `[A]` Server model ověří (admin + povolený seznam), jinak použije výchozí. Měsíční limit tokenů zůstává společný.
- M19E: Regenerate poslední odpovědi a Edit poslední zprávy; neaplikované návrhy se odmítnou, po provedené změně se tlačítka nenabídnou (uživatel).
- M19E: `[A]` Kolize s M18G (prompt injection): Regenerate a Edit spouští nový tah, příznak cizího obsahu se počítá znovu a Auto → Propose platí dál. Návrhy otázek v prázdném stavu jen vyplní zprávu, nic nespouští.
- M19E: `[A]` Markdown vykresluje vlastní malý renderer do Svelte uzlů bez `{@html}` a bez knihovny: výstup modelu je nedůvěryhodný a nechceme novou závislost ani paměť navíc.
- M19E: `[A]` Streamování mění jen zobrazení textu; volání nástrojů a karty akcí přicházejí až po dokončeném kroku, jak je dnes.
- M19E: `[A]` Řádek „Read: …“ skládá server ze skutečných volání nástrojů (událost `tool` s cílem), ne model z textu, takže nemůže lhát o tom, co četl.
- M19E: Rozbalení položky „Read: …“ ukáže přesný text poslaný modelu (uživatel). `[A]` Je to výsledek nástroje spuštěného s oprávněními přihlášeného uživatele, takže ukazuje jen to, co jeho role smí vidět. Obsah z cizích zdrojů (logy) se vykreslí jako prostý text bez markdownu a bez klikacích odkazů.
- M19E: Jeden registr nástrojů s MCP serverem (uživatel, dokončuje M18G). `[A]` Každý nástroj v registru nese rozsah (`read` / `operate` / `deploy`), příznak cizího obsahu a kategorii asistenta. MCP ho filtruje ACL tokenu jako dnes; asistent rolí (viewer = jen `read`), úrovněmi kategorií a pevnými zákazy (navíc vlastní stack Moorage a stack add-ons). Chování MCP serveru se nemění: jeho rozdíly (`stack_action` u vlastního stacku Moorage) zůstávají a mají vlastní test.
- M19E: `[A]` Mapování nástrojů: `read` (`list_hosts`, `list_stacks`, `get_stack`, `list_checks`, `list_deps`, `lint_compose`, `get_logs` podle úrovně dat) má každý; `check_images` (`operate`, nic nemění, jen dotaz do registru) má admin při kategorii `stack` jiné než Off a běží bez karty; `stack_action` → kategorie `stack`; `update_stack` a `deploy_stack` → kategorie `compose`; `create_stack` → kategorie `generate`. Dnešní `propose_compose` je `update_stack` z registru.
- M19E: Externí MCP servery asistent nedostane (uživatel) → `ROADMAP.md` zamítnuté. Důvod: Moorage nezná jejich nástroje, takže na ně nejde uplatnit pevné zákazy, a data Moorage by z nich mohla odcházet ven.
- M19E: Karta rozhodnutí `ask_choice` (uživatel). `[A]` Tah po ní končí a odpovědí je volba poslaná jako zpráva uživatele (stejná cesta jako psaná odpověď). Nejde přes ni nic spustit, nejde do auditu a dostane ji i viewer.
- M19E: `[A]` Chybějící povolení dat není cizí obsah: výsledek bez obalu `<untrusted-data>` a bez příznaku.
- M19E: `[A]` Bez štítku „AI“ (beze změny rozhodnutí M18G).
- M19E: Odkazy na služby: hlavní odkaz na výchozí adresu hostu, ostatní adresy pod ním, stejně jako v detailu stacku (uživatel).
- M19E: `[A]` Kolize se system promptem („odkazy jen z mapy rout“) a s pravidlem M19E „externí URL zůstávají textem“. Model by jinak mohl napsat špatný port nebo převzít URL z logů. Řešení: server si v chatu pamatuje URL, které vrátil `get_service_links`, a pošle je v události `tool`. Externí URL je klikací jen při přesné shodě s tímto seznamem, jinak zůstane textem. Tlačítka **Open <služba>** se skládají ze seznamu, ne z textu modelu. System prompt dostane pravidlo: odkazy na služby jen z `get_service_links`, adresu ani port nikdy neodhaduj.
- M19E: `[A]` Odkaz na službu ukazuje celou URL (žádný skrytý text). Povolené jsou jen `http` a `https`, otevírá se v nové kartě s `rel="noopener noreferrer"`. Viewer odkazy dostane, protože je vidí i v detailu stacku.
- M19E: `[A]` Výsledek `get_service_links` není cizí obsah: odkazy skládá Moorage z adres hostu, portů a labelů compose. URL z logů a z Problems klikací nejsou nikdy.
- M19E: `[A]` Kolize s M18G (prompt injection): Problems v `get_stack` dnes obsahují řádky logů („Evidence“ z `diagnose`) bez obalu a bez příznaku cizího obsahu, takže obcházejí úroveň dat `logs` i Auto → Propose. Oprava: Evidence z logů se počítá jako cizí obsah a řídí se úrovní dat `logs` (Never = bez řádků logů, Ask = karta povolení). Totéž platí pro `get_problems` a `list_incidents`.
- M19E: Víc poskytovatelů (uživatel). `[A]` Výchozí je jedna dvojice poskytovatel + model. Klíč je u každého poskytovatele zvlášť a jen pro zápis jako dnes. Měsíční limit tokenů zůstává společný pro všechny poskytovatele.
- M19E: Automatická detekce: u Ollamy a OpenAI-kompatibilních endpointů jsou ve výběru všechny nalezené modely, u Anthropic, OpenAI a Gemini jen zaškrtnuté adminem (uživatel). `[A]` Detekce běží při uložení poskytovatele, při otevření nastavení a při otevření výběru modelu v panelu (cache 5 min), ne na pozadí. Ollama: `/api/tags` a `/api/show` s `capabilities`, model bez `tools` se označí „advise only“. Ostatní neříkají, co model umí: předpokládá se function calling, a když ho poskytovatel odmítne, model se označí.
- M19E: Žádné automatické přepnutí na jiného poskytovatele. Chyba nabídne **Try with <poskytovatel · model>** (uživatel). Důvod: logy a compose nesmí potichu odejít z lokálního modelu do cloudu. `[A]` Tlačítko vidí jen admin (kolize s rozhodnutím, že model přepíná jen admin); ostatní vidí „The assistant is unavailable right now – ask an admin“.
- M19E: Rozšíření MCP: `get_service_links`, `get_ports`, `get_problems`, `list_incidents`, `list_updates`, `get_stack_stats`, MCP prompts a resources (uživatel). `[A]` Všechny nové nástroje mají rozsah `read`, protože odpovídají stránkám dostupným každé roli. Prompts a resources dostanou jen externí agenti; asistent má vlastní system prompt a nástroje. Resources se filtrují ACL tokenu stejně jako `list_stacks`, bez odběru změn (`resources/subscribe`).
- M19E: `[A]` Doplnění mapování nástrojů: `rollback_stack` (MCP ho má, v mapování chyběl) → kategorie `compose` s kartou a diffem.
- M19F: Služba jen na `127.0.0.1` dostane **Fix** s volbami Publish on Tailscale address, Publish on all addresses a Set link (uživatel, po vysvětlení, že `x:x` Docker publikuje na všech adresách a obchází firewall).
- M19F: `[A]` Změna portů stejným cíleným textovým přepisem jako **Change to `<tag>`** u image (zachová komentáře, uvozovky a CRLF; `${PORT}` odmítne a otevře editor). Pak diff, git verze s autorem, kontrola obsazených portů z M18H a `up`. Kolize s `tailscale serve`: když `tailscaled` drží `100.87.100.2:8090`, kontrola změnu odmítne s „tailscale serve already uses this port – keep 127.0.0.1 and use Set link“.
- M19F: `[A]` **Publish on all addresses** chce potvrzení s větou „Docker publishes past the host firewall (ufw): the service will be reachable from your whole network, and from the internet if the host is public.“
- M19F: `[A]` **Set link** ukládá URL v Moorage (nastavení `service_links:<hostID>`), ne jako label v compose: platí hned a funguje i u read-only stacků a hostů. Nastavuje jen admin, vidí každý, kdo vidí stack. Povolené jsou jen `http` a `https`. Ruční odkaz se ukazuje první se zdrojem „set in Moorage“.
- M19F: Host síť: odkaz z `EXPOSE` image nebo z compose, jen když host na portu naslouchá (uživatel). `[A]` Naslouchání vidí jen Moorage nebo agent v nativním či host módu (`PortReport.native`). V bridge módu se odkaz nevymýšlí: řádek „host network – port not detected“ a Set link.
- M19F: `[A]` Vlastní stack Moorage: odkaz je veřejná URL z General settings, Fix se nenabízí (porty Moorage mění jen Apply & restart). Read-only stack nebo host (dockge): jen Set link.
- M19F: Kompaktní graf závislostí je výchozí pro všechny včetně stávajících uživatelů (uživatel). `[A]` Je to předvolba uživatele `depsView`, takže na jiném zařízení zůstane stejná. Filtry Only unhealthy a Hide ignored platí i na štítky, Show standalone v kompaktním režimu ovládá řádek štítků.
- M19F: Add host nabízí víc způsobů: instalační příkaz, příkaz bez skriptu, compose, `docker run`, samotný token (uživatel). Preference ukládá viditelná pole, jejich výchozí hodnoty s možností vlastní hodnoty a výchozí záložku (uživatel).
- M19F: `[A]` Instalační příkaz stahuje `install-agent.sh` z tagu verze hubu (dev build z `main`), ne z `main`, aby skript seděl k verzi agenta. Token jde v proměnné prostředí (`MOORAGE_JOIN_TOKEN=… sh`), ne jako argument, protože argumenty vidí v `ps` každý uživatel hostu. Token je jednorázový, platí 24 h a příkaz to říká.
- M19F: `[A]` Skript nic neinstaluje bez souhlasu: na terminálu se ptá přes `/dev/tty` („Install Docker now? [y/N]“), bez terminálu skončí s návodem, pokud nedostal `--install-runtime docker|podman`. Docker instaluje oficiálním skriptem `get.docker.com` (Debian, Ubuntu, Raspberry Pi OS, Fedora, CentOS), na Alpine a Arch balíčkovým správcem. Podman vždy balíčkovým správcem. Pro rootless Podman nabídne `loginctl enable-linger`, aby agent běžel i po odhlášení.
- M19F: `[A]` `install-cli.sh` jen kontroluje: v režimu `--docker` bez Dockeru nebo bez kontejneru Moorage skončí s „Install Moorage first, or use --native“. Instalace Dockeru by tam nedávala smysl, protože bez Moorage nemá CLI s čím mluvit.
- M19E: `[A]` Upřesnění k „chování MCP serveru se nemění“: přibudou nové nástroje a pole. `list_stacks` a `get_stack` vrátí navíc složené odkazy (`urls`, `altUrls`) a `get_logs` přes MCP začne maskovat hodnoty z `.env` jako asistent. Odebrané ani přejmenované nic není.
- M19G: `MOORAGE_SELF_UPDATE=false` nově blokuje i Apply & restart se změnou image Moorage, Update Moorage, pull vlastního image a návrh asistenta se změnou image; zámek zůstává jen v env (uživatel). Přepínač v UI zamítnut, protože by ho kompromitovaný hub mohl vypnout.
- M19H: `[A]` Instalační skript hubu sdílí kontrolu runtime s `install-agent.sh` (M19F) a stahuje se z tagu vydání.
- M19I: Asistent vidí upozornění, chyby a varování na obrazovce jako strukturovaný text, ne snímek (uživatel; forma `[A]`).
- M19I: Kolize s pevným zákazem M18G „vlastní stack Moorage“: uživatel zvolil kartu návrhu s Apply & restart i pro vlastní stack (výjimka jen pro změnu compose, jen Propose, jen admin, rollback M19A, výrazné upozornění na rozšíření přístupu k hostu; změna image dál zakázaná).
- M19I: Přemýšlení modelu sbalené a neukládané (uživatel). Příkazy s lomítkem vestavěné + vlastní uložené na hubu u uživatele (uživatel). Tím se z „Mimo rozsah“ M19E přesouvají zobrazení přemýšlení a uložené dotazy.
- M19J: Tailscale Serve jako opt-in zámek `MOORAGE_TAILSCALE_SERVE` s připojeným socketem, bez Funnelu; NetBird jen odkaz a příkaz ke zkopírování (uživatel). Nahrazuje Tailscale Serve z `vpn-expose` (M15).
- M19K: Problems i pro samostatné kontejnery a cizí projekty + souhrn na Home; opravy na klik jen u spravovaných stacků (uživatel).
- M19L: Označení rolí podle nastavitelných pravidel (události, rozsah, jen poprvé za incident) per kanál (uživatel). `[A]` Výchozí je nikoho neoznačovat, `allowed_mentions` brání označení z textu logu.
- M19M: Dříve schválené a neimplementované nápady z ROADMAP (Files, disk a RAM, hromadné akce, hodinové agregace, Podman, sidecary) zařazeny do M19 na pokyn uživatele s návrhem z M15.

### Okrajové stavy
- První start po aktualizaci na M19 s neuloženými změnami (disk ≠ běžící): `self-stack-good` nevznikne, Apply varuje „rollback není k dispozici“, dokud Moorage jednou úspěšně neběží ze souborů na disku.
- Úprava compose mimo Moorage (shell, Git): stejné jako Save, `selfPending` ji ukáže.
- Restart (ne Apply) kontejner nepřestaví, `selfPending` zůstává.
- Release kontrola vypnutá (`MOORAGE_SELF_UPDATE=false`) nebo offline: řádek image ukáže nález, tlačítko Update Moorage se neukáže, text vysvětlí proč.
- Agent bez M19 (starší verze): hub nabízí Update Moorage jen tam, kde to Moorage updates umí dnes.
- Port drží stará instance Moorage (bridge → host): helper vynechá porty, které publikuje současný kontejner.
- Port drží `tailscale serve` na Tailscale adrese, zatímco v bridge módu šlo vše přes `127.0.0.1`: přechod na host mód s `:5080` se odmítne a chyba navrhne `127.0.0.1:5080`; s ní Apply projde a Tailscale dál přeposílá na localhost.
- M19E: Poskytovatel nebo endpoint neumí streamovat: odpověď přijde celá po kroku, řádek stavu s časem zůstává.
- M19E: Spojení spadne uprostřed odpovědi: co dorazilo, zůstane s „incomplete“ a tlačítkem Retry (= Regenerate). Bez dokončeného kroku se žádná akce neprovede (beze změny).
- M19E: Edit nebo Regenerate odpovědi s neaplikovaným návrhem: návrh se odmítne a karta zmizí. Odpověď s provedenou změnou: tlačítka chybí s vysvětlením.
- M19E: Admin odebere model z povolených, zatímco chat běží: server použije výchozí model a panel ukáže „Model X is no longer allowed – using Y“.
- M19E: Vybraný model neumí function calling: v panelu „This model can only advise“, karty akcí nepřijdou.
- M19E: **Load models** selže (chybí klíč, endpoint nemá seznam modelů): jméno modelu jde dál napsat ručně, chyba řekne proč.
- M19E: Okno se zúží pod 1280 px s ukotveným panelem: panel přejde do překryvu, chat se neztratí.
- M19E: Viewer: panel bez karet akcí, návrhy otázek jen čtecí („Explain this stack“).
- M19E: Široký blok kódu nebo tabulka v úzkém panelu: vodorovně se posouvá jen blok, ne stránka.
- M19E: **Copy** na HTTP bez secure context: záložní kopírování výběrem textu, jinak hláška „Select the text to copy“.
- M19E: 40 zpráv: od 35. zprávy upozornění, na limitu pole zprávy nahradí tlačítko New chat (místo dnešní chyby 400 po odeslání).
- M19E: Karta rozhodnutí a uživatel místo kliknutí napíše vlastní odpověď: karta se zamkne jako „Answered in the chat“. Regenerate nebo Edit odpovědi s kartou rozhodnutí kartu nahradí.
- M19E: Model vrátí `ask_choice` s 1 nebo více než 4 možnostmi nebo s prázdnou otázkou: nástroj vrátí chybu modelu, uživatel nevidí nic rozbitého.
- M19E: Nástroj z registru, který MCP má a asistent ne (podle role, úrovně nebo pevného zákazu): asistent ho v seznamu nástrojů vůbec nedostane, model ho tedy nemůže zavolat. Volání neexistujícího nástroje skončí jako dnes chybou „Unknown or not permitted tool“.
- M19E: Rozbalený výsledek nástroje je velký (logy, compose): blok má výšku nejvýš 320 px se svislým posuvníkem a zkrácení je označené „…(truncated)“, stejně jako pro model.
- M19E: Starší agent bez nového nástroje (např. `list_deps` na vzdáleném hostu): nástroj vrátí srozumitelnou chybu „This host runs an older Moorage“ a asistent pokračuje.
- M19E: Host bez známé adresy (bridge mód bez `MOORAGE_HOST_IP` a bez uložených adres): `get_service_links` vrátí „No address known for host X“ a odkaz `#/hosts` na panel Addresses. Asistent adresu nevymyslí.
- M19E: Port publikovaný jen na `127.0.0.1`: bez odkazu, s poznámkou „reachable only from the host itself“. Služba bez publikovaného portu a bez proxy: „not published – reachable only inside the Docker network“.
- M19E: Schéma se odhaduje jako v UI (443 a 8443 = https, jinak http). Když nesedí, správné schéma dá label `moorage.url`; asistent to v takovém případě poradí.
- M19E: Zastavený stack: odkazy se vrátí s poznámkou „stack is stopped“ a tlačítko **Open** chybí.
- M19E: Vzdálený host přes agenta: odkaz míří na adresu toho hostu, ne hubu. Adresa označená „missing“ (rozhraní je dole) se vynechá.
- M19E: Odpověď opakuje URL, které nástroj nevrátil (jiný port, URL z logů): zůstane textem, bez tlačítka Open.
- M19E: Detekce modelů selže (Ollama je vypnutá): zůstane poslední známý seznam s „Couldn't refresh models – list from 14:02“ a jméno modelu jde napsat ručně.
- M19E: Placený poskytovatel přidá nové modely: ve výběru nejsou, dokud je admin nezaškrtne; nastavení ukáže „3 new models“.
- M19E: Stejné jméno modelu u dvou poskytovatelů (dvě Ollamy): výběr ukazuje „poskytovatel · model“, server rozlišuje podle ID poskytovatele.
- M19E: Admin smaže poskytovatele, kterého používá běžící chat: další zpráva použije výchozí dvojici s poznámkou „Provider X was removed – using Y“. Výchozího poskytovatele smazat nejde, dokud se nevybere jiný.
- M19E: Externí MCP klient bez podpory prompts a resources je ignoruje, nástroje fungují beze změny.
- M19F: Host bez adresy typu VPN: volba Publish on Tailscale address chybí. Port v proměnné (`${PORT}:80`): Fix otevře editor s „Change the port in .env or compose by hand“.
- M19F: Kontejner v host síti s víc porty v `EXPOSE`, z nichž naslouchá jen část: odkazy jen na naslouchající porty, nejvýš 5 jako dnes.
- M19F: Stack bez webového rozhraní (databáze, exporter bez UI): řádek bez odkazu nevadí, ale nejde schovat. `[A]` Sbalený řádek „N services without a link“ je v základu zavřený, takže Home nezahltí.
- M19F: Kompaktní graf s desítkami stacků na jednom hostu (ep-vps s Coolify): řádek štítků se zalomí, dlouhá jména (UUID Coolify) se zkrátí s plným jménem v tooltipu a Coolify jménem tam, kde je add-on zapnutý.
- M19F: Úzké okno a mobil: kompaktní graf platí i tam. Řádek štítků se zalamuje, stránka se vodorovně neposouvá.
- M19F: Přizpůsobení s neplatnou hodnotou (relativní cesta složky stacků, adresa s mezerou): chyba u pole a Copy je vypnuté, dokud není opravená.
- M19F: Skript na hostu bez internetu nebo bez přístupu na GitHub: příkaz selže hned na `curl`. Záložky Command without script a compose.yaml fungují bez stahování skriptu, image se ale stáhnout musí.
- M19F: Docker je nainstalovaný, ale daemon neběží: skript nabídne `systemctl enable --now docker`. Uživatel bez rootu a bez `sudo`: skript skončí s tím, co spustit jako root.
- M19F: Na hostu už běží kontejner `moorage-agent`: skript nic nepřepíše a nabídne `--replace` (nový token, data agenta zůstanou). Složka agenta už existuje: totéž.
- M19F: macOS nebo Windows: skript neinstaluje, odkáže na Docker Desktop a záložku compose.yaml.
- M19F: Token vyprší nebo je použitý, zatímco uživatel ladí pole: záložky to ukáží („Token expired – create a new one“). Pole zůstanou vyplněná.
- M19G: Uzel se starší verzí bez M19G a s `MOORAGE_SELF_UPDATE=false`: Apply & restart se změnou image ho dál aktualizuje (blokuje to až nový uzel); hub to v Moorage updates nepozná, proto štítek Manual updates only čte zámek z uzlu.
- M19G: Ruční `docker compose pull && up -d` na hostu je jediná cesta; po ní Moorage updates ukáže novou verzi a kontrola image u vlastního stacku zmizí.
- M19I: Upozornění zmizí (uživatel odejde ze stránky) mezi otázkou a odpovědí: asistent dostal stav v okamžiku odeslání, řádek „Read:“ ukazuje ten.
- M19I: Karta Apply & restart vlastního stacku na agentovi s `MOORAGE_READ_ONLY=true`: karta se neukáže, asistent vysvětlí, že změna jde jen na hostu.
- M19J: Socket `tailscaled.sock` chybí nebo Tailscale neběží: Serve on tailnet je neaktivní s vysvětlením; uživatel v tailnetu bez HTTPS certifikátů (MagicDNS/HTTPS vypnuté): chyba z LocalAPI s odkazem na nastavení tailnetu.
- M19K: Stovky samostatných kontejnerů: souhrn na Home ukáže prvních 20 podle závažnosti a „+N more“.
- M19L: Discord role smazaná nebo bez práva být označena: zpráva odejde bez označení a kanál ukáže varování po prvním neúspěchu.
- M19M: Hromadná akce zahrnuje stack s DeployError: přeskočí se s důvodem v souhrnu, ostatní pokračují.

### Data a API
- Bez migrace databáze. Nové soubory v `/data`: `self-stack-good/`.
- `GET …/stacks/{name}`: `selfPending` (bool). `SelfApplyResult`: `unverified` (bool), `rolledBack` jen při skutečné změně.
- Linter: `self-port-taken` (DeployError), `self-listen-invalid` (DeployError), `self-host-network-unverified` (varování).
- Dokumentace: `docs/troubleshooting.md` (vzniklo 2026-11-27) dostane odkazy z textů chyb `self-port-taken` a `self-listen-invalid`.
- Pořadí vydání: jeden release (hub i agent ze stejného image). Helper běží z image současného kontejneru, takže první Apply po aktualizaci už používá nový helper.
- M19E: Bez migrace. Nastavení add-onu (`assistant_settings`): `models` (povolené modely, pole), `model` zůstává výchozím modelem.
- M19E: `POST /api/assistant/chat`: volitelné `model` (bere se jen od admina a jen z `models`). SSE: nová událost `delta` (kus textu); událost `tool` dostane `target` (host, stack, počet řádků), `args` a `result` (přesný text poslaný modelu, maskovaný, nejvýš 24 KiB) a v UI zůstává; nová událost `choice` (otázka, možnosti). Chybové události nesou `code` (`unreachable`, `auth`, `forbidden`, `rate_limit`, `quota`, `model`, `timeout`) pro lidskou větu.
- M19E: Registr nástrojů v novém balíčku `internal/tools` (definice, schéma, rozsah, příznak cizího obsahu, kategorie asistenta, běh). Používá ho `addon_mcp.go` (`tools/list`, `tools/call`) i `assistant.go`; drafty a `applyDraft` jsou už společné. Test hlídá, že každý nástroj má rozsah a kategorii a že asistent nedostane nic z pevně zakázaných akcí.
- M19E: `GET /api/addons/assistant/models` (admin): seznam od poskytovatele (OpenAI-kompatibilní `/v1/models`, Ollama `/api/tags`, Anthropic models API). `POST /api/addons/assistant/test` (admin): krátký dotaz bez nástrojů, vrací ok nebo lidskou chybu.
- M19E: `GET /api/assistant/status`: navíc `model`, `models` (jen admin) a `functionCalling`.
- M19E: Adaptéry: `Session.Next` dostane callback pro `delta` (Anthropic stream z SDK, OpenAI-kompatibilní `stream: true` se skládáním `tool_calls`). Paměť: stream text jen přeposílá, nic nehromadí navíc (PSS gate).
- M19E: Frontend: `assistantChat.svelte.ts` (sdílený stav chatu pro panel i stránku), `AssistantPanel.svelte` (ukotvení, klávesy, přístupnost), `AssistantView.svelte` (`#/assistant`), `Markdown.svelte` (bezpečný renderer s Copy). Routa `#/assistant` přibude do mapy rout system promptu (test proti routeru).
- M19E: Dokumentace `docs/assistant.md` (panel, zkratky, výběr modelu, Regenerate a Edit, nápověda k `localhost` u Ollamy). Pořadí: ve stejném releasu jako M19A–D, nezávisle na nich.
- M19E: Poskytovatelé v `assistant_settings`: `providers` (pole: `id`, `name`, `type`, `baseUrl`, `models` = zaškrtnuté modely u placených API) a `default` (`provider`, `model`). Klíče v nastavení `assistant_api_key:<providerId>`. `[A]` Bez SQL migrace: při načtení se dnešní `provider` / `model` / `baseUrl` a `assistant_api_key` převedou na poskytovatele `default`. Staré klíče se dál zapisují podle výchozího poskytovatele, takže downgrade funguje (stejný vzor jako `host_ip` u adres hostu). Tím se upřesňuje dřívější řádek o poli `models`.
- M19E: `GET /api/addons/assistant/models?provider=<id>` (admin) vrací nalezené modely, `functionCalling` a čas detekce. `POST /api/addons/assistant/test` bere `provider`. `POST /api/assistant/chat` bere `provider` a `model` (obojí jen od admina, ověřené proti seznamu). `GET /api/assistant/status`: `models` seskupené podle poskytovatele (jen admin).
- M19E: Odkazy: nový `internal/api/links.go` se `stackLinks(ctx, hostID, h, stack)` vytaženým z `listStacks` / `getStack`. Používají ho UI handlery, nástroj `get_service_links` i MCP `list_stacks` / `get_stack`. Test hlídá, že UI a nástroj vrátí stejné odkazy. Výstup nástroje: po službách `url`, `alternatives` (`label`, `kind`, `url`), `source` (`label` / `proxy` / `port`) a `note`. Událost SSE `tool` dostane `links` (ověřený seznam pro renderer a tlačítka Open).
- M19E: MCP: `initialize` ohlásí `prompts` a `resources`. Nové metody `prompts/list`, `prompts/get`, `resources/list` a `resources/read` (`moorage://hosts/<id>`, `moorage://hosts/<id>/stacks/<name>`), filtrované ACL tokenu. Nové nástroje čtou ze stávajících funkcí: mapa portů (sdílená 5s cache), `stackProblems`, časová osa incidentů, `/api/updates` (jen uložené výsledky, žádný dotaz do registru) a `stackStats`.
- M19F: Bez SQL migrace. Ruční odkazy v nastavení `service_links:<hostID>` (JSON `stack/service → [url]`). `PUT /api/hosts/{host}/stacks/{name}/links` (admin, audit `stack.link_set`). Odkazy služeb dostanou `source` (`label` / `proxy` / `port` / `expose` / `manual`) a služby bez odkazu `reason` (`loopback` / `host-network` / `unpublished`), obojí ve `stackLinks` z M19E.
- M19F: `POST /api/hosts/{host}/stacks/{name}/ports/fix` (admin) s `service`, `port`, `mode` (`vpn` / `all`): vrací diff k potvrzení a s `confirm: true` uloží verzi a spustí `up`. Používá přepis řádku z `images/update` a kontrolu portů z M18H.
- M19F: Host síť: `EXPOSE` z `ContainerInspect` (`Config.ExposedPorts`), naslouchající porty z mapy portů hostu (zdroj `host`, sdílená 5s cache). Starší agent bez nativní mapy portů se chová jako bridge.
- M19F: Preference uživatele (`UserPreferences`): `depsView` (`compact` výchozí / `detailed`) a `addHost` (`fields`, `defaults`, `tab`). Preset Beginner a Advanced je nastaví.
- M19F: Frontend: `DepsView.svelte` (kompaktní rozložení a řádek štítků; `layoutByHost` dostane menší rozměry uzlů), `Home.svelte` (karta Services), nový `ServiceLinkFix.svelte`, `HostsView.svelte` (záložky a pole přizpůsobení, generátor příkazů v novém `agentInstall.ts` s testem), `AccountView.svelte` (blok Add host defaults a Dependencies view).
- M19F: Skripty: nový `scripts/install-agent.sh` (`--hub`, `--runtime docker|podman|podman-rootless`, `--stacks`, `--network`, `--host-ip`, `--read-only`, `--dir` (výchozí `/opt/moorage-agent`), `--install-runtime`, `--replace`, `--dry-run`, `--help`); `install-cli.sh` dostane kontrolu runtime. CI: `shellcheck` a běh `--dry-run` v kontejnerech Debian a Alpine. Dokumentace: `docs/hosts.md` (instalace agenta), `docs/cli.md`.
- M19F: Pořadí: oprava odkazů dřív než `get_service_links` z M19E (sdílí `stackLinks`), pak kompaktní graf, nakonec Add host se skripty. Skript musí být v tagu releasu dřív, než ho UI začne nabízet, proto vychází ve stejném releasu.
- M19G: Bez migrace. Uzel vrací `locks.selfUpdate` ve stavu (už existuje), nové kódy chyb `self-update-locked` (409) u Apply & restart, Update Moorage a pull vlastního stacku.
- M19H: Nový `scripts/install.sh`, společná část `scripts/lib/runtime.sh` vložená do `install.sh` a `install-agent.sh` při vydání (CI ověří, že vložená kopie sedí).
- M19I: Nová tabulka `assistant_commands` (user_id, name, text, created, updated) – migrace SQLite. Kontext obrazovky jde v požadavku chatu jako pole `notices` (max 20, každé do 2 KiB). Audit `assistant.self_apply`.
- M19J: Nová env `MOORAGE_TAILSCALE_SERVE` (v `EnvLocks`), API agenta `GET/POST /serve` (seznam a změna záznamů Moorage), nastavení odkazu typu `tailscale`. QR knihovna ve frontendu (pod 10 kB gzip).
- M19K: `GET /api/problems` (souhrn napříč hosty, cache 60 s), problémy samostatných kontejnerů v `GET /api/hosts/{host}/containers/{id}/problems`.
- M19L: Kanál dostane pole `mentions` (seznam pravidel: komu, události, rozsah, jen poprvé) v JSON nastavení kanálu – bez migrace.
- M19M: Migrace SQLite pro hodinové agregace (`check_results_hourly`). Pořadí v M19: A–D (oprava vlastního stacku) → E–F → G → I → K → J → L → H → M.

### Mimo rozsah M19
- Automatická instalace Moorage v okně údržby (→ ROADMAP).
- Pull + Apply & restart pro vlastní buildy a forky z kontroly image (→ ROADMAP).
- Kontrola dostupnosti přes veřejnou doménu zvenčí (M18H mimo rozsah, beze změny).
- M19E: Externí MCP servery pro asistenta (zamítnuto uživatelem). Historie více chatů, přílohy a obrázky, zobrazení přemýšlení modelu, uložené dotazy a příkazy s lomítkem, cena v měně (→ ROADMAP). Hlasové ovládání a asistent na pozadí zůstávají mimo rozsah z M18G.
- M19E: Automatické přepnutí na jiného poskytovatele (zamítnuto uživatelem). Odběr změn přes MCP resources, úrovně dat podle poskytovatele, nabídka endpointu Ollamy z vlastních stacků, QR kód odkazu a kontrola dostupnosti odkazu (→ ROADMAP).
- M19F: Automatická detekce `tailscale serve` (patří do add-onu `vpn-expose`, kvůli socketu `tailscaled.sock` s plnou kontrolou nad Tailscale). Instalační skript pro hub, instalace agenta přes SSH z Moorage a zobrazení závislostí jako seznam řetězců (→ ROADMAP).
- M19J: Tailscale Funnel a spouštění `netbird expose` z Moorage (veřejné vystavení do internetu).
- M19L: Označení v jiných kanálech než Discord a Slack (Matrix, Teams) – stejný model pravidel jde rozšířit později.

### Otevřené otázky
- Žádné. Příčina pádu u uživatele ověřena 2026-11-27 na Pi: nejdřív `MOORAGE_LISTEN=5080` bez dvojtečky (`missing port in address`), po opravě port 5080/5081 obsazený `tailscaled` (`tailscale serve` na Tailscale adrese) → `bind: address already in use` v host módu.
- M19E: Ověřit při implementaci, že Ctrl+J přebije zkratku stahování v Chrome a Firefoxu; kde ne, zůstává ikona a paleta Ctrl+K. Okno desktopové Ollamy se nepodařilo ovládat (computer use nebyl v relaci k dispozici); její chování je ze zdrojáků na GitHubu, dokumentace a screenshotu, ne z klikání.
- M19E: Ověřit při implementaci, od které verze Ollama vrací `capabilities` v `/api/show`; u starší verze bez tohoto pole se předpokládá function calling jako u ostatních.
- M19F: Ověřit na ep-pi, že `beszel-agent`, `node-exporter` a `homelab` mají port v `EXPOSE` nebo v compose. Kde ne, zůstane jen Set link. Ověřit, že oficiální `get.docker.com` podporuje Raspberry Pi OS na arm64 i armhf.
