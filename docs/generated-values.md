# Generated values (`@generate`)

Write a keyword instead of a password, key or port. When you **save** the stack, Moorage generates the value once, writes it to `.env` as an ordinary variable and puts `${KEY}` into `compose.yaml`. The stack keeps working with plain `docker compose`.

```yaml
services:
  db:
    image: postgres:17
    environment:
      POSTGRES_PASSWORD: "@generate(password, 32)"
  app:
    image: example/app:1
    environment:
      - DB_PASS=${POSTGRES_PASSWORD}
    ports:
      - "${SERVICE_PORT_WEB:-8080}:80"
```

After **Save**, the editor shows *Generated: POSTGRES_PASSWORD (password, 32) · SERVICE_PORT_WEB = 8081 (8080 is taken)*. Secret values are never shown in that note. They are in `.env`.

## Where keywords work

| Where | Example |
| --- | --- |
| `.env` | `APP_KEY=@generate(base64key)` |
| `environment:` mapping | `KEY: "@generate(hex, 32)"` |
| `environment:` list | `- KEY=@generate(password)` |
| host port in `ports:` | `- "@generate(port, 8080):80"` → `${SERVICE_PORT_<SERVICE>}` |

Anywhere else (`command:`, `labels:` …) the linter reports an error.

## Kinds

| Kind | Value |
| --- | --- |
| `password[, length][, symbols]` | 32 characters `[A-Za-z0-9]` by default; `symbols` adds punctuation that is safe in `.env` |
| `alnum[, length]` | letters and digits |
| `hex[, bytes]` | hex of 16 random bytes by default (32 characters) |
| `base64[, bytes]` | standard base64 of 32 random bytes by default |
| `base64key` | `base64:` + 32 random bytes (Laravel `APP_KEY`) |
| `uuid` | random UUID v4 |
| `user` | `u_` + 8 lowercase letters and digits |
| `port[, preferred]` | the nearest free host port at or after *preferred* |

Lengths are limited to 8–256. Values come from `crypto/rand`, without modulo bias. The same generator serves `generate:` in `template.yaml` and the MCP `generateSecrets` argument.

## Coolify variables

These names work as in Coolify (verified against the Coolify docs, 2026-11):

| Name | Value |
| --- | --- |
| `SERVICE_PASSWORD_<ID>`, `SERVICE_PASSWORD_64_<ID>` | password, 32 / 64 characters |
| `SERVICE_PASSWORDWITHSYMBOLS_<ID>`, `…_64_<ID>` | password with symbols |
| `SERVICE_USER_<ID>`, `SERVICE_LOWERCASEUSER_<ID>` | 16 random characters |
| `SERVICE_BASE64_<ID>`, `…_64_`, `…_128_` | 32 / 64 / 128 alphanumerics (not base64, as in Coolify) |
| `SERVICE_REALBASE64_<ID>`, `…_64_`, `…_128_` | base64 of 32 / 64 / 128 random bytes |
| `SERVICE_HEX_32_<ID>`, `…_64_`, `…_128_` | hex string of that many characters |
| `SERVICE_PORT_<ID>` (`${SERVICE_PORT_WEB:-8080}`) | free host port, preferring the default |
| `SERVICE_URL_<ID>[_<PORT>]`, `SERVICE_FQDN_<ID>` | from the host's default address (Settings → Hosts) |

Stacks that Coolify manages (`coolify.*` labels) are left alone: Coolify generates these values itself.

## Rules

- A value is generated only when `.env` doesn't have the key or still holds a keyword. **An existing value is never replaced**, because changing a database password would break the database.
- Deploys don't generate anything. The exception is a keyword that was written outside Moorage (file editor, git): it is generated first, saved as its own version, and then the deploy runs.
- Ports are chosen under a per-host lock from the port map (running containers, ports of all managed stacks, the host's own sockets). Ports below 1024 are skipped unless you asked for one. If nothing is free within 1000 ports of the preferred one, the save fails and points you to **Ports**.
- Generated values are in `.env`, and so in the stack history, like passwords you type yourself.
- Read-only hosts and Moorage's own stack never generate.

## Quick fixes

- **Use a free port**: on *Host port … is already used*, replaces the host port with `@generate(port, <port>)`.
- **Add variables**: popular images (Postgres, MariaDB/MySQL, Mongo, MinIO, LinuxServer images, authentik) get a hint with the variables they need, inserted as `@generate(...)` keywords or fixed defaults (`PUID=1000`). Services that already set a variable get no hint for it.
