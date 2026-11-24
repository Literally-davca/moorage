# Moorage – interface design system

> Schváleno 2026-11-20 · směr „klidný přístav“

## Směr a pocit

- **Kdo:** správce vlastního homelabu/VPS. Otevírá Moorage, když chce něco nasadit, nebo když mu přišel alert a chce rychle zjistit proč.
- **Úkol (focal):** *Je můj stack v pořádku, a pokud ne, co se změnilo?*
- **Pocit:** přívětivý a klidný jako Dockge/Uptime Kuma (zaoblené karty, vzduch, čitelné písmo), ale s vlastní identitou: tmavá hladina, **oranžová bóje** jako jediný akcent, deník jako paměť stacku.
- **Inspirace, ne kopie:** z Dockge/Kumy bereme rozložení seznam + detail, zaoblení a jasný puls. Liší se: akcent (bóje místo mátové zelené), písmo (Onest), podpis (pásek sond s kotvami deployů), Logbook.

## Slovník

- Námořní metafora žije **jen v tokenech a podpisu**. UI mluví srozumitelně anglicky (sentence case): *Stack, Container, Check, Alert, Deploy, Roll back, Maintenance*.
- Jediná výjimka: **Logbook** – sjednocená časová osa stacku (deploy, commit, restart, alert, shoda v logu, audit log).

## Tokeny

### Tmavý režim (výchozí) – „deep water“

| Token | Hodnota | Použití |
|---|---|---|
| `--deep-water` | `#0F1A1F` | plátno aplikace |
| `--pier` | `#16242A` | karty, postranní panel |
| `--pier-raised` | `#1C2D34` | popover, menu, dropdown (o úroveň nad kartou) |
| `--well` | `#0B1418` | inputy, log viewer, hledání (zapuštěné = tmavší) |
| `--foam` | `#E8EEEE` | primární text |
| `--mist` | `#B7C4C6` | sekundární text, neaktivní položky seznamu |
| `--haze` | `#7D8F93` | metadata, popisky, jednotky |
| `--murk` | `#5E7074` | časové značky, disabled |
| `--rope` | `rgba(232,238,238,.06)` | ring karet, jemné oddělení |
| `--rope-strong` | `rgba(232,238,238,.10)` | oddělovače řádků |
| `--rope-stronger` | `rgba(232,238,238,.14)` | ring sekundárních tlačítek, hover |
| `--buoy` | `#FF7A45` | **jediný akcent**: primární akce, fokus, výběr, kotvy deployů |
| `--buoy-ink` | `#2A1206` | text na `--buoy` (kontrast ~7:1) |
| `--buoy-wash` | `rgba(255,122,69,.12)` | vybraná položka seznamu |
| `--buoy-text` | `#FF9A6E` | akcent jako text (deploy v Logbooku) |
| `--afloat` | `#4CC38A` | Up |
| `--adrift` | `#F2555A` | Down (badge text `#FF8A8E`, badge bg `rgba(242,85,90,.15)`) |
| `--signal` | `#F5C451` | Degraded / Partially down / Maintenance |
| `--info` | `#4DA3FF` | klidný informativní stav: modrý odznak (aktivní údržba, dostupné updaty) – text `#8CC4FF`, bg `rgba(77,163,255,.16)` |
| `--becalmed` | `#7D8F93` | Paused / Unknown |

### Světlý režim – „sailcloth“ (výchozí pro veřejnou status stránku)

| Token | Hodnota |
|---|---|
| `--deep-water` | `#F6F3EE` |
| `--pier` | `#FFFFFF` |
| `--pier-raised` | `#FFFFFF` (+ stín) |
| `--well` | `#EFEBE4` |
| `--foam` / `--mist` / `--haze` / `--murk` | `#14232A` / `#3E5258` / `#6A7C80` / `#97A4A7` |
| `--rope` … `--rope-stronger` | `rgba(20,35,42,.08)` / `.12` / `.18` |
| `--buoy` / `--buoy-ink` | `#FF7A45` / `#2A1206` (stejné – tlačítko drží kontrast v obou režimech) |
| `--buoy-wash` / `--buoy-text` | `rgba(255,122,69,.14)` / `#C2410C` |
| `--afloat` / `--adrift` / `--signal` | `#1E8E5A` / `#D63A40` / `#B7860B` |
| `--info` / `--info-text` / `--info-wash` | `#1D6FD8` / `#1858AC` / `rgba(29,111,216,.12)` |

Odznaky v navigaci: vždy nejvýše jeden na položku. Červený (`--adrift`) = něco selhává (počet), modrý (`--info`) = jen informace (aktivní údržba, dostupné aktualizace). `0` se nikdy nevykresluje. `--info` se nesmí plést s akcentem `--buoy` ani se žlutým `--signal`.

Pravidla: jeden odstín (modro-zelená hladina), mezi povrchy se mění jen světlost. Žádné gradienty. Barva = význam (stav nebo akce), nikdy dekorace. Rozložení ~60/30/10: hladina / molo / bóje + stavy.

## Typografie

- **Onest** (OFL) – UI. **JetBrains Mono** (OFL) – logy, čísla, hashe, časy. Obě jako woff2 **vložené v binárce** (žádné Google Fonts – soukromí, offline).
- Měřítko 1,25 od 14 px: `11 · 12 · 14 · 16 · 18 · 22 · 28`.
- Váhy: 400 / 500 / 600. Hierarchie hlavně vahou a barvou, ne velikostí.
- Nadpis stacku 22/600, tracking −0,015em. Sekce v kartě 13/600. Položka seznamu 14/400 (`--mist`), vybraná 14/500 (`--foam`). Metadata 11–12/400 `--haze`.
- Všechna dynamická čísla `font-variant-numeric: tabular-nums`. Hero číslo (uptime) mono 22/500 s jednotkou 13 `--haze`.
- `text-wrap: balance` nadpisy, `pretty` texty, `-webkit-font-smoothing: antialiased`.

## Prostor, tvar, hloubka

- Základní jednotka **4 px**. Mezera mezi kartami 12. Vnitřek karty 16 × 20 (vertikálně × horizontálně), kompaktní karta 14 × 18. Položka seznamu 9 × 10.
- **Rádiusy:** 6 malé ovládací prvky · 8 tlačítka, inputy, položky seznamu · 12 karty · 16 shell a modální okna · 999 jen stavové badge. Vnořené prvky: vnější = vnitřní + padding.
- **Hloubka:** posuny povrchů + 1px ring (`box-shadow: 0 0 0 1px var(--rope)`), v tmavém režimu žádné vržené stíny. Ve světlém: ring + `0 1px 2px -1px rgba(20,35,42,.06), 0 2px 4px rgba(20,35,42,.04)`. Popover o úroveň výš než rodič.
- **Rozložení:** postranní seznam stacků **240 px** (stejné pozadí jako karta, ne jiný odstín), detail fluidní, max. šířka obsahu 1200 px. Pod 768 px: seznam je samostatná obrazovka, detail se otevírá přes něj.

## Podpis: pásek sond (Soundings strip)

- Každá kontrola = svislý tik **5 px široký, mezera 3 px, rádius 3 px**, výška 14–22 px podle latence (normalizováno na stack), barva podle stavu.
- **Deploy kotva:** tik 3 px široký, plná výška 26 px, `--buoy`; hover ukáže commit, autora, čas. Stejné kotvy pro rollback a auto-update.
- Údržba: tiky `--signal` se sníženou výškou (10 px).
- Pásek je vždy vedle hero čísla uptime; pod ním legenda (up / down / deploy) 11 px.
- Výskyty podpisu: detail stacku, položka seznamu (mini verze 3 px tiky, posledních 24), status stránka, alert notifikace (náhled jako obrázek – roadmap), Logbook (kotvy odpovídají záznamům).

## Komponenty

| Komponenta | Míry a pravidla |
|---|---|
| **Stack list item** | 36–40 px výška, rádius 8, padding 9 × 10, stavová tečka 8 px + název. Vybraná: `--buoy-wash`, text 500. Hover: `--rope` pozadí. |
| **Status badge** | pill 999, 12/500, padding 3 × 9, tónové pozadí stavu + text stavu. Vždy text (Up / Down · exit 137 / Maintenance), stav nikdy jen barvou. |
| **Button primary** | `--buoy` / `--buoy-ink`, 13/500, padding 7 × 12, rádius 8, výška 32. **Max. jeden na pohled.** |
| **Button secondary** | průhledné, ring `--rope-stronger`, text `--mist`. |
| **Button ghost** | bez ringu, hover `--rope`. Nebezpečné akce (Down, Delete) jsou secondary s textem `--adrift` a vždy s potvrzením. |
| **Input / search** | `--well`, rádius 8, výška 32, focus ring 2 px `--buoy` + 4 px `--buoy-wash`. |
| **Stack header** | název 22/600, pod ním badge + doba v mono 12 `--haze`; akce vpravo (secondary Logs, primary podle kontextu: Deploy / Roll back). |
| **Logbook** | karta; řádek: čas mono 12 `--murk`, typ (deploy `--buoy-text`, alert `#FF8A8E`, audit `--mist`), popis. Řádková výška 1,85. Filtr podle typu. |
| **Log viewer** | `--well`, mono 12/1,7, čas `--murk`, název kontejneru `--haze`; zvýraznění shody `--buoy-wash`. Žádná animace přírůstků; auto-scroll se zastaví při ručním scrollu („Jump to latest“). |
| **Compose editor** | `--well`, mono 13, gutter `--murk`; chyba validace = podtržení `--adrift` + řádek v panelu pod editorem. |
| **Host switcher** | v hlavičce postranního panelu; host offline = šedá tečka `--becalmed` + „last seen“. |

## Stavy (vždy navržené)

- **Prázdno:** pozvánka, ne omluva („Add your first stack“ + akce Create stack / Scan folder).
- **Načítání:** skeleton ve tvaru obsahu (tiky v `--rope-strong`), žádné spinnery přes celou stránku.
- **Chyba:** co se stalo + co dělat; žádné surové výjimky.
- **Read-only stack** (mimo adresář Moorage): zámek + vysvětlení, akce skryté, ne disabled.
- Interaktivní prvky: default, hover, active (`scale(.97)`), focus-visible, disabled jen výjimečně (s důvodem).

## Pohyb

- 150–200 ms, `cubic-bezier(0.23, 1, 0.32, 1)`; jen `transform` a `opacity`.
- Bez animace: přepínání stacků v seznamu, příkazová paleta, proud logů, nové tiky (jen se objeví).
- Popovery z místa spouštěče od `scale(.95)`; modály střed. `prefers-reduced-motion` respektováno.

## Ikony

Tabler Icons (outline, MIT) jako SVG sprite v binárce, 16 px v textu, 14 px v tlačítkách. Logo: kotva na `--buoy` čtverci (rádius 7).

## Mimo systém (záměrně)

- Žádné grafy CPU/RAM s výplní a gradienty – sparkline 1,5 px čára `--mist`.
- Žádné karty metrik v mřížce „ikona-číslo-popisek“; přehled hostu = seznam stacků se stavem.
