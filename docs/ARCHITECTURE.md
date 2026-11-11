# Architecture: candidate CCTV pipeline

**Status:** live view, recording, and playback were verified for the single home camera. The next target is three store cameras through the same Windows stack. The private VPS restream is reserved for later migration.
**Updated:** November 7, 2026.

## Development and promotion path

```text
Home test camera -> Windows Go bridge -> loopback go2rtc -> Live view
                                           |
                                           v
                                     MediaMTX recorder -> local fMP4 archive -> History view
```

The Windows launcher uses the dedicated lab account and maps its single verified camera to `home_camera`. The [MediaMTX recorder](https://mediamtx.org/docs/features/record) pulls go2rtc over loopback RTSP continuously and writes H.264 as fragmented MP4 without transcoding. It uses one-second parts, approximately 30-second segments, and 168-hour retention. The [playback API](https://mediamtx.org/docs/features/playback) lists saved intervals from disk and remuxes the selected time into browser video. The dashboard uses advancing frames for live state and archive coverage for History. All three listeners bind to loopback, and the playback API permits the dashboard origin. For the on-site build, use a separate private store profile with one source alias and one recorder path per camera. Live view, saved coverage, playback and export use the same camera selection. Preserve the home profile and archive. Bound store recording sessions and stop the recorder after verification. The following diagram is the later deployment architecture.

```text
Store: 3 powered GMK CG6 cameras
          |
          | VicoHome ticket + WebSocket signaling; WebRTC/ICE media
          v
Hetzner CX23 (Nuremberg)
  Go camera bridge: auth, wake, signaling, media, reconnect, health
          |                         |
          | H.264 RTP               | health/events
          v                         v
  go2rtc live restream        status endpoint
  (loopback only)                    |
          |                           |
          +----------> private owner dashboard <---------+
          |
          v
  segment recorder (remux, no default transcode)
          |
          v
  bounded local queue + metadata index
          |
          v
  verified upload to private Google Drive archive
          |
          v
  indexed on-demand retrieval -> history player

Later, separately: restream -> person/zone detection -> activity view
```

## Component contracts

| Component | Responsibility | First proof required |
|---|---|---|
| Camera bridge | Obtain short-lived vendor ticket, wake camera, negotiate WebRTC, receive video, reconnect, expose stream and health. | Deterministic local incomplete-frame recovery, then a clean remote sample with automatic recovery. |
| Live restream | Serve browser-compatible live video to owner devices without one vendor session per viewer. | One camera plays remotely with measured latency. |
| Recorder | Convert received H.264 into finalized timestamped segments without damaging frames. | Full decode and clock checks across start, stop, and reconnect. |
| Archive queue | Upload completed segments, retry safely, verify bytes, and track local pressure. | Simulated upload outage and recovery. |
| History service | Index by camera/time and retrieve the right segments from Drive. | Seek to oldest and newest retained day, play across a boundary. |
| Dashboard | Authenticated live overview, full-screen view, history picker, and outage indicators. | Owner completes the PRD's end-to-end test. |

Use short finalized segments, provisionally around five minutes, so upload/retry and date/time indexing stay manageable. The final duration must be chosen after testing crash recovery, seeking, and Google Drive retrieval. The index should hold camera ID, segment start/end UTC, codec, bytes, checksum, archive file ID, upload state, and recording-gap events. Store display timezone separately. A lost index must be reconstructable from archived segment metadata and names.

## Trust and network boundaries

- The bridge uses a dedicated VicoHome account. Vendor credentials and short-lived ICE/TURN credentials remain outside Git and are never logged.
- Media is encrypted during WebRTC transport. The exact selected ICE route must be measured; a relay-selected home test is not proof of a LAN path.
- Private owner access should start with Tailscale or equivalent authenticated encrypted access. Do not expose raw camera, go2rtc, or archive endpoints on the public Internet.
- The CX23 should use key-only SSH and a host firewall. Google Drive access should be scoped to a private archive folder and use a dedicated OAuth client where feasible.
- Only the owner can view or export footage in the MVP. Record the store's retention and employee notice policy before production rollout.

## Storage and capacity

The owner reports a 5 TB Google Drive plan with about 60 GB used. This is Google Drive account storage, not a Google Cloud Storage bucket. Archive writes and on-demand playback through the Drive API still need a real test. The CX23 has 40 GB local disk and cannot retain seven days of three full feeds locally.

| Per-camera average video bitrate | Three cameras for seven days |
|---:|---:|
| Home quiet-scene sample, about 0.45 Mb/s | about 102 GB |
| 2 Mb/s | about 454 GB |
| 4 Mb/s | about 907 GB |

At 2 Mb/s per camera, three feeds ingest about 0.75 MB/s. A 20 GB local queue would fill in roughly 7.4 hours if Drive uploads stopped. The real queue budget will be lower or higher depending on OS usage and measured bitrate. Add a Hetzner Volume if the required outage buffer exceeds safe free space. Upload complete segments, verify, then delete local copies; never treat a mounted Drive as the recorder's live filesystem without a benchmark.

## Evidence and unresolved design gates

- Home lab: one powered CG6 maintained a 35-minute WebRTC session and delivered packets in 99.8% of observed seconds. A 30-minute ticket expiry did not end that active session. This says nothing about 24/7 reliability.
- Deeper RTP assembly enabled clean short samples, but two three-camera rounds still had multi-second complete-frame gaps while RTP arrived. Deterministic recovery and a bounded home-camera loss test now demonstrate recovery at an IDR. The home check did not establish that PLI accelerated recovery.
- One short store-to-VPS path worked. Three-camera concurrency has been tested and failed its coverage gate. Local browser playback has been verified for the home camera. Three-camera browser playback, remote browser playback, automated Drive archive, and oldest-day retrieval remain unverified.

See [research notes](research/CG6_TRANSPORT.md), [current status](STATUS.md), and [ADR 0003](decisions/0003-vendor-bridge.md).
