# Feature R8: Podman support

Read `prompts/roadmap-agent-common.md` first; it applies fully. Already in the code: R1–R7.

## Goal
Run Moorage (all-in-one or agent) against Podman's Docker-compatible API socket (`podman system service`, `podman.socket`), rootful and rootless. This widens the audience to Fedora/RHEL and rootless servers.

## Important: environment
- Podman is **not installed** in the dev WSL, and installing it needs `sudo` with a password, which you don't have. Do not try to install it.
- Work from:
  - the Podman Docker-compat API documentation (https://docs.podman.io/en/latest/_static/api.html, the "compat" endpoints) and known differences;
  - fakes in tests;
  - explicit detection code.
- Mark every behaviour you could not verify against a real Podman as **unverified** in your report. The lead will test with a real Podman later.

## Required design
- **Detection:** at engine start, read the server version components (`/version` returns a "Podman Engine" component) and `Info`. Report in `HostInfo`:
  - `engine: docker|podman`;
  - podman version;
  - rootless yes/no.

  The UI shows "Podman 5.x (rootless)" where it shows the Docker version today.
- **Socket:** `DOCKER_HOST=unix:///run/podman/podman.sock` (rootful) or `unix://$XDG_RUNTIME_DIR/podman/podman.sock` (rootless). Document both, plus the systemd unit (`systemctl --user enable --now podman.socket`) and `loginctl enable-linger` for rootless agents. No auto-discovery magic beyond `DOCKER_HOST`; say if you add a fallback probe of the standard paths and why.
- **Review every engine feature against Podman's compat API** and make each one work or degrade clearly. Make a table in your report: feature → Docker → Podman behaviour → what you changed. At least:
  - compose project discovery by labels (`com.docker.compose.project`): set by compose-go, should work; verify the label filters you use;
  - `docker compose` operations through the compose library (up/down/pull/restart, `pull_policy`, `up-local` from M8): known Podman gaps (healthcheck `depends_on: condition: service_healthy`, `network_mode`, profiles);
  - **stats**: Podman's compat stats differ (CPU fields, `memory_stats` for rootless cgroups v1/v2 can be empty). Never show garbage: show "–" with a reason;
  - **events** used for log watching / container state;
  - **logs** with timestamps and the multiplexed stream (TTY vs non-TTY);
  - **exec / terminal (M9)**: the compat exec + resize + the reaper exec (`/proc/*/environ` marker; rootless PID namespace);
  - **image digests** (M8): `RepoDigests` format and `ImageInspect` differences, registry checks unchanged;
  - **engine ID** (no stable ID on Podman; anything that relied on it must use another key);
  - networks for the R5 dependency detection (Podman network names and `external` handling).
- **Rootless specifics:**
  - ports < 1024 fail without sysctl, so show the Podman error clearly;
  - user-namespace file ownership in the stacks folder (`MOORAGE_PUID/PGID` behaviour);
  - no `/var/run/docker.sock` mount for the agent container: document running the agent as a user service or container with the user socket.
- **Testing matrix** (document it in the report; the lead runs it):
  - Podman 4.9 (Ubuntu 24.04 package) and 5.x;
  - rootful and rootless;
  - the dev stacks `whoami`/`crashy` equivalents;
  - each feature from the table.

  Add a `scripts/dev/podman-smoke.sh` that runs a throwaway all-in-one against a given `DOCKER_HOST` on ports 5095/5096 and exercises the API (create stack, up, logs, stats, exec, down, delete) with curl, printing PASS/FAIL per step. It must clean up after itself.
- **Code:**
  - Keep the Docker path untouched where possible.
  - Branch on `engine == podman` only where behaviour really differs, in small named helpers with a comment linking the Podman doc or issue.

## Tests (at least)
- Detection from recorded `/version` payloads (Docker 29, Podman 4.9 rootful, Podman 5 rootless). Put recorded JSON fixtures in `internal/engine/testdata/podman/` and write them from the official API docs examples; say which are synthetic.
- Stats parsing with Podman-shaped payloads (including empty memory stats) shows "–" and not 0 or NaN.
- Anything else that can be unit-tested with fakes.
- All existing tests pass.

## Out of scope
- Podman pods.
- Quadlet / systemd unit generation.
- Kubernetes YAML (`podman kube play`).
- Buildah.
