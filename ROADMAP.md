# Moorage – roadmap

Přehled nápadů a budoucích funkcí mimo aktuální implementaci.
> **Pravidlo (2026-11-23):** Jádro Moorage zůstává univerzální. Věci určené pro konkrétní skupinu uživatelů nebo vlastní potřebu se řeší jako volitelné **Add-ons** v **Settings → Add-ons**, jako **Sidecar add-ons** v repozitáři `moorage-addons`, nebo jako samostatná externí repozitáře.

---

## Schválené návrhy (neimplementované)

Položky schválené k realizaci do jádra Moorage (viz podrobný návrh v `PLAN.md`):

| Nápad | Proč | Poznámka | Zdroj |
|---|---|---|---|
| **Add-ons: filtry, auto-katalog a společný stack `add-ons`** | Rychlejší orientace v add-onech a jediný stack `add-ons` místo desítek `addon-*` stacků. | Filtry podle stavu/kategorie, auto-načtení katalogu, správa služeb v jednom compose. | uživatel |
| **Coolify styl `.env` v compose** | Automatické doplňování proměnných z `compose.yaml` do `.env` a přehled služeb, které je používají. | Bez nutnosti ručně přidávat každou `${VAR}`. | uživatel |
| **Zpřehlednění stránky Hosts** | Oddělení seznamu hostů, přidání hosta (modal), Watchdogu a přesun Terminal sessions do Auditu. | Lepší čitelnost a orientace. | uživatel |
| **Rozšíření notifikací** | Nové události (auto-update, rollback, selhání deploye, offline agent), kanály (Slack, Gotify, Pushover, Matrix, vlastní HTTP) a UI toasty / Web Push. | Po vzoru Kumy, vše pod `netguard`. | uživatel + agent |
| **Obnova smazaného stacku z historie** | Poslední stav smazaného stacku už je v `history.git`; vrátit ho z UI jedním klikem. | Malá úprava (navazuje na M2). | agent |
| **Úklid disku s náhledem + denní úloha** | Přehled nepoužitých image, volumes a build cache před potvrzením + volitelný denní úklid v Settings. | Volumes nikdy nemazat automaticky bez explicitního souhlasu. | agent + uživatel |
| **Export a import konfigurace i dat** | Kompletní záloha hubu (SQLite DB) i definic stacků (`compose.yaml`, `.env`, `history.git`) pro přesun na jiný stroj. | Aplikační volumes řeší Restic add-on. | uživatel + agent |
| **Automatický rollback po selhání kontrol** | Když po auto-update přejde kontrola stacku do `DOWN`, vrátit předchozí digest a přepnout politiku na `notify`. | Navazuje na M8 a M4 (`v1.1`). | agent |
| **Hodinové agregace výsledků kontrol (1 rok)** | Dlouhodobý uptime bez růstu DB (M4 drží jen 30 dní surových dat). | Zbývá z M11 (`v1.0`). | agent |
| **Podman podpora (dokončení a ověření)** | Ověřit rozpracovanou podporu z R8 proti reálnému Podmanu 4.9 / 5.x (rootful i rootless). | Částečně hotovo v R8. | uživatel + agent |

---

## Add-ons (volitelné integrace a funkce)

Vestavěné (`builtin`) funkce a integrace mimo univerzální jádro, zapínatelné v **Settings → Add-ons**:

| Add-on | Popis | Stav | Zdroj |
|---|---|---|---|
| **Proxy & dashboard label detection (`proxy-labels`)** | Autodetekce webových URL z labelů Traefik, Caddy a Homepage. | implementováno | uživatel + agent |
| **Official template catalog (`official-templates`)** | Připojení repozitáře `Literally-davca/moorage-templates` s denní obnovou. | implementováno | uživatel + agent |
| **Prometheus & Grafana metrics (`prometheus-grafana`)** | Export metrik na `/metrics` a hotový Grafana dashboard. | implementováno | uživatel + agent |
| **Automatické povyšování tagů verzí** | Volitelné přepsání tagu v `compose.yaml` (patch/minor) v časovém okně s git commitem. | nápad | agent + uživatel |
| **MCP server pro AI agenty** | Volitelné HTTP/SSE MCP rozhraní nad API tokeny pro vzdálenou správu, vytváření a zastavování stacků agenty (pro lokální agenty stačí CLI/soubory). | nápad | uživatel + agent |
| **API tokeny a CLI `moorage`** | Automatizace (CI nasadí stack, skript zjistí stav) bez UI; scope tokenů (`read`/`deploy`). | nápad | agent |
| **Vyhledávání image při zakládání stacku** | Našeptávač názvů image z Docker Hubu / GHCR. | implementováno (`imagefind`) / doladit jako add-on | agent |
| **Sdílené proměnné mezi stacky** | Centrální proměnné na úrovni hosta/hubu sdílené mezi více stacky (Coolify styl). | nápad | agent |
| **Šifrování `.env` v historii (age/SOPS) + push na remote** | Šifrování tajných hodnot v `history.git` pro bezpečný push záloh na GitHub/Gitea. | nápad | agent |
| **Záložní hub (pasivní standby)** | Standby hub přebírající replikovanou SQLite (např. Litestream) s fencingem. | nápad | uživatel + agent |

---

## Sidecar add-ons (externí kontejnery)

Samostatné kontejnerové služby v `moorage-addons` spouštěné do společného stacku `add-ons`:

| Sidecar | Popis | Stav | Zdroj |
|---|---|---|---|
| **Trivy CVE vulnerability scanner (`trivy-scanner`)** | Skenování image stacků na známé CVE zranitelnosti bez nafukování binárky Moorage. | implementováno | agent |
| **Restic volume backups (`restic-backup`)** | Plánované šifrované a deduplikované zálohy Docker volumes jednotlivých stacků. | implementováno | agent |

---

## Implementované

Funkce dokončené a začleněné do Moorage (podrobnosti v `PLAN.md`):

| Funkce | Popis | Verze / Milník |
|---|---|---|
| **All-in-one základ, Compose SDK, živé logy, stats** | Správa stacků, převod `docker run`, auth + TOTP. | M1 |
| **Git verze stacků (`history.git`)** | Každá změna jako commit, diff a rollback v UI. | M2 |
| **Hub + agenti (mTLS WebSocket + yamux)** | Více hostů za NAT, read-only režim agenta, socket proxy. | M3 |
| **Sondy a alerty s kontextem** | HTTP/TCP/ping/DNS/stack kontroly, 20 řádků logů v alertu, ztišení při deployi. | M4 |
| **Alerty z logů** | RE2 regex/klíčové slovo sledované přímo na agentovi. | M5 |
| **Watchdog hubu** | Kvorum agentů a deterministický reporter při výpadku hubu. | M6 |
| **Veřejné status stránky a RSS** | Izolované DTO bez úniku interních dat, vlastní domény. | M7 |
| **Auto-update image** | Kontrola digestů/tagů, notifikace nebo automatický pull+up. | M8 |
| **Webový terminál** | Exec do kontejneru (opt-in na agentovi) s auditem. | M9 |
| **OIDC / forward-auth a správa uživatelů** | Přihlášení přes Authentik/Keycloak nebo proxy hlavičku, role admin/viewer. | M10 |
| **Aktualizace Moorage z UI** | Ed25519 podepsané releasy pro hub i agenty s automatickým rollbackem. | M12 |
| **Audit log všech akcí** | Záznam všech operací (up/down/pull/rollback/terminál/nastavení). | R1 (M11) |
| **Plánovaná údržbová okna** | Ztišení alertů a stav „údržba“ na status stránce. | R2 |
| **Napojení na Grafanu (`/metrics`)** | Prometheus endpoint + dashboard JSON (přesunuto do add-onů). | R3 |
| **Časová osa stacku (Logbook)** | Sjednocený přehled deployů, commitů, alertů a auditu. | R4 |
| **Graf závislostí mezi stacky** | Sdílené sítě a `depends_on`, seskupení kaskádových alertů. | R5 |
| **Sondy z více míst (hlasování agentů)** | Kontroly z více agentů s kvórem proti falešným poplachům. | R6 |
| **Šablony stacků** | Vestavěné šablony + vzdálené HTTPS indexy (vč. `moorage-templates`). | R7 |
| **Webové odkazy služeb, Settings, Monitoring, `Ctrl+K`** | `moorage.url`, sjednocená navigace, globální vyhledávání. | M13+ |

---

## Odložené / Zamítnuté

| Nápad | Důvod | Stav |
|---|---|---|
| **Push/heartbeat sondy** | Hlídaní cron úloh (dead-man's switch). | odloženo (uživatel) |
| **Hlídání expirace TLS certifikátů a domén** | Kuma parita; WHOIS má rate limity. | odloženo (uživatel) |
| **Plná mesh síť bez hubu** | Příliš složitý konsenzus/split-brain; nahrazeno Watchdogem (M6). | zamítnuto |
