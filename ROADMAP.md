# Moorage – roadmap

Přehled nápadů a budoucích funkcí mimo aktuální implementaci.
> **Pravidlo (2026-11-23):** Jádro Moorage zůstává univerzální. Věci určené pro konkrétní skupinu uživatelů nebo vlastní potřebu se řeší jako volitelné **Add-ons** v **Settings → Add-ons**, jako **Sidecar add-ons** v repozitáři `moorage-addons`, nebo jako samostatná externí repozitáře.

---

## Schválené návrhy (neimplementované)

Položky schválené k realizaci do jádra Moorage:

| Nápad | Proč | Poznámka | Zdroj |
|---|---|---|---|
| **Hodinové agregace výsledků kontrol (1 rok)** | Dlouhodobý uptime bez růstu DB (M4 drží jen 30 dní surových dat). | Zbývá z M11 (`v1.0`). | agent |
| **Podman podpora (dokončení a ověření)** | Ověřit rozpracovanou podporu z R8 proti reálnému Podmanu 4.9 / 5.x (rootful i rootless). | Částečně hotovo v R8. | uživatel + agent |
| **Přehled obsazených portů (Port map)** | Přehled TCP/UDP portů na hostu a varování před kolizí před `up`. | Zařazeno do M15 (`PLAN.md`). | agent + uživatel |
| **Prohlížeč a editor souborů stacku** | Úprava doprovodných souborů (`Caddyfile`, `nginx.conf`…) přímo ze stacku s git historií. | Zařazeno do M15 (`PLAN.md`). | agent + uživatel |
| **Hlídání volného disku a RAM hosta** | Včasné varování před zaplněním disku nebo paměti hosta. | Zařazeno do M15 (`PLAN.md`). | agent + uživatel |
| **Hromadné operace nad stacky (Bulk actions)** | Hromadný start, stop, restart a update vybraných stacků ve frontě. | Zařazeno do M15 (`PLAN.md`). | agent + uživatel |
| **Tailscale / NetBird add-on (`vpn-expose`)** | Detekce VPN rozhraní a přepínání portů jen na VPN síť na jeden klik. | Zařazeno do M15 (`PLAN.md`). | uživatel + agent |
| **Sidecary `log-shipper` a `node-exporter`** | Odesílání logů do VictoriaLogs/Loki a OS metriky po boku vestavěného Promethea. | Zařazeno do M15 (`moorage-addons`). | agent + uživatel |

---

## Návrhy do jádra (k posouzení)

Univerzální funkce pro každého správce Docker/Compose hostů:

| Nápad | Proč | Poznámka | Zdroj |
|---|---|---|---|
| **Tiché hodiny a opakované připomínky alertů** | Nerušit v noci u nekritických kontrol a připomenout trvající výpadek po X hodinách. | Malá; nastavení per kanál nebo globálně. | agent |

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
| **GitOps sync (pull z Git repozitáře)** | Napojení stacku na GitHub/Gitea/Forgejo větev s automatickým nasazením po commitu nebo webhooku. | nápad | agent |
| **Privátní registry přihlášení** | Správa přihlašovacích údajů pro soukromé registry (GHCR, Docker Hub, vlastní registry) pro pull i kontrolu aktualizací. | nápad | agent |
| **Sdílené proměnné mezi stacky** | Centrální proměnné na úrovni hosta/hubu sdílené mezi více stacky (Coolify styl). | nápad | agent |
| **Šifrování `.env` v historii (age/SOPS) + push na remote** | Šifrování tajných hodnot v `history.git` pro bezpečný push záloh na GitHub/Gitea. | nápad | agent |
| **Tailscale / NetBird detekce** | Zobrazení overlay IP adres a rychlých odkazů do privátní sítě (`*.ts.net`). | nápad | agent |
| **Export SLA / incident reportů** | Měsíční přehled výpadků a dostupnosti v Markdownu/JSON. | nápad | agent |
| **Záložní hub (pasivní standby)** | Standby hub přebírající replikovanou SQLite (např. Litestream) s fencingem. | nápad | uživatel + agent |
| **Vyhledávání image při zakládání stacku** | Našeptávač názvů image z Docker Hubu / GHCR. | implementováno (`imagefind`) | agent |

---

## Sidecar add-ons (externí kontejnery)

Samostatné kontejnerové služby v `moorage-addons` spouštěné do společného stacku `add-ons`:

| Sidecar | Popis | Stav | Zdroj |
|---|---|---|---|
| **Trivy CVE vulnerability scanner (`trivy-scanner`)** | Skenování image stacků na známé CVE zranitelnosti bez nafukování binárky Moorage. | implementováno | agent |
| **Restic volume backups (`restic-backup`)** | Plánované šifrované a deduplikované zálohy Docker volumes jednotlivých stacků. | implementováno | agent |
| **Log shipper (`log-shipper` / Vector / Fluent Bit)** | Dlouhodobé odesílání logů kontejnerů do VictoriaLogs / Loki (jádro Moorage logy neukládá). | nápad | agent |
| **Node Exporter (`node-exporter`)** | Detailní metriky hosta (disk I/O, síť, teplota CPU na Pi) pro Prometheus/Grafanu. | nápad | agent |
| **Autoheal (`autoheal`)** | Automatický restart kontejnerů ve stavu `unhealthy` podle labelu. | nápad | agent |
| **Tunnel connector (`cloudflared` / `pangolin`)** | Bezpečné vystavení vybraných služeb bez otevírání portů na domácím routeru. | nápad | agent |

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
| **Add-ons (auto-katalog, filtry, společný stack `add-ons`), Coolify `.env`, čistší Hosts & General settings + Diagnostics** | Přehlednější UI, autodetekce HTTPS cookies, token pro metriky v UI. | M14 |
| **Obnova smazaného stacku, úklid disku + denní úloha, export/import záloh, auto-rollback s UI bannerem, rozšířené notifikace** | Slack/Gotify/Pushover/Matrix, odběr událostí, živé UI/browser alerty. | M14 |

---

## Odložené / Zamítnuté

| Nápad | Důvod | Stav |
|---|---|---|
| **Push/heartbeat sondy** | Hlídaní cron úloh (dead-man's switch). | odloženo (uživatel) |
| **Hlídání expirace TLS certifikátů a domén** | Kuma parita; WHOIS má rate limity. | odloženo (uživatel) |
| **Plná mesh síť bez hubu** | Příliš složitý konsenzus/split-brain; nahrazeno Watchdogem (M6). | zamítnuto |
