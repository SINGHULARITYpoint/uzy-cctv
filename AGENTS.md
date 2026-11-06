# Working context for coding assistants

This is the canonical, editor-neutral instruction file for UZY CCTV. Read `README.md`, `docs/STATUS.md`, `docs/PRD.md`, and `docs/ARCHITECTURE.md` before changing scope or deploying.

## Goal and order

Build an owner-only CCTV/NVR service for three owned GMK CG6 cameras: secure remote live view, then seven-day playback. Only after these pass their acceptance gates should work begin on person detection and station/team activity. The owner prefers an incremental learning path and clear explanations of real constraints.

## Evidence boundaries

- A 35-minute powered home-lab WebRTC session worked. It does not prove 24/7 reliability or VPS-to-store transport.
- H.264 output from the long probe has decode errors. The packet reassembly and reconnect path require more work.
- A one-minute follow-up selected a relay ICE path even on the home LAN. Do not assume a direct LAN path or a dependable remote relay.
- The VPS was identified but had not been rebuilt at the last verified check. Check live state before acting.
- Google Drive is proposed for the seven-day archive; upload, indexing, retrieval, and playback are not yet implemented.
- The immediate next task is on-site mapping and short connection tests for the three store cameras. The historical DHCP/MAC notes in ignored `local/private/store-network.md` must be rechecked on the current shop network. Defer the Hetzner token handoff and rebuild until store onboarding is recorded.

## Implementation rules

- Treat `docs/PRD.md` as the MVP acceptance contract and keep `docs/STATUS.md` current after tests or deployment.
- Keep camera ingest, restream, recording, archive, playback, and later analytics as separable components. Do not introduce AI dependencies into the CCTV critical path.
- Prefer H.264 remuxing over transcoding on the 2-vCPU CX23 until measurement supports a different choice.
- Mark missing video as a recording outage, never as worker inactivity.
- Use a dedicated VicoHome bridge account/session. A lab test showed a new CLI login can invalidate the app's session.
- No credentials, API tokens, Wi-Fi passwords, serials, private footage, or account logs in committed files. Use environment variables or an untracked secrets file with narrow access.
- Preserve upstream MIT attribution in `services/vico-bridge/LICENSE.md`.

## VPS authorization and safety

The owner explicitly authorized a clean rebuild of the dedicated Hetzner CX23 `uzy-bridge` (server ID `138377676`) and explicitly declined a backup of its old contents. Verify the server ID, name, type, and attached resources through the CLI before issuing a rebuild. The prior session created a project API token but could not hand it into the CLI; the last verified `hcloud context list` was empty. Do not claim the VPS was wiped until a fresh CLI check proves it.

## Useful commands

Run Go commands from `services/vico-bridge`. The local lab script prompts for credentials and writes only to ignored `local/`. See `scripts/README.md`. Keep any new tests focused on the real transport and recording risks.
