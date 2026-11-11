# Working context for coding assistants

This is the canonical, editor-neutral instruction file for UZY CCTV. Read `README.md`, `docs/STATUS.md`, `docs/PRD.md`, and `docs/ARCHITECTURE.md` before changing scope or deploying.

## Goal and order

Build an owner-only CCTV/NVR service for three owned GMK CG6 cameras: secure remote live view, then seven-day playback. Only after these pass their acceptance gates should work begin on person detection and station/team activity. The owner prefers an incremental learning path and clear explanations of real constraints.

Build and validate the working CCTV system on this Windows PC first. The single home camera must support live viewing, automatic recording to disk, and playback by time in the same dashboard. Use deterministic local tests and the home camera for regression checks. With the owner now on site, extend and verify the same Windows workflow with all three store cameras. Move the reviewed system to the VPS after the local store gate. Verify remote transport and concurrency separately there.

## Evidence boundaries

- A 35-minute powered home-lab WebRTC session worked. It does not prove 24/7 reliability or VPS-to-store transport.
- The old long H.264 probe had decode errors. New short captures from all three store cameras and one remote VPS capture fully decoded after increasing RTP reassembly depth. This does not prove continuous quality; the later three-camera go2rtc overlap left recording gaps.
- A one-minute follow-up selected a relay ICE path even on the home LAN. Do not assume a direct LAN path or a dependable remote relay.
- The dedicated VPS was cleanly rebuilt with Ubuntu 24.04 on November 6, 2026. A private loopback-only go2rtc service is installed; check live state before acting.
- MediaMTX is configured for a seven-day rolling home archive on this PC. The owner stopped home recording after the November 6 session. Google Drive is proposed for the later remote archive.
- Store mapping, short probes, VPS bootstrap, and remote concurrency failures are documented. The immediate gate is the bounded local store test for three-camera live view, recording, and playback. Exact camera/network identities remain in ignored `local/private/`.

## Implementation rules

- Treat `docs/PRD.md` as the MVP acceptance contract and keep `docs/STATUS.md` current after tests or deployment.
- The owner has ruled out further VicoHome app live-view and multi-camera tests. Build and verify concurrent feeds through our Go bridge and UZY dashboard. Use the existing offline REA artifacts for protocol research. App sign-in, feature access, subscriptions, and emulator setup are not prerequisites for this work.
- Do not ask the owner to plug in Kitchen Back, propose a Kitchen power test, or make its external power a prerequisite. Record power readings as test context. Investigate three-camera continuity with the existing power arrangement.
- Keep local product evidence separate from deployment evidence. Run focused tests and verify the local dashboard before promoting the integrated system after owner review.
- Keep camera ingest, restream, recording, archive, playback, and later analytics as separable components. Do not introduce AI dependencies into the CCTV critical path.
- Prefer H.264 remuxing over transcoding on the 2-vCPU CX23 until measurement supports a different choice.
- Mark missing video as a recording outage, never as worker inactivity.
- Use a dedicated VicoHome bridge account/session. A lab test showed a new CLI login can invalidate the app's session.
- No credentials, API tokens, Wi-Fi passwords, serials, private footage, or account logs in committed files. Use environment variables or an untracked secrets file with narrow access.
- Preserve upstream MIT attribution in `services/vico-bridge/LICENSE.md`.

## VPS authorization and safety

The owner authorized and completed a clean rebuild of dedicated Hetzner CX23 `uzy-bridge` (server ID `138377676`) without a backup of its old contents on November 6, 2026. CLI context `uzy-cctv` is active. The one-time token file was deleted after verification. Recheck live state before any further server mutation.

## Useful commands

Run Go commands from `services/vico-bridge`. The local lab script prompts for credentials and writes only to ignored `local/`. See `scripts/README.md`. Keep any new tests focused on the real transport and recording risks.
