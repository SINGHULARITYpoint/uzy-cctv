# Roadmap and acceptance gates

**Ordering:** finish one gate before adding the next layer. Estimates follow measured transport and media results, not calendar promises. Product owner: Uzy's NY Pizza owner. Engineering owner: project developer/assistant working in this repository.

| Milestone | Deliverable | Exit gate |
|---|---|---|
| M0 — Workspace | Source, PRD, decisions, status, and repeatable commands in this repository. | A fresh editor/assistant can identify the next step without the old chat. |
| M1 — Clean host | Authorized CX23 rebuilt with Linux, SSH key access, host firewall, and no old-content backup. | CLI identifies server ID `138377676`; SSH and cloud-init checks pass. |
| M2 — One remote camera | Lab CG6 streams to the VPS from another network with clean H.264 output and reconnect. | Decoded recording is clean; ICE route, gaps, bitrate, and recovery are measured. |
| M3 — Live CCTV | Three store feeds and private live dashboard. | Owner views all three from outside the store during a busy shift; offline state is obvious. |
| M4 — Seven-day history | Recorder, local queue, private Drive archive, index, history player. | Seek/play all seven days on every camera; segment integrity, outage recovery, and deletion verified. |
| M5 — Operations | Start-on-boot, health reporting, credential rotation, recovery runbook. | Simulated camera, VPS, and upload interruptions cause visible gaps and automatic recovery where possible. |
| M6 — Optional analytics | Person/zone trial, then station/team activity view. | Separate owner-approved PRD, measured compute budget, and manually reviewed output. |

## Immediate backlog

1. Verify `hcloud context list` and server state. The context was empty at last check; the owner was given a one-time token handoff instruction.
2. Build `infra/generated/cloud-init.yaml` from the local SSH public key; confirm target server ID/name/type/attached resources; rebuild with Ubuntu LTS and verify SSH.
3. Reproduce and fix H.264 decode errors, especially the transition from 640×360 preview to 2304×1296 main stream and RTP packet loss.
4. Exercise the retry path that was changed after a failed immediate restart.
5. Run one-camera remote path test on the VPS; capture selected ICE pair and clean media metrics.
6. Choose live restream/playback components based on the measured stream, then implement dashboard and archive.

## Decision trigger

If M2 cannot produce a stable clean remote stream, do not build a dashboard on top of it. Measure whether a small store-side gateway that keeps the VPS as the application host resolves the transport problem. Record the outcome in a new ADR.
