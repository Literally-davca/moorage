# Moorage – roadmap

Nápady mimo MVP. Pořadí v sekci = priorita.
Stav: `v1.0 (M11)` / `v1.1` / `v1.2` (zařazeno do verze) · `líbí se` (uživatel chce, kandidát na první verzi po MVP) · `nápad` · `odloženo` · `zamítnuto`.

## Plán verzí (rozhodnuto 2026-11-22)
- **v1.0:** M10 OIDC / forward-auth a správa uživatelů (hotovo); M12 aktualizace Moorage z UI (hotovo); M11 audit log všech akcí, export/import konfigurace, hodinové agregace kontrol.
- **v1.1 – napojení na okolí:** `/metrics` pro Prometheus/Grafanu, plánovaná údržbová okna, automatický rollback po selhání kontrol.
- **v1.2 – co Kuma ani Dockge neumí:** kontroly z více míst s hlasováním agentů, graf závislostí mezi stacky, časová osa stacku.
- Ostatní zůstává v pořadí níže.

## Líbí se – kandidáti na první verze po MVP

| Nápad | Proč | Cena / poznámka | Zdroj | Stav |
|---|---|---|---|---|
| Audit log všech akcí | Kdo, kdy a odkud spustil up/down/pull/rollback/terminál. Typicky: někdo službu vypnul a pak se diví, že neběží – audit log to vysvětlí. | Malá; terminál ho má už v M9, rozšířit na vše. Záznamy jdou i do časové osy stacku. | agent | hotovo (R1) |
| Časová osa stacku (incident timeline) | Jedno místo: deploy, změna compose (commit), restart, alert, shoda v logu, auto-update, záznam z audit logu. Odpověď na „co se změnilo, než to spadlo?“. | Střední; data vznikají v M2–M8, jde hlavně o UI. | agent | hotovo (R4) |
| Napojení na Grafanu: export metrik (`/metrics`, Prometheus formát) | Stav stacků, sond a kontejnerů v existující Grafaně/VictoriaMetrics místo duplikace grafů. | Malá. Metriky: up/down sond, latence, stav kontejnerů, restarty, CPU/RAM. Volitelně Grafana dashboard JSON v repu. | uživatel + agent | hotovo (R3) |
| Sondy z více míst (hlasování agentů) | Každý agent může sondovat ostatní hosty; DOWN až když selže většina míst → méně falešných alertů. Kuma to neumí, agenty už máme. | Střední: plánování sond, agregace výsledků. | agent | hotovo (R6) |
| Plánovaná údržbová okna | Ztišit alerty a ukázat „údržba“ na status stránce v daný čas. | Malá. | agent | hotovo (R2) |
| Graf závislostí mezi stacky | Sdílené sítě/DB: když spadne stack s databází, alert ukáže dotčené stacky a seskupí kaskádu do jednoho incidentu. | Střední; ze sdílených sítí a `depends_on`, ručně doplnitelné. | agent | hotovo (R5) |
| Šablony stacků | Rychlé nasazení běžných aplikací z katalogu compose šablon – podobně jako v Coolify. | Malá–střední; vlastní/komunitní git repo šablon, bez centrálního katalogu. Šablona = compose + popis proměnných. | agent | hotovo (R7) |
| Podman podpora | Rozšíří publikum (rootless servery, Fedora/RHEL). | Střední. Docker-kompatibilní API, ale bez stabilního engine ID a s rozdíly v compose – potřeba testovací matice. | uživatel + agent | rozpracováno (R8, neověřeno) |

## Další návrhy

| Nápad | Proč | Cena / poznámka | Zdroj | Stav |
|---|---|---|---|---|
| Šifrované tajné hodnoty v `.env` (age/SOPS) + push historie na remote | Historie stacků obsahuje `.env`; push na remote (GitHub/Gitea záloha) by jinak vynesl hesla. Šifrovat v repu, dešifrovat jen na agentovi při nasazení. | Střední. Odemkne odložený push z M2. | agent | nápad |
| Obnova smazaného stacku z historie | Poslední stav smazaného stacku je v historii; z UI ho vrátit jedním klikem. | Malá. | agent | nápad |
| Push/heartbeat sondy | Hlídat cron úlohy a zálohy, které se mají ozvat (dead-man's switch). | Malá: endpoint + timeout. | agent | odloženo (uživatel) |
| Hlídání expirace TLS certifikátů a domén | Kuma parity; častá příčina výpadků. | Malá u TLS, střední u WHOIS (rate limity). | agent | odloženo (uživatel) |
| Web push notifikace (PWA) | Alert na telefon bez Telegramu/ntfy; UI instalovatelné jako aplikace. | Malá–střední; VAPID klíče, service worker. | agent | nápad |
| Úklid disku s náhledem | Nepoužité image, volume a build cache: co se smaže a kolik místa to uvolní, před potvrzením. | Malá; jen pro admina, vždy s náhledem. | agent | nápad |
| Sken zranitelností image (Trivy) | U každého stacku počet kritických CVE; upozornění při auto-update. | Střední; Trivy jako volitelný externí kontejner. | agent | nápad |
| API tokeny a CLI `moorage` | Automatizace (CI nasadí stack, skript zjistí stav) bez UI. | Střední; scope tokenů (read/deploy). | agent | nápad |
| Restic zálohy volume per stack | Dockhand to má; záloha patří ke stacku. | Střední; externí binárka → bezpečnostní posouzení. | agent | nápad |
| Záložní hub (pasivní standby) | Žádný jediný bod selhání: standby hub přebírá replikovanou SQLite (např. Litestream), agenti se při výpadku primárního hubu přepnou. Alternativa k zamítnuté mesh síti (viz PLAN, Watchdog hubu). | Střední–velká: replikace, přepnutí agentů, ochrana proti dvěma aktivním hubům (fencing). | uživatel + agent | nápad |
| Hodinové agregace výsledků kontrol (1 rok) | Dlouhodobý uptime bez růstu DB; plán počítal s 1 rokem agregací, M4 drží jen 30 dní surových dat. | Malá. | agent | v1.0 (M11) |
| Sdílené proměnné mezi stacky | Jedna hodnota (např. heslo k DB, doména) pro víc stacků, změna na jednom místě. Coolify to má. | Střední: nové úložiště, propsání do `.env` při nasazení, historie. | agent | nápad |
| Šifrování `.env` v historii | `history.git` obsahuje hesla v čitelné podobě; šifrování klíčem hubu by zmenšilo dopad úniku zálohy. | Střední; diff tajných hodnot by se zobrazoval maskovaně. | agent | nápad |
| Vyhledávání image při zakládání stacku | Našeptávač názvů image z Docker Hubu/GHCR místo psaní přesného jména. | Malá–střední; rate limity registrů. | agent | nápad |
| Export/import konfigurace Moorage | Záloha hubu (uživatelé, sondy, kanály) a přesun na jiný stroj. | Malá. | agent | v1.0 (M11) |
| Automatický rollback při selhání kontrol po auto-update | Když po automatické aktualizaci image přejde kontrola stacku do stavu DOWN, automaticky vrátit předchozí digest (rollback z M8) a přepnout politiku na `notify`. | Malá–střední; navazuje na `image_updates` a rollback v M8. | agent | v1.1 |
| Automatické povyšování tagů verzí podle pravidel | Volitelné automatické přepsání tagu v `compose.yaml` (např. jen patch/minor ve stejné major řadě) uvnitř časového okna, s git commitem. | Malá–střední; M8 už tagy rozpoznává a přepisuje na kliknutí. | agent | nápad |
