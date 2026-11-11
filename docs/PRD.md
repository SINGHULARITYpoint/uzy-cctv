# PRD: UZY CCTV MVP

**Status:** Active engineering contract
**Version:** 0.6
**Updated:** November 7, 2026
**Owner:** Uzy's NY Pizza owner
**Delivery owner:** Project engineering

## Product promise

From home or while traveling, the owner can securely open one dashboard, see the three store cameras live, and find and play footage from any point in the previous seven days. The system makes recording outages obvious. It runs without a VicoHome cloud-recording subscription, although the current camera bridge still depends on VicoHome for authentication, ticket issuance, and signaling.

## Problem and evidence

The store already owns three GMK CG6 battery cameras. VicoHome's subscription model is costly for continuous business recording, and its battery-camera recording workflow is motion based. A spare camera in the home lab sustained an independently initiated live WebRTC stream for 35 minutes while powered. New short store captures and one remote VPS capture decode cleanly. Two instrumented three-camera VPS rounds produced bridge-output gaps while RTP continued, so continuous coverage is not yet established. [Current test status](STATUS.md) separates proven results from assumptions.

## Delivery strategy

Build and validate the CCTV workflow on the Windows project PC. The single-home-camera prototype has demonstrated live view, automatic recording, time-based playback, export across segment boundaries, restart persistence, and a visible gap. Seven-day retention is configured and was checked with aged fixtures. Recording was stopped at the owner's request at the end of the November 6 session.

The current milestone brings Dining Area, Cash Register, and Kitchen Back into that same dashboard on the Windows PC at the store. Use bounded on-site recordings to prove simultaneous live viewing, correctly attributed storage, and playback by camera and time. The short gate requires at least 20 seconds of shared playable coverage at 2304×1296 per camera, a full decode with zero video errors, and a maximum observed reader and saved timestamp gap of two seconds. Report startup, gaps, reassembly drops, and retries. Stop recording after the test. Owner review and a separate store-to-VPS acceptance run follow this milestone.

## Primary user and jobs

**Primary user:** the owner, using a laptop or phone outside the store.

1. Open a private dashboard and see whether all three cameras are online.
2. Watch one camera live, full screen, without handling VicoHome app sessions manually.
3. Select a camera and date/time within the last seven days and play continuous footage.
4. Distinguish a quiet scene from a camera or recorder outage.

## Dashboard layout

The owner’s reference sets the layout for the target build. The image is saved locally at ignored `local/private/dashboard-design-reference.png`. Use a dark viewing area, a camera and workspace sidebar, and a shared timeline beneath the video. Blue marks saved footage, the playhead shows the selected time, and gaps show unavailable footage. Use the same viewer for live and recorded video.

| Section | Verified home-camera workflow | Target store workflow |
|---|---|---|
| Overview | Camera, live state, resolution, and latest saved time. | Store camera grid with a featured feed and per-camera health. |
| Live View | Focus the home camera, reconnect, or enter full screen. | Select a camera or switch between the grid and focused view. Add snapshots and camera settings when supported. |
| Playback | Choose date and time, zoom the timeline, step through footage, and skip to an available interval. | Filter the shared timeline by camera and align recordings across views. |
| Storage | Show actual local archive coverage, oldest available footage, and configured retention. | Show local disk and archive capacity, upload queue, and retention health. |
| Integrations | Show the local camera, restream, and recording connections. | Add private remote access and archive connection status during deployment. |
| Export | Select a saved start time in Playback and download a bounded MP4. | Export a timestamped clip from the selected store camera. |

## MVP scope

| ID | Priority | Requirement | Acceptance evidence |
|---|---|---|---|
| F1 | P0 | Connect a CG6 to the VPS across networks and reconnect after camera sleep, network loss, or process restart. | Deterministic local recovery tests pass first. A remote test then records ICE path, packet and frame gaps, retries, restart behavior, and decoded-frame quality. |
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
- **Continuity:** the owner waived a separate 24-hour powered lab endurance test on November 6. This leaves long-run camera and bridge reliability unproven. An unattended busy store shift with explicit outage reporting and a seven-day archive trial remain required to accept F2 and the full MVP; ordinary production observation can supply this evidence.
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
2. **Remote transport:** one short VPS-to-store path decoded cleanly, but two instrumented three-camera rounds failed continuity. The vendor relay or simultaneous sessions might be unreliable. A small store gateway remains a measured fallback.
3. **Media integrity:** short raw H.264 captures now decode after deeper RTP assembly. During failed rounds, RTP continued while complete bridge output paused. Local recovery tests must prove bounded progress after incomplete frames before another VPS acceptance round.
4. **Storage:** the CX23 has only 40 GB local disk. Google Drive has ample reported capacity but is not yet proven as an automated playback archive. A local queue or extra volume may be needed for upload outages.
5. **Compute:** two shared vCPUs and 4 GB RAM require a lean, remux-first design. AI is a separate capacity decision.

## Open product decisions

- Does the owner want audio at all? Default for MVP: video only.
- Is private device access through Tailscale sufficient for the first dashboard, or is a separate web login required? Default: Tailscale first.
- What camera names and positions should appear in the dashboard? Collect after live feeds work.
