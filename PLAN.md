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
| M10 | **OIDC / forward-auth** | přihlášení přes OIDC (Authentik, Keycloak…) a hlavičku z reverse proxy (jen od důvěryhodné proxy); mapování na role `admin` / `viewer` (ty už existují od M1) |
| M11 | **Audit, záloha, dlouhodobý uptime** | audit log všech akcí (rozšíření tabulky z M9 na up/down/pull/rollback/smazání/změny nastavení); export/import konfigurace hubu; hodinové agregace výsledků kontrol na 1 rok |
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
1. Git repozitář a první commit (projekt zatím není pod gitem).
2. **M10 – OIDC / forward-auth** (prompt pro Gemini, pak review).
3. **M11 – export/import konfigurace, hodinové agregace kontrol** (audit log všech akcí je hotový jako R1).
4. Ověřit R8 (Podman) proti skutečnému Podmanu: `scripts/dev/podman-smoke.sh`, matice v sekci R8.
5. Vydání **v1.0**. Z plánu 1.1/1.2 zbývá jen automatický rollback po selhání kontrol.
