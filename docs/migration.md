# Migrating to Moorage

Moorage can take over what Portainer, Dockge, Uptime Kuma and Gatus manage today. Every import shows a preview first and **nothing is deployed or started**: imported stacks are saved as files, you deploy them when you are ready. All imports live under **Settings → Import** (admins only) and are recorded in the audit log (`import.portainer`, `import.monitors`).

## Portainer CE

1. In Portainer, open **My account → Access tokens** and create a token. A token of an admin sees every stack; a normal user's token sees only that user's stacks.
2. In Moorage, open **Settings → Import → Portainer CE stacks**, enter the Portainer URL (for example `https://portainer.lan:9443`), the token and the Moorage host the stacks should go to. Tick **Accept a self-signed certificate** if Portainer still uses its own certificate.
3. **Load stacks** reads the stack list and each compose file through the Portainer API. The preview shows every stack with its status:
   - **Ready**: it can be imported as is (the compose file passed the same validation and linter rules as a normal save).
   - **Name taken**: a stack with that name already exists in Moorage, or another Portainer stack in the list has the same name. Tick it and type another name.
   - **Needs attention**: the compose file doesn't validate in Moorage (the reason is shown), or the name can't be turned into a stack name.
   - **Not supported**: Swarm and Kubernetes stacks, Git-based stacks for which Portainer returns no file, empty files and files over 512 KB.
4. **Import** saves each selected stack as `compose.yaml` + `.env` in the stacks folder, through the same save path as the editor, with one history version ("Imported from Portainer stack …").

What to know:

- **Environment variables** of a Portainer stack are written to the stack's `.env`, the place Moorage keeps stack secrets. Names that aren't valid variable names are left out (the preview says which).
- **Files next to the compose file** are not part of the Portainer API. The preview warns about `build:` contexts, `env_file:` files and relative bind mounts (`./data`), which pointed into Portainer's data folder (`/data/compose/<id>/`). Copy them into the new stack folder before deploying. A compose file that uses `env_file: stack.env` (Portainer writes its variables there) should be changed to `.env`.
- **Git-based stacks** are saved with the file Portainer has now; the repository link isn't kept.
- **Stack names** are lowercase letters, digits, `-` and `_` (the compose project name). `My App` becomes `my-app`. When the Portainer stack still runs, its containers already belong to that project name: **stop the stack in Portainer before you deploy it from Moorage**, so two tools never manage the same containers.
- **The token** is used for one request only: it is never stored, logged, written to the audit log or sent anywhere except the Portainer URL you entered. Moorage doesn't follow redirects (the token would travel with them) and reaches Portainer through the same SSRF guard as checks and notifications: LAN addresses are fine, link-local and cloud metadata addresses are refused.

## Dockge

There is nothing to import: Dockge keeps every stack in its own folder (`/opt/stacks/<name>/compose.yaml` and `.env`), which is exactly what Moorage reads.

1. Stop Dockge (or at least don't use it for changes while you try Moorage).
2. Mount the Dockge stacks folder into Moorage **at the same path it has on the host** and point `MOORAGE_STACKS_DIR` at it. With Dockge's default folder that is the Moorage default too:

   ```yaml
   services:
     moorage:
       volumes:
         - /var/run/docker.sock:/var/run/docker.sock
         - moorage-data:/data
         - /opt/stacks:/opt/stacks   # same path inside and outside
       environment:
         MOORAGE_STACKS_DIR: /opt/stacks
   ```

   The same path matters because compose resolves relative paths (`./data`, `env_file`) on the host.
3. Start Moorage. Every folder with `compose.yaml`, `compose.yml`, `docker-compose.yaml` or `docker-compose.yml` becomes a managed stack; running containers are matched by their compose project name (the folder name, as Dockge starts them). Moorage records the files as the first version of each stack ("First version found on disk") in its own history in the data folder (`history.git`).

**Nothing in the folder is changed** when Moorage takes it over: no file is written, renamed or re-owned until you save a stack in Moorage (a test, `TestTakeOverDockgeFolder`, checks that contents and modification times stay the same). Changes made later with another tool show up in the history as "Changed outside Moorage".

If your Dockge folder lives elsewhere (for example `/srv/dockge/stacks`), either set `MOORAGE_STACKS_DIR` to that folder (mounted at the same path), or keep `/opt/stacks` as Moorage's own folder and add the Dockge one with `MOORAGE_EXTRA_STACKS_DIRS=/srv/dockge/stacks` plus `MOORAGE_MANAGE_EXTERNAL=true`, again mounted at the same path. Moorage never reaches folders that aren't mounted into its container.

Not carried over:

- Dockge's `global.env` (in the stacks folder) isn't read. Copy the variables you need into each stack's `.env`.
- `compose.override.yml` files aren't loaded: Moorage deploys exactly one compose file per stack plus its `.env`. Merge the override into the main file.
- Folder names that aren't valid project names (uppercase letters, spaces) are skipped; rename the folder (and redeploy) to manage it.

## Uptime Kuma and Gatus

**Settings → Import → Uptime Kuma and Gatus monitors** reads an Uptime Kuma JSON backup (Kuma 1.x: **Settings → Backup → Export**) or a Gatus `config.yaml` (at most 5 MB). The preview lists the notification channels, checks and status pages it would create, what already exists, and everything that couldn't be converted with the reason. Untick what you don't want; **Create all checks paused** helps while the old tool still runs, so you don't get every alert twice.

### Uptime Kuma

| Kuma | Moorage |
| --- | --- |
| HTTP(s), HTTP(s) – Keyword, HTTP(s) – Json Query (simple paths like `data.status`) | HTTP check (GET or HEAD, accepted status codes, keyword / inverted keyword, JSON value, ignore TLS errors) |
| TCP Port | TCP check |
| Ping | Ping check |
| DNS (A, AAAA, CNAME, MX, TXT, NS) | DNS check with the same resolver |
| Interval, timeout, retries, paused state, notifications | Same; intervals below 20 s become 20 s, retries become "alert after retries + 1 failures" (1–10) |
| Discord, Slack, Telegram, ntfy (token), Gotify, Pushover, Matrix, SMTP, Webhook | Channel of the same type |

Not converted (listed in the report): groups (their monitors are imported on their own), Docker container monitors (add a Stack check instead), Push monitors, databases, gRPC, MQTT, game servers, browser monitors and other types; monitors with custom headers, a request body, authentication or other HTTP methods; upside-down mode; notification providers Moorage doesn't have; ntfy with user name and password. Webhooks keep their URL and headers, but Moorage sends its own JSON payload. **Status pages aren't part of a Kuma backup**, so none are imported.

### Gatus

| Gatus | Moorage |
| --- | --- |
| `http(s)://` endpoints | HTTP check: `[STATUS]` conditions (`==`, `<`, `>`, `any(…)`), `[BODY] == pat(*text*)` (or `!=`), one `[BODY].path == value`, `client.insecure`, `client.timeout` |
| `tcp://host:port`, `icmp://host` | TCP and Ping checks |
| DNS endpoints (`dns:` with A, AAAA, CNAME, MX, TXT, NS) | DNS check, `[BODY] == …` becomes the expected answer |
| `interval`, `enabled`, `alerts` with `failure-threshold` (or the provider's `default-alert`) | Interval, paused state, attached channels, "alert after N failures" |
| `alerting`: discord, slack, telegram, ntfy, gotify, pushover, matrix, email, custom | Channel named "Gatus <provider>" |
| The dashboard | One **unpublished** status page (slug `gatus`, title from `ui.header` / `ui.title`), sections from the endpoint groups |

Not converted: `[RESPONSE_TIME]`, `[CERTIFICATE_EXPIRATION]` and `[DOMAIN_EXPIRATION]` conditions (the check is imported with a warning that it tests less), other placeholders and functions (`len()`, …); endpoints with headers, a body, GraphQL or methods other than GET/HEAD; `ssh://`, `tls://`, `starttls://`, `udp://`, `ws://` and other endpoint types; external endpoints; values that use environment variables (`${VAR}`), alert group overrides and providers Moorage doesn't have; top-level settings like `storage`, `web` or `security` (named in the report).

### Secrets

Webhook URLs, tokens and passwords from the file go into the channels' settings, where Moorage keeps them when you add a channel by hand (shown masked afterwards, included in encrypted backups). The preview never shows them back, and they aren't logged or written to the audit log. The uploaded file isn't stored: the browser sends it again for the import.

### Running an import again

Imports are safe to repeat. Checks and notification channels are matched by **name**, status pages by **URL slug**:

- a check whose name exists is skipped;
- a channel whose name exists (same type) is reused, so new checks still get their alerts; with another type it is skipped;
- a status page whose slug is taken is skipped; a new page links to existing checks of the same name.

Existing objects are never changed, duplicated or deleted. Fix something in the source file and import again, and only what is still missing is created. A run that creates nothing changes nothing and isn't recorded in the audit log.
