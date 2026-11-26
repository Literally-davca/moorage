# Logging

Moorage writes its log to stderr, so `docker logs moorage` (or the journal of a native install) shows it.

## Settings

| Variable | Values | Default |
| --- | --- | --- |
| `MOORAGE_LOG_LEVEL` | `debug`, `info`, `warn`, `error` | `info` |
| `MOORAGE_LOG_FORMAT` | `text`, `json` (for Loki/Promtail, Vector…) | `text` |

An unknown value falls back to the default and logs a warning at start. Agents and the Apply & restart helper use the same settings.

## Levels

- **error** – Moorage couldn't do something and won't fix it by itself: a backup failed, an audit row or a history version wasn't written, check results or incidents weren't stored, an automatic repair failed (image rollback, auto-rollback, Rollback & Verify, auto-update, self-update rolled back at boot), a snapshot restore failed, a passkey's sign counter went back (possibly a cloned authenticator), the static status page snapshot failed 3 times in a row, and every API answer 5xx (`request failed` with route, host, stack, status, duration and cause).
- **warn** – something to look at that may fix itself or is expected: a check is down, an agent lost its connection and retries, a notification wasn't delivered, a compose operation failed, refused sign-ins, configuration that is ignored.
- **info** – what happened: start, listeners, sign-ins, changes to stacks, checks, channels and status pages, terminals, agents, compose operations, snapshots, assistant actions, MCP tool calls, generated values (key names only).
- **debug** – every API request (`request` with route, status and duration), stream ends, live connections closed after a permission change.

## Security events

Feed these to CrowdSec, fail2ban or your log alerting:

| Message | Fields |
| --- | --- |
| `sign-in failed` | `ip`, `method` (password, password+passkey, passkey, link), `reason`, `user` (only when the account exists) |
| `sign-in rate limited` | `ip`, `method` |
| `signed in` | `user`, `ip`, `method` |
| `passkey sign counter went back…` | `passkey`, `user_id` |
| `terminal session started` / `host terminal session started` | user, host, stack or container |

A fail2ban filter for the text format:

```ini
[Definition]
failregex = msg="sign-in (failed|rate limited)".* ip=<HOST>
```

## Never in the log

`.env` values, passwords, tokens, API keys, join tokens, the assistant's conversation, container log lines, HTTP bodies of checks, and request query strings or bodies. `TestLogNeverContainsSecrets` guards the common paths. The setup token at first start is the one exception: it is printed on purpose so you can open Moorage.

Audit rows (Settings → Audit) and the Logbook keep the history of changes; the log is for operations and troubleshooting. Rotation is left to Docker (`--log-opt max-size`) or journald.
