# Problems and snapshots

## Problems card

When a container of a stack isn't well, the stack's **Containers** tab shows a **Problems** card above the list and a badge on the container's row. Each problem says what is wrong, shows the evidence (exit code, healthcheck output, the first error line of the last run) and offers fixes. Fixes are existing actions with a preview: a change to `compose.yaml` opens a diff with **Save & redeploy**. Nothing runs without your click.

| Problem | Evidence | Fix |
| --- | --- | --- |
| Out of memory | OOM kill, exit 137, memory limit, how often | Raise memory limit (diff) |
| Crash loop | restarts in the last 10 min | the cause from the rules below |
| Command not found / wrong architecture | exit 126/127, `exec format error`, image vs. host architecture | edit the command, choose another image |
| Finished but restarted | exit 0 with `restart: always` | `restart: "no"` |
| Unhealthy | last healthcheck output | another healthcheck when the tool is missing |
| Port already in use | who holds the port | Use a free port, open Ports |
| Missing variable | the message naming it | add it to `.env` |
| Permission denied | the path | terminal, `user:` / `PUID` |
| Image can't be pulled | missing tag, login needed, Docker Hub limit | choose a tag, registry login |
| No space left on device | free space | disk cleanup |
| A dependency is down | connection refused / DNS errors in the log | dependency graph |
| Never started / dead | `State.Error` | restart |
| Started after a change | a version or update within 15 min before | Roll back |

Problems can be ignored per service (for example an intentional one-off job). The card adds no alerts. The **Detailed** notification preset and incidents get one line, *Likely cause: Out of memory (exit 137)*, never log lines. Status pages never show it. Older agents give a basic diagnosis from the container list.

## Snapshots

**Stack → Snapshots → Take snapshot** saves the stack's volumes, the bind mounts inside the stack folder and `compose.yaml` + `.env` into one compressed file on the host (`<data>/snapshots/<stack>/<time>-<id>.tar.zst`, with `manifest.json` inside and next to it). Bind mounts outside the stack folder are listed with their size, unchecked; tick the ones you want (the choice is remembered).

- **Consistency:** *Pause during copy* (default, like a power cut: databases recover from their journal), *Stop, copy, start*, or *Live* (you confirm that a database may not restore). A pause always ends, also when Moorage restarts during the copy.
- **Copying** is done by a short-lived helper container from Moorage's own image, with the volumes mounted read-only, so Moorage's memory use doesn't change.
- **Restore:** preview (what is overwritten, the configuration diff), choose *Data + configuration*, *Data only* or *Configuration only*. A *pre-restore* snapshot is taken first (you can turn it off). Then the stack is stopped, restored, the configuration saved as a new version, and started.
- **Download:** admins can download a snapshot. It contains `.env` and application data, unencrypted.
- **Snapshot before updates:** per stack, off by default. When on, every image update (automatic or manual) first takes a snapshot; if that fails, the update waits and the Logbook says why.
- **Retention:** the newest 5 automatic snapshots per stack are kept; manual ones until you delete them (pin to keep any). A snapshot is refused when less than 10 % of the disk would stay free. Both limits can be changed in the Snapshots tab.

Not available for Moorage's own stack (the `.mbk` backup covers it) or read-only hosts. Agents need an update to support snapshots. For long-term backups off this server use the `restic-backup` add-on.
