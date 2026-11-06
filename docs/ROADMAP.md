# Roadmap and acceptance gates

**Ordering:** finish one gate before adding the next layer. Estimates follow measured transport and media results, not calendar promises. Product owner: Uzy's NY Pizza owner. Engineering owner: project developer/assistant working in this repository.

| Milestone | Deliverable | Exit gate |
|---|---|---|
| M0 — Workspace | Source, PRD, decisions, status, and repeatable commands in this repository. | A fresh editor/assistant can identify the next step without the old chat. |
| M1 — Store camera onboarding | Verify shop network, map three physical CG6 units to app/device/network identities, and test each through the Go bridge. | Three unambiguous mappings and per-camera connection/quality findings, or explicit blockers. |
| M2 — Clean host | Authorized CX23 rebuilt with Linux, SSH key access, host firewall, and no old-content backup. | CLI identifies server ID `138377676`; SSH and cloud-init checks pass. |
| M3 — One remote camera | A CG6 streams to the VPS from another network with clean H.264 output and reconnect. | Decoded recording is clean; ICE route, gaps, bitrate, and recovery are measured. |
| M4 — Live CCTV | Three store feeds and private live dashboard. | Owner views all three from outside the store during a busy shift; offline state is obvious. |
| M5 — Seven-day history | Recorder, local queue, private Drive archive, index, history player. | Seek/play all seven days on every camera; segment integrity, outage recovery, and deletion verified. |
| M6 — Operations | Start-on-boot, health reporting, credential rotation, recovery runbook. | Simulated camera, VPS, and upload interruptions cause visible gaps and automatic recovery where possible. |
| M7 — Optional analytics | Person/zone trial, then station/team activity view. | Separate owner-approved PRD, measured compute budget, and manually reviewed output. |

## Immediate backlog

1. While the owner is at the shop, follow the [store camera onboarding handoff](handoffs/STORE_CAMERA_ONBOARDING.md): verify Wi-Fi, map physical units, and test each owned camera through the existing bridge without changing its production configuration.
2. Reproduce and fix H.264 decode errors, especially the transition from 640×360 preview to 2304×1296 main stream and RTP packet loss. Exercise the retry path changed after a failed immediate restart.
3. Verify `hcloud context list` and server state. The context was empty at last check; the owner was given a one-time token handoff instruction.
4. Build `infra/generated/cloud-init.yaml` from the local SSH public key; confirm target server ID/name/type/attached resources; rebuild with Ubuntu LTS and verify SSH.
5. Run one-camera remote path test on the VPS; capture selected ICE pair and clean media metrics.
6. Choose live restream/playback components based on the measured stream, then implement dashboard and archive.

## Decision trigger

If M2 cannot produce a stable clean remote stream, do not build a dashboard on top of it. Measure whether a small store-side gateway that keeps the VPS as the application host resolves the transport problem. Record the outcome in a new ADR.
