# Troubleshooting

Common problems when running Moorage, what the log says and how to fix them. Read the log with `docker logs moorage-moorage-1` (or `docker compose logs moorage` in the stack folder).

## Moorage keeps restarting

The error is on the `moorage stopped` line. An `OIDC provider unreachable … context canceled` line right before or after it is only a side effect of the shutdown, not the cause.

### `listen tcp: address 5080: missing port in address`

`MOORAGE_LISTEN` (or `MOORAGE_AGENT_LISTEN`) holds a bare port number. It needs an address with a colon. In `.env` or `compose.yaml`:

```
MOORAGE_LISTEN=:5080
MOORAGE_AGENT_LISTEN=:5081
```

`:5080` listens on every address, `127.0.0.1:5080` only on the loopback. With `ports:` in bridge mode, keep `:5080` inside the container and limit access in the port mapping (`127.0.0.1:5080:5080`).

### `listen tcp :5080: bind: address already in use`

Another process holds the port. This usually appears after switching to `network_mode: host`. In bridge mode, Docker publishes only the mapped address (for example `127.0.0.1:5080`). In host mode, Moorage binds the port on every address of the host, so any other listener on that port blocks it, even one on a single address.

Find who holds the port:

```sh
sudo ss -ltnp | grep -E ':508[0-2]\b'
docker ps -a --format '{{.Names}}\t{{.Status}}\t{{.Ports}}' | grep -E '508[0-2]'
```

- **`docker-proxy`**: a container publishes the port. The second command shows which one, for example an old Moorage container from another project folder.
- **`tailscaled`** on the Tailscale address (`100.x.y.z%tailscale0:5080`): `tailscale serve` forwards that address to Moorage. Let Moorage listen on the loopback only, so both fit:

  ```
  MOORAGE_LISTEN=127.0.0.1:5080
  MOORAGE_AGENT_LISTEN=127.0.0.1:5081
  ```

  Check with `tailscale serve status` that it forwards to `127.0.0.1` or `localhost`. The agent port must be forwarded as plain TCP (`tcp://…`), not HTTPS: agents use mutual TLS with a client certificate, which an HTTPS proxy cuts off.
- **another process** (`moorage`, a web server…): stop it, or move Moorage to a free port with `MOORAGE_LISTEN=127.0.0.1:5090`.

Then run `docker compose up -d` in the stack folder.

## Apply & restart "rolled back", but Moorage is still down

The helper restores the files that were on disk when you clicked **Apply & restart**. If you clicked **Save** (or pressed Ctrl+S) with the broken change first, those saved files are what it restores, so Moorage stays down even though the result says it rolled back.

Recover on the host:

1. Read `<data>/self-stack-apply.log` and the `moorage stopped` line in the log to see what fails.
2. Fix `compose.yaml` / `.env` in the stack folder (for example `/opt/stacks/moorage`). The files of the last attempt are in `<data>/self-stack-prev/`; the stack history in Moorage has earlier versions once it runs again.
3. Run `docker compose up -d`.

To avoid it, change Moorage's own stack with **Apply & restart** only, without **Save** first. See [Apply & restart of Moorage's own stack](self-stack.md).

## The image check found a new Moorage, but nothing was updated

Moorage never updates its own stack through the image check or an auto-update policy. Those only notify. Install new versions in **Hosts → Moorage updates**: it checks the release signature and rolls back by itself if the new version doesn't come back.

Moorage updates asks GitHub for new releases once a day, so it can show the new version later than the image check. Click **Check now** there.

If you run Moorage with Compose and a fixed version tag, set the new tag in your compose file too; otherwise the next `docker compose up` goes back to the old one.

## Manual updates only

With `MOORAGE_SELF_UPDATE=false` on a host, nothing outside that host can change which Moorage version it runs. Moorage refuses:

- **Moorage updates** and **Update Moorage** for that host (shown as *Manual updates only*; **Update all** skips it),
- **Pull** of the stack Moorage runs from,
- **Apply & restart** of that stack when it changes the Moorage `image:` (other changes still apply),
- the same change proposed by the assistant.

Update it on the host itself:

```sh
docker compose pull && docker compose up -d
```

The setting is read from the environment only, so the hub can't switch it off. Remove the variable (or set it to `true`) to allow updates from the UI again.

## "Saved, not applied" on Moorage's own stack

You saved changes to the stack Moorage runs from, but Moorage still runs the previous configuration. **Apply & restart** uses them. If the new configuration doesn't come back, Moorage returns to the configuration it last ran with, not to the files you saved. If Moorage has never run from the files on disk since the update to this version, the confirmation says that no rollback is available; fix problems on the host as described in [Apply & restart rolled back, but Moorage is still down](#apply--restart-rolled-back-but-moorage-is-still-down).
