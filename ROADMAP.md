# Moorage – roadmap

Přehled nápadů a budoucích funkcí mimo aktuální implementaci.
> **Pravidlo (2026-11-23):** Jádro Moorage zůstává univerzální. Věci určené pro konkrétní skupinu uživatelů nebo vlastní potřebu se řeší jako volitelné **Add-ons** v **Settings → Add-ons**, jako **Sidecar add-ons** v repozitáři `moorage-addons`, nebo jako samostatná externí repozitáře.

---

## Schválené návrhy (neimplementované)

Položky schválené k realizaci do jádra Moorage:

| Nápad | Proč | Poznámka | Zdroj |
|---|---|---|---|
| **Hodinové agregace výsledků kontrol (1 rok)** | Dlouhodobý uptime bez růstu DB (M4 drží jen 30 dní surových dat). | Implementováno v M19M (2026-11-27) (`PLAN.md`). | agent |
| **Podman podpora (dokončení a ověření)** | Ověřit rozpracovanou podporu z R8 proti reálnému Podmanu 4.9 / 5.x (rootful i rootless). | Implementováno v M19M (2026-11-27) (`PLAN.md`), ověřeno na WSL (Podman 5.4.2 rootful i rootless). | uživatel + agent |
| **Prohlížeč a editor souborů stacku** | Úprava doprovodných souborů (`Caddyfile`, `nginx.conf`…) přímo ze stacku s git historií. | Implementováno v M19M (2026-11-27) (`PLAN.md`, návrh z M15). | agent + uživatel |
| **Hlídání volného disku a RAM hosta** | Včasné varování před zaplněním disku nebo paměti hosta. | Implementováno v M19M (2026-11-27) (`PLAN.md`, návrh z M15). | agent + uživatel |
| **Hromadné operace nad stacky (Bulk actions)** | Hromadný start, stop, restart a update vybraných stacků ve frontě. | Implementováno v M19M (2026-11-27) (`PLAN.md`, návrh z M15). | agent + uživatel |
| **Tailscale / NetBird add-on (`vpn-expose`)** | Detekce VPN rozhraní a přepínání portů jen na VPN síť na jeden klik. | Implementováno 2026-11-27: přepnutí portů na VPN adresu = Fix v M19F, Tailscale Serve = M19J (`PLAN.md`); NetBird jen odkaz a příkaz. | uživatel + agent |
| **Sidecary `log-shipper` a `node-exporter`** | Odesílání logů do VictoriaLogs/Loki a OS metriky po boku vestavěného Promethea. | Implementováno v M19M (2026-11-27) (`moorage-addons`). | agent + uživatel |

---

## Návrhy do jádra (k posouzení)

Univerzální funkce pro každého správce Docker/Compose hostů:

| Nápad | Proč | Poznámka | Zdroj |
|---|---|---|---|
| **Tiché hodiny a opakované připomínky alertů** | Nerušit v noci u nekritických kontrol a připomenout trvající výpadek po X hodinách. | Malá; nastavení per kanál nebo globálně. | agent |
| **Automatická instalace Moorage v okně údržby** | Policy auto u vlastního stacku by Moorage opravdu aktualizovala, ne jen oznámila. | Střední; staví na M19C. Riziko: Moorage se mění bez člověka. V M19 uživatel zvolil variantu bez toho (2026-11-27). | agent |
| **Pull + Apply & restart pro vlastní buildy Moorage** | Update z kontroly image i pro image, které nejsou podepsaný release (fork, vlastní registr). | Malá; helper s rollbackem z M19A, bez ověření podpisu. | agent |

---

## Add-ons (volitelné integrace a funkce)

Vestavěné (`builtin`) funkce a integrace mimo univerzální jádro, zapínatelné v **Settings → Add-ons**:

| Add-on | Popis | Stav | Zdroj |
|---|---|---|---|
| **Proxy & dashboard label detection (`proxy-labels`)** | Autodetekce webových URL z labelů Traefik, Caddy a Homepage. | implementováno | uživatel + agent |
| **Official template catalog (`official-templates`)** | Připojení repozitáře `Literally-davca/moorage-templates` s denní obnovou. | implementováno | uživatel + agent |
| **Prometheus & Grafana metrics (`prometheus-grafana`)** | Export metrik na `/metrics` a hotový Grafana dashboard. | implementováno | uživatel + agent |
| **Automatické povyšování tagů verzí** | Volitelné přepsání tagu v `compose.yaml` (patch/minor) v časovém okně s git commitem. | nápad | agent + uživatel |
| **MCP server pro AI agenty (`mcp-server`)** | Volitelné HTTP/SSE MCP rozhraní nad API tokeny pro vzdálenou správu, vytváření a zastavování stacků agenty (pro lokální agenty stačí CLI/soubory). | implementováno (`addon_mcp.go`, ACL read/write) | uživatel + agent |
| **Označení rolí v Discordu / Slacku při DOWN** | Volitelný ping `@role` jen u DOWN kritických kontrol (logy nikdy nikoho neoznačí). | implementováno v M19L (2026-11-27) (`PLAN.md`), podmínky nastavitelné | agent |
| **Wasm (`wazero`) sandbox pro uživatelské add-ony** | Vlastní notifikátory a sondy bez nafukování jádra a bez spouštění cizích procesů. | nápad (z analýzy konkurence) | agent |
| **API tokeny a CLI `moorage` + TUI `moorage cli`** | Automatizace (CI nasadí stack, skript zjistí stav) přes Unix socket / token a interaktivní TUI layout; headless režim bez web portu. | zařazeno do `M16B` (`PLAN.md`) | uživatel + agent |
| **Shell completions a multi-profil CLI (`~/.config/moorage/config.json`)** | Automatické doplňování pro bash/zsh/fish a přepínání více vzdálených hubů bez opakování `--url` a `--token`. | nápad (po M16B) | agent |
| **Windows Named Pipe pro lokální CLI** | Lokální CLI připojení bez TCP portu i na Windows hostech (v M16B lokálně přes Unix socket na Linuxu/macOS, na Windows přes `--url` + token). | nápad (po M16B) | agent |
| **Vlastní klávesové mapování a motivy pro TUI** | Přizpůsobení zkratek a barevného schématu v `moorage cli` mimo výchozí paletu „klidný přístav“. | nápad (po M16B) | agent |
| **GitOps sync (pull z Git repozitáře)** | Napojení stacku na GitHub/Gitea/Forgejo větev s automatickým nasazením po commitu nebo webhooku. | nápad | agent |
| **Privátní registry přihlášení** | Správa přihlašovacích údajů pro soukromé registry (GHCR, Docker Hub, vlastní registry) pro pull i kontrolu aktualizací. | nápad | agent |
| **Sdílené proměnné mezi stacky** | Centrální proměnné na úrovni hosta/hubu sdílené mezi více stacky (Coolify styl). | nápad | agent |
| **Šifrování `.env` v historii (age/SOPS) + push na remote** | Šifrování tajných hodnot v `history.git` pro bezpečný push záloh na GitHub/Gitea. | nápad | agent |
| **Tailscale / NetBird detekce** | Zobrazení overlay IP adres a rychlých odkazů do privátní sítě (`*.ts.net`). | detekce hotová v M17B, Tailscale Serve implementováno v M19J (`PLAN.md`) | agent |
| **Export SLA / incident reportů** | Měsíční přehled výpadků a dostupnosti v Markdownu/JSON. | nápad | agent |
| **Záložní hub (pasivní standby)** | Standby hub přebírající replikovanou SQLite (např. Litestream) s fencingem. | nápad | uživatel + agent |
| **Vyhledávání image při zakládání stacku** | Našeptávač názvů image z Docker Hubu / GHCR. | implementováno (`imagefind`) | agent |
| **Obnova z S3 / WebDAV při prvním spuštění** | `MOORAGE_RESTORE_FILE` z M20K umí jen lokální soubor; varianta stáhne poslední vzdálenou zálohu (S3, WebDAV) podle nastavení v env. | nápad (po M20K); střední, přístupové klíče v env | agent |
| **Přesun stacku na jiný host pomocí snapshotu** | Snapshot (data + konfigurace) obnovený na jiném hostu = migrace stacku jedním tokem. | nápad (po M18F) | agent |
| **Databázově vědomé snapshoty (`pg_dump` / `mysqldump` hooky)** | Snapshot databáze bez pauzy a s jistotou obnovy; hook z katalogu receptů M18E. | nápad (po M18F) | agent |
| **`restic-backup` zálohuje i složku snapshotů** | Snapshoty z M18F se dostanou mimo server a do retence Resticu bez další konfigurace. | nápad (po M18F) | agent |
| **Uložení image do snapshotu (`docker save`)** | Obnova i tehdy, když tag z registru zmizel nebo host je offline. | nápad (po M18F) | agent |
| **Asistent reaguje na alert („Explain this alert“)** | Tlačítko u alertu a v incidentu M18A otevře panel s předvyplněným kontextem výpadku. | Implementováno v M19I (2026-11-27) (`PLAN.md`). | agent |
| **Další poskytovatelé LLM: Azure OpenAI, Amazon Bedrock** | Firemní účty bez veřejného API klíče. | nápad (po M18G) | agent |
| **Asistent v CLI (`moorage ask`)** | Stejný asistent a nástroje v terminálu / TUI. | nápad (po M18G) | agent |
| **„Ask the assistant“ u karty Problems a u chyby deploye** | Jedno tlačítko otevře panel s otázkou a kontextem konkrétního problému nebo neúspěšného Apply, bez psaní. | Implementováno v M19I (2026-11-27) (`PLAN.md`). | agent |
| **Přílohy v chatu (soubor, screenshot)** | Přetáhnout `compose.yaml` z jiného serveru nebo screenshot chyby jako v Ollamě; obrázky jen u modelů, které je umí. | nápad (po M19E); střední, limit velikosti a maskování tajemství jako u logů | agent |
| **Uložené dotazy a příkazy s lomítkem (`/why`, `/logs`, `/explain`)** | Opakované dotazy jedním slovem místo psaní. | Implementováno v M19I (2026-11-27) (`PLAN.md`): vestavěné + vlastní na hubu. | agent |
| **Zobrazení přemýšlení modelu (reasoning)** | Sbalitelný blok „Thinking“ u modelů, které ho vrací (Ollama thinking, Claude), aby bylo vidět, proč asistent navrhuje změnu. | Implementováno v M19I (2026-11-27) (`PLAN.md`): sbalené, neukládá se. | agent |
| **Odhad ceny v měně** | U odpovědi a v měsíčním souhrnu cena podle ceníku modelu, nejen tokeny. | nápad (po M19E); malé, ceníky se mění | agent |
| **Endpoint Ollamy z vlastních stacků** | Když běží stack s image `ollama/ollama`, formulář poskytovatele nabídne jeho adresu (`http://<host>:11434`) z odkazů služeb. Bez skenování sítě. | nápad (po M19E); malé | agent |
| **Úrovně dat podle poskytovatele** | Logy a audit jen pro lokální modely (Ollama), do cloudu Never nebo Ask. S víc poskytovateli z M19E je to přirozený další krok. | nápad (po M19E); střední, víc nastavení | agent |
| **Odběr změn přes MCP resources** | `resources/subscribe` pro externí agenty: upozornění na změnu stavu stacku nebo nový incident bez dotazování. Vyžaduje stavové spojení (SSE), M19E má resources jen ke čtení. | nápad (po M19E); střední | agent |
| **QR kód odkazu služby** | U tlačítka Open v chatu i v detailu stacku QR kód pro otevření služby v mobilu. | Implementováno v M19J (2026-11-27) (`PLAN.md`). | agent |
| **Kontrola dostupnosti odkazu** | Před nabídnutím odkazu krátký HEAD přes `netguard` a štítek „reachable“ / „not responding“, aby asistent nedával odkaz na nedostupnou adresu. | nápad (po M19E); malé, odkaz může fungovat jen z jiné sítě | agent |
| **Instalační skript pro hub (`install.sh`)** | Stejná kontrola a instalace Dockeru nebo Podmanu jako u agenta (M19F), pak zapsání `compose.yaml` hubu s vybranými poli a první spuštění. | Implementováno v M19H (2026-11-27) (`PLAN.md`). | agent |
| **Varování před veřejně otevřeným portem** | U portů publikovaných na všech adresách upozornit, že Docker obchází ufw, a nabídnout přepnutí na VPN adresu nebo `127.0.0.1` (opačný směr k Fix z M19F). | nápad (po M19F); malé | agent |
| **Instalace agenta přes SSH z Moorage** | Admin zadá SSH hosta a Moorage instalační skript spustí sám. Bez kopírování příkazu, ale Moorage by musel držet SSH klíč k cizím hostům. | nápad (po M19F); velké, bezpečnost klíčů | agent |
| **Závislosti jako seznam řetězců** | Třetí zobrazení vedle Compact a Detailed: po hostech řádky `beszel-agent → beszel → homepage`, vhodné pro mobil a čtečky obrazovky. | nápad (po M19F); malé | agent |

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
| **Přehled obsazených portů (Port map)** | Přehled TCP/UDP portů na hostu a varování před kolizí před `up`. | M17B |
| **Bohatá oznámení (embedy, předvolby obsahu, odesílatel `<host> • Moorage <role>`)** | Přehlednější alerty a volba, kolik toho zpráva obsahuje. | M17B |
| **Bezpečnostní balík WebSocketů, CI pojistky a PSS Memory Gate** | Tiket pro terminál, ukončení živých spojení při revokaci, hlídání limitu 100 MB PSS v CI. | M17A |
| **Incidenty s časovou osou a Rollback & Verify** | Co se změnilo před výpadkem a ověřený návrat na funkční verzi. | M18A |
| **Passkeys, SBOM + cosign, threat model** | Pilíře důvěry před veřejným vydáním. | M18B |
| **Importéry (Portainer, Uptime Kuma, Gatus) a demo instance** | Snadný přechod od konkurence. | M18C |
| **Klíčová slova `@generate` / Coolify `SERVICE_*`, volné porty a recepty služeb** | Hesla, klíče a nekolizní porty se vygenerují při uložení do `.env`; linter podle image napoví, co služba potřebuje. | M18E |
| **Statický snapshot status stránky, oddělení logů v tunelu** | Status stránka přežije výpadek serveru; logy nebrzdí řídicí provoz. | M18D |
| **Karta Problems u kontejnerů a komprimované snapshoty stacků a kontejnerů** | Konkrétně říct, co je s kontejnerem špatně a jak to opravit; bod obnovy dat a konfigurace před riskantní změnou. | M18F |
| **AI asistent (built-in add-on)** | Panel s chatem, který ví, kde co v Moorage je, dává přesné odkazy a podle zvolené míry oprávnění navrhne nebo provede změnu. | M18G |

---

## Odložené / Zamítnuté

| Nápad | Důvod | Stav |
|---|---|---|
| **Push/heartbeat sondy** | Hlídaní cron úloh (dead-man's switch). | odloženo (uživatel) |
| **Hlídání expirace TLS certifikátů a domén** | Kuma parita; WHOIS má rate limity. | odloženo (uživatel) |
| **Plná mesh síť bez hubu** | Příliš složitý konsenzus/split-brain; nahrazeno Watchdogem (M6). | zamítnuto |
| **Historie více chatů asistenta (v prohlížeči)** | Seznam konverzací jako v Ollamě; v M19E uživatel ponechal jednorázový chat z M18G (2026-11-27). | odloženo (uživatel) |
| **Externí MCP servery pro AI asistenta (GitHub, Home Assistant…)** | Moorage nezná jejich nástroje, takže na ně nejde uplatnit pevné zákazy, a data Moorage by mohla odcházet ven. Asistent používá jen registr nástrojů Moorage (M19E, 2026-11-27). | zamítnuto (uživatel) |
| **Automatické přepnutí asistenta na jiného poskytovatele** | Když poskytovatel nejde, logy a compose by mohly potichu odejít z lokálního modelu do cloudu. Místo toho chyba nabídne adminovi tlačítko Try with… (M19E, 2026-11-27). | zamítnuto (uživatel) |
