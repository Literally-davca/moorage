# Security policy

## Reporting a vulnerability

Please report security problems **privately**, not in public issues or discussions:
use GitHub's private vulnerability reporting on this repository
(**Security → Report a vulnerability**).

Include what you found, the Moorage version (`moorage version` or **Hosts → Moorage updates**),
how it is deployed (all-in-one container, hub with agents, binary), and the steps to
reproduce. Proof-of-concept code is welcome; please don't test against instances you
don't own.

What to expect:

- an acknowledgement within a few days;
- an assessment and, for a confirmed issue, a fix in a new release; the fix and the
  advisory are published together, and you are credited unless you prefer not to be;
- please keep the issue private until the fixed release is out.

## Supported versions

Only the newest release gets security fixes. Moorage updates itself in a few clicks
(**Hosts → Moorage updates**), so please update before reporting.

## How Moorage approaches security

Moorage controls Docker, so **an admin of Moorage is root on every managed host**. The
public [threat model](docs/security/threat-model.md) explains what Moorage protects, from
whom, and which automated test guards each protection: the Docker socket, sign-in and
passkeys, cross-site WebSocket hijacking, SSRF (`netguard`), path traversal, public status
pages, agents, the MCP server and the release supply chain.

In short, run it behind HTTPS and a VPN or an SSO proxy, set `MOORAGE_PUBLIC_URL`, keep
the number of admins small and treat the data volume like a secret.

## Verifying a release

- Every release lists its binaries and image digests in `SHA256SUMS`, signed with the
  project's Ed25519 key (`SHA256SUMS.sig`). Moorage's self-updater checks this signature on
  every node before installing anything.
- The container image is also signed with **cosign** (keyless, through GitHub OIDC) and
  carries SBOM and build provenance attestations; SBOMs in SPDX and CycloneDX are attached
  to every release. The release notes contain the exact `cosign verify` and
  `gh attestation verify` commands for that release.
