# Roadmap and acceptance gates

**Ordering:** validate the dashboard with one home camera, expand and verify it with three store cameras on the Windows PC, then move the reviewed system to the VPS. Estimates follow measured transport and media results. Product owner: Uzy's NY Pizza owner. Engineering owner: project developer/assistant working in this repository.

| Milestone | Deliverable | Exit gate |
|---|---|---|
| M0 — Workspace | Source, PRD, decisions, status, and repeatable commands in this repository. | A fresh editor/assistant can identify the next step without the old chat. |
| M1 — Store camera onboarding | Verify shop network, map three physical CG6 units to app/device/network identities, and test each through the Go bridge. | Three unambiguous mappings and per-camera connection/quality findings, or explicit blockers. |
| M2 — Clean host | Authorized CX23 rebuilt with Linux, SSH key access, host firewall, and no old-content backup. | CLI identifies server ID `138377676`; SSH and cloud-init checks pass. |
| M3 — Local CCTV | A browser dashboard shows the home test camera, records automatically to a rolling local archive, and plays video by date and time. | Owner can watch live, replay saved video after a restart, identify a recording gap, and select a time in History. |
| M3b — Local store CCTV | Store profile implemented; local short gate failed after two rounds. | Bounded on-site run proves simultaneous live feeds, per-camera saved playback and gap visibility under the short gate in the PRD. Recording stops after verification. [Local evidence](STATUS.md#windows-store-result--november-7). |
| M4 — Remote live CCTV | Move the reviewed system to the VPS and add the three store streams and private remote access. | Owner views all three from outside the store during a busy shift and can see when video is missing. |
| M5 — Seven-day history | Recorder, local queue, private Drive archive, index, history player. | Seek/play all seven days on every camera; segment integrity, outage recovery, and deletion verified. |
| M6 — Operations | Start-on-boot, health reporting, credential rotation, recovery runbook. | Simulated camera, VPS, and upload interruptions cause visible gaps and automatic recovery where possible. |
| M7 — Optional analytics | Person/zone trial, then station/team activity view. | Separate owner-approved PRD, measured compute budget, and manually reviewed output. |

## Immediate backlog

1. Validate Packet Monitor logging, export, and Wi-Fi frame decoding with a known synthetic UDP request/response. The first elevated Dining/Cash capture completed with insufficient capture coverage. After the synthetic probe passes, correlate a bounded pair capture against the socket trace and choose one host or transport change from that boundary. Follow the [Gemini implementation handoff](handoffs/GEMINI_VICOHOME_REA.md#next-diagnostic-after-the-counters-comparison). The owner ended VicoHome app testing on November 11.
2. Verify that the three store feeds advance together for five quiet minutes with the dashboard closed, then reopen Overview and verify live view and playback.
3. After the local gate passes and the owner reviews it, schedule a separate VPS migration and remote acceptance gate. Verify busy-shift coverage, seven-day history, and recovery before MVP acceptance.

## Decision trigger

If local recovery passes but the same bridge revision still fails the bounded VPS gate, isolate vendor session concurrency and the remote ICE path. Measure a small store-side gateway only if those results show the transport path is the remaining blocker. Record that outcome in a new ADR.
