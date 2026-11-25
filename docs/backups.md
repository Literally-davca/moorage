# Backups & Restore

Moorage protects two distinct layers:

1. **Hub configuration & stack definitions (`.mbk` encrypted archives)**
2. **Application Docker volumes (Restic sidecar add-on)**

## Manual `.mbk` Export & Restore

Open **Settings → Backups**:

- **Export `.mbk`**: enter an encryption password (at least 8 characters) to download an encrypted `.mbk` archive containing:
  - Hub SQLite database (`users`, `checks`, `channels`, `connections`, `shared_vars`, `status_pages`, `maintenance`, `audit_log`)
  - All managed stacks' `compose.yaml` + `.env`
  - Host `history.git` repositories
- **Check & preview / Restore**: pick a `.mbk` file and password to inspect its contents (hosts, stack count, overwrites) before applying. Database restores stage cleanly and apply on restart (keeping `moorage.db.bak`).

## Scheduled Remote Encrypted Backups (S3 / WebDAV)

In **Settings → Backups**, configure a schedule (daily `HH:MM`) and retention count to encrypt fresh `.mbk` snapshots automatically and upload them to:

- **S3-compatible storage**: Garage, Cloudflare R2, MinIO, AWS S3, Backblaze B2 S3
- **WebDAV**: Nextcloud, Hetzner Storage Box

Click **Backup & test now** to verify credentials immediately.

## Application Volume Backups (`restic-backup` add-on)

For live application data inside Docker volumes (databases, uploads, configs):

1. Enable/install **Restic Backup** from **Settings → Add-ons** (or directly from the card in **Settings → Backups**).
2. Configure your Restic repository URL, password, schedule, and target volumes in the sidecar's `.env` / compose stack inside Moorage.
