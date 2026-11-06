# PRD: UZY CCTV MVP

**Status:** Draft for owner review
**Version:** 0.1
**Updated:** November 6, 2026
**Owner:** Uzy's NY Pizza owner
**Delivery owner:** Project engineering

## Product promise

From home or while traveling, the owner can securely open one dashboard, see the three store cameras live, and find and play footage from any point in the previous seven days. The system makes recording outages obvious. It runs without a VicoHome cloud-recording subscription, although the current camera bridge still depends on VicoHome for authentication, ticket issuance, and signaling.

## Problem and evidence

The store already owns three GMK CG6 battery cameras. VicoHome's subscription model is costly for continuous business recording, and its battery-camera recording workflow is motion based. A spare camera in the home lab sustained an independently initiated live WebRTC stream for 35 minutes while powered. That shows a possible path but does not establish clean continuous video or remote VPS transport. The current recording prototype has H.264 decode errors. [Current test status](STATUS.md) separates proven results from assumptions.

## Primary user and jobs

**Primary user:** the owner, using a laptop or phone outside the store.

1. Open a private dashboard and see whether all three cameras are online.
2. Watch one camera live, full screen, without handling VicoHome app sessions manually.
3. Select a camera and date/time within the last seven days and play continuous footage.
4. Distinguish a quiet scene from a camera or recorder outage.

## MVP scope

| ID | Priority | Requirement | Acceptance evidence |
|---|---|---|---|
| F1 | P0 | Connect the lab CG6 to the VPS across networks; reconnect after camera sleep, network loss, or process restart. | One remote test records ICE path, packet gaps, restart behavior, and decoded-frame quality. |
| F2 | P0 | Ingest all three store feeds concurrently. | An unattended busy shift has playable footage for each camera; every outage is timed and shown. |
| F3 | P0 | Owner-only remote access. | Authorized laptop and phone can open the dashboard from outside store Wi-Fi; unauthenticated users cannot reach a feed. |
| F4 | P0 | Three-camera live overview and single-camera full-screen view. | Owner can select each feed; online/offline state and last-frame time are visible. |
| F5 | P0 | Continuous H.264 recording with accurate timestamps. | A full decode check on sampled segments reports no damaged frames; segment timestamps remain monotonic across restarts. |
| F6 | P0 | Seven-day history by camera, date, and time. | Owner can seek to and play samples from day 1 and day 7, including across segment boundaries. |
| F7 | P0 | Durable archive and retention. | Completed segments are uploaded, verified, indexed, retrievable, and removed after seven days; failed uploads stay queued and visible. |
| F8 | P0 | Health and recovery. | Dashboard shows stale streams, recording gaps, queue pressure, and upload failures; services resume after VPS reboot. |
| F9 | P1 | Export a selected clip. | Owner can download a bounded, timestamped clip without manually locating archive files. |

## Quality targets

- **Completeness:** every camera/time interval in the retention window is either playable or explicitly marked as a recording gap. Never fabricate coverage.
- **Live usability:** target under 15 seconds to show a waking camera; measure actual startup and steady-state latency on the remote path.
- **Playback usability:** selecting a time should start a valid segment without manual Drive navigation. Measure recent and oldest-day fetch times before setting a hard service target.
- **Continuity:** pass a 24-hour powered lab run and a full busy store shift before declaring unattended capture ready. A seven-day archive trial is required for the full MVP.
- **Capacity:** measure CPU, memory, ingress, egress, queue size, and bitrate for one and then three feeds on the CX23. Do not assume it can transcode or run AI alongside capture.
- **Privacy:** owner-only access, encrypted remote access, minimal logs, no audio recording in the MVP unless the owner explicitly chooses it after reviewing local requirements. Confirm applicable employee notice and camera placement requirements before store rollout.

These are engineering gates, not claims that the current prototype passes them.

## MVP completion test

From a network outside the store, the owner opens the private dashboard on a laptop and phone, watches all three live feeds, seeks to multiple times on each of the prior seven days, and plays footage across segment boundaries. Engineering verifies the same timestamps against stored segments, exercises a camera disconnect, VPS restart, and temporary Drive upload outage, and confirms every resulting gap or queue state is visible. No damaged H.264 sample or silent missing interval is accepted.

## Explicitly later

- Person detection, work-zone polygons, movement scoring, and station/team activity dashboard.
- Named employee identification, facial recognition, automated productivity verdicts, POS correlation, and alerts about worker behavior.
- Public multi-user sharing, paid subscriptions, or an app-store mobile app.

The first later analytics metric, if approved, is visible station/team activity from zone presence plus movement. It must link back to video and mark missing footage as unknown.

## Dependencies and risks

1. **Vendor bridge:** CG6 live access currently requires VicoHome authentication, ticket issuance, and WebSocket signaling. This is a compatibility integration, not firmware independence.
2. **Remote transport:** a VPS-to-camera path has not been tested. The vendor relay might be unreliable or rate limited. A small store gateway is a fallback if measured results demand it.
3. **Media integrity:** the current raw H.264 recorder has decode errors. Correct RTP frame assembly, timestamps, and stream transitions are the first engineering work.
4. **Storage:** the CX23 has only 40 GB local disk. Google Drive has ample reported capacity but is not yet proven as an automated playback archive. A local queue or extra volume may be needed for upload outages.
5. **Compute:** two shared vCPUs and 4 GB RAM require a lean, remux-first design. AI is a separate capacity decision.

## Open product decisions

- Does the owner want audio at all? Default for MVP: video only.
- Is private device access through Tailscale sufficient for the first dashboard, or is a separate web login required? Default: Tailscale first.
- What camera names and positions should appear in the dashboard? Collect after live feeds work.
