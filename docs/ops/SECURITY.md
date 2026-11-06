# Security and privacy notes

## Assets

Camera footage may show employees and customers. VicoHome credentials, session tokens, Hetzner API tokens, Google OAuth grants, SSH private keys, and Wi-Fi credentials provide access to systems or footage. Treat these as secrets even during development.

## Workspace handling

- Never commit footage, auth caches, raw app logs, packet captures, environment files, or generated cloud-init with private data. `.gitignore` blocks common artifacts; inspect `git status` before every commit.
- The lab script prompts for VicoHome credentials at runtime and removes its auth cache at exit. Do not put credentials in a PowerShell command line or a checked-in `.env` file.
- Keep redacted test summaries and aggregate metrics in documentation. Device serials and short-lived WebRTC credentials do not belong in reports.
- The Hetzner project token is for bootstrap work. After the server setup, decide whether to retain it in the local CLI context or revoke/rotate it. Never put it on the VPS.

## Service boundaries

- Keep live camera/restream endpoints private. Start with authenticated encrypted owner-device access such as Tailscale; do not expose raw RTSP/WebRTC endpoints publicly.
- Give the archive a private Drive folder and narrow OAuth scope where practical. Verify who owns the folder and how access is granted before uploading footage.
- Use key-only SSH and a host firewall. Monitor the disk queue so failed cloud uploads cannot silently consume the system disk.
- Default to video only. Confirm employee notice, camera placement, retention, and audio rules for the store's jurisdiction before production rollout. This document does not assert a particular legal rule.

## Incident basics

If an account or token is exposed: revoke it at the provider, rotate credentials, invalidate active sessions, and check access logs. If a camera or archive is inaccessible: mark the interval as a recording gap; never show a stale frame as live.
