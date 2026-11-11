# Bounded Windows store verification

## Live dashboard preview

Run `pwsh -NoProfile -File scripts/store-dashboard.ps1 -Live` from the project root for the default `2304×1296` (`2K`) live profile (or add `-LowBandwidth` for the opt-in `640×360` trial mode, kept separate from `2K` acceptance), and open `http://127.0.0.1:11984/#overview` once all three camera startups have been reported. Overview shows the three live tiles plus live upstream status (`Starting`, `Live`, `Recovering`, `Stalled`, `Offline`). Select **Focus** for one camera or **Playback** to open a saved interval. The launcher keeps one upstream bridge session and MediaMTX RTSP reader per camera running independently of browser open/close state until `Ctrl+C` (pass `-Minutes <N>` for a bounded timed run). All active cameras keep recording until there is one shared saved interval of at least 25 seconds across all three, or until the 240-second recording limit, 116 MiB stop threshold, or requested session cutoff. Recording then stops together while upstream live feeds and playback remain active. Recordings and `live-summary.json` are kept in the ignored `local/store-dashboard/live-*/` directory. The summary includes the longest shared saved interval as `common_overlap_s`. The preview does not depend on the recording acceptance gate.

The store profile uses the existing Go bridge and the verified go2rtc, MediaMTX, and video player assets from the home install. It does not run the home launcher or write to the home profile. The local dashboard listens at `127.0.0.1:11984`, RTSP at `19554`, playback at `19996`, and the recorder control API at `19997`. All four are loopback only. MediaMTX pulls the three inputs whether a browser is open or not, and `www/status.json` reports live pipeline telemetry.

## Run

1. From the project root, compare the current Windows Wi-Fi association and active IPv4 default route against the ignored store inventory. Treat historical LAN candidates as unrelated. Check drive free space and record the camera inventory's battery and charging readings as context. Use the existing power arrangement and follow the owner constraint in `AGENTS.md`.
2. Run the following commands in PowerShell 7. The store launcher reads the Windows-encrypted `local/private/vicohome-store.credential.xml` if present or prompts privately for the dedicated store bridge account. It rejects the saved home-lab account before login. Do not put a password in a shell argument or in chat. The preflight compares all three current device identities with the ignored mapping before opening any camera.

   ```powershell
   pwsh -NoProfile -File scripts/build-bridge.ps1
   pwsh -NoProfile -File scripts/store-dashboard.ps1 -Preflight
   pwsh -NoProfile -File scripts/store-dashboard.ps1 -KitchenAB
   pwsh -NoProfile -File scripts/store-dashboard.ps1 -TwoCamera
   pwsh -NoProfile -File scripts/store-dashboard.ps1 -Round 1
   ```

3. Keep the dashboard closed until saved files appear. Then open [the store dashboard](http://127.0.0.1:11984/). Look for advancing live frames on every tile; focus each feed. The run measures packet arrival at MediaMTX about four times per second and records an uninterrupted measurement window before a deliberate eight-second recorder pause. Live ingest remains active during the pause. Recording stops at 90 seconds or the 128 MiB disk budget, whichever comes first. The disk check starts shutdown early to leave space for in-flight MP4 parts.
4. After shutdown, inspect the private round summary. Only if the short gate fails, make one evidence-driven adjustment and run `pwsh -NoProfile -File scripts/store-dashboard.ps1 -Round 2`. This variant staggers the recorder inputs at 0, 9 and 18 seconds and pauses recording after 60 seconds. Stop after a full gate pass or two rounds. For playback after either recording round, `pwsh -NoProfile -File scripts/store-dashboard.ps1 -Review 1` (or `-Review 2`) serves saved files for up to five minutes with the recorder disabled and no camera sources. Open Playback, choose each camera and a saved date/time, play past a known segment boundary, and seek to the deliberate gap. Stop the review with Ctrl+C when done.

Evidence stays in ignored `local/store-dashboard/preflight-*/` and `local/store-dashboard/round-*/`. Each round directory is single-use to preserve its evidence. The launcher deletes its temporary authentication cache at exit. Confirm the runner prints `writes stable=True` and the store listeners close. Never paste logs, camera identifiers, or footage into an issue or chat.

## How to read the evidence

- Bridge first-frame time starts when that bridge attempt starts. Reader time starts when the recorder launches. Reader gap is the time between increases in MediaMTX received bytes, sampled at roughly 250 ms, including a stalled tail to the end of the uninterrupted window. This measures receiver progress; saved packet timestamps separately check individual video timing.
- The analyzer probes the actual MediaMTX segments and decodes every video frame using FFmpeg with `-xerror`. It intersects saved time intervals from all three cameras within the uninterrupted window and uses the longest shared interval. The eight-second intentional pause is excluded from that measurement. The dashboard intersects saved timestamp coverage with [MediaMTX's playback list](https://mediamtx.org/docs/features/playback), which can otherwise represent a segment with an internal hole as one interval.
- A missing frame, missing file, or missing span is an outage. No decoder result is available for an empty recording. Bridge progress logs distinguish RTP-input sequence gaps from H.264-assembly discards:
  - `seq_gaps`, `seq_missing`, `seq_late`: measured immediately at `track.ReadRTP()` (`seq_gaps` = forward sequence jumps; `seq_missing` = net sequence numbers that never arrived at `track.ReadRTP()`; `seq_late` = out-of-order packets arriving after a higher sequence number).
  - `asm_drops`, `asm_late`: measured during H.264 sample assembly (`asm_drops` = arrived RTP packets discarded during `SampleBuilder` / `h264LiveRecovery` assembly; `asm_late` = subset of `asm_drops` that arrived after assembly had already advanced past their sequence number or RTP timestamp).
  A bridge retry is an internal retry, separate from restreamer launches of a new process.
- If footage is missing, follow the pipeline in order: ticket/SDP, ICE/data channel, RTP arrival, complete H.264 output, go2rtc producer/consumer counters, MediaMTX input bytes, and saved timestamps. Do not apply a downstream frame-recovery fix when the bridge received no RTP.

## November 7 result

The build and all three sequential smoke captures passed on the Windows project PC. The two real camera rounds ended at the wall cutoff. Round 1 saved nothing. Round 2 saved nine segments (6,164,027 bytes) before and after the recorder pause. Saved videos for Dining and Cash fully decoded at 2304×1296, but their reader gaps exceeded two seconds, and Kitchen had no RTP after signaling. Three-camera common playable overlap was zero. Edge verified seeking on Dining and Cash and showed the gap. The live and segment-boundary browser checks did not pass. See [the result table and next decision](../STATUS.md#windows-store-result--november-7).

Private evidence: `preflight-20261107-122401/`, `round-1/`, `round-2/`, `kitchen-ab-20261107-152955/`, `kitchen-ab-20261107-154934/`, `two-camera-20261107-163351/`, and `two-camera-20261107-173947/` under `local/store-dashboard/`. The preflight's original summary labeled Kitchen by the preview's container metadata; its full decode contains both preview and main-stream frames with no errors. The smoke check now tests decoded frame dimensions for the main stream. The saved-media check decodes the original frames and assigns a separate clock only to the disposable validation output so its muxer cannot mislabel source timing defects as decoder errors.

### Kitchen-only A/B comparison (`-KitchenAB`)

To isolate whether Kitchen Back's Round 1/2 failure was caused by `go2rtc`'s continuous source or by multi-camera startup/session concurrency, run:

```powershell
pwsh -NoProfile -File scripts/build-bridge.ps1
pwsh -NoProfile -File scripts/store-dashboard.ps1 -KitchenAB
```

- **Arm A (`A_standalone`):** Runs `vico-cli.exe devices media-probe <serial> --timeout 20s --observe-for 12s --progress-every 100ms --output kitchen_a.h264`.
- **Arm B (`B_go2rtc_continuous`):** Runs the same bridge build through `go2rtc` (`exec:... --continuous --timeout 20s --observe-for 12s --progress-every 100ms --output - --health-log kitchen_b.bridge.log`) with one loopback RTSP reader (`ffmpeg` writing fragmented MP4 `kitchen_b.mp4`), keeping MediaMTX and the browser out of the comparison.
- Both arms share the same clock origin (before account auth, wake, and ticket issuance), a 20-second startup cutoff canceled on the first complete `2304×1296` frame, a 12-second post-first-frame observation window, and a 5-second cooldown between arms so no vendor sessions overlap.
- **Result (`kitchen-ab-20261107-154934`):** Both arms passed (`both_pass=true`) over `relay/srflx` on the first offer with **0 assembler drops** and **0 decode errors**. Arm A delivered first RTP at `6,340 ms`, first complete `2304×1296` frame at `7,410 ms`, and `189` decoded frames (`432 ms` max frame gap). Arm B delivered first RTP at `7,332 ms`, first complete `2304×1296` bridge frame at `8,314 ms`, first `go2rtc` producer bytes at `8,410 ms`, and first loopback reader frame at `9,284 ms`, with `185` decoded `2304×1296` frames across an `11.594 s` reader span (`444 ms` max bridge frame gap, `0.444 s` max PTS gap).
- **Evidence limits:** Single-camera continuous restreaming works cleanly for Kitchen Back on the store PC, proving `go2rtc` is not the blocker when Kitchen Back runs alone.

### Two-camera serialized-startup test (`-TwoCamera`)

To test whether starting Dining Area after Kitchen Back's first complete `2304×1296` frame maintains continuous MediaMTX recording across both feeds under the 90-second wall limit and 128 MiB disk budget, run:

```powershell
pwsh -NoProfile -File scripts/build-bridge.ps1
pwsh -NoProfile -File scripts/store-dashboard.ps1 -TwoCamera
```

- **Result (`two-camera-20261107-163351`):**
  - Kitchen Back started first, reached first RTP at `11.024 s` and first complete `2304×1296` frame at `11.746 s` over `srflx/srflx` with `0` assembler drops (`21 ms` max RTP gap, `55 ms` max frame gap).
  - Dining Area was started at `+12.843 s` recorder elapsed, reached first RTP at `6.208 s` and first complete `2304×1296` frame at `7.368 s` over `srflx/prflx` with `0` startup drops.
  - Both feeds recorded valid `2304×1296` fMP4 segments in MediaMTX that fully decoded with **`0` decode errors** (`34.969 s` / `404` frames across `7` segments for Kitchen Back; `48.685 s` / `733` frames across `8` segments for Dining Area; `10,576,504` bytes total; `writes_stable = true` at the `90 s` wall cutoff).
  - However, Dining's startup and concurrent `2304×1296` streaming interrupted Kitchen Back (`kitchen_interrupted_by_dining_startup = true`), and both feeds failed at **`bridge output` (`H.264 output stalled while RTP continued`)** after upstream `1.154–1.668 s` RTP arrival freezes and `2,521–2,714` assembler drops during `2304×1296` IDR bursts. Kitchen Back received RTP for `7.929 s` after its last complete frame before `output_stall` triggered `attempt=2` (`21.394 s` reader gap / `21.197 s` saved gap); Dining Area received RTP for `4.489 s` after its last complete frame during a `4.539 s` bridge gap (`4.669 s` reader gap / `4.540 s` saved gap). Longest shared playable coverage was **`9.646 s`** (below the `20.0 s` gate).
- **Result (`two-camera-20261107-173947`, 3-phase `Kitchen-only → Kitchen + Dining → Kitchen-only` with RTP-input vs. H.264-assembly counters):**
  - **Phase 1 (`Kitchen-only` baseline, `12.177 s`):** Kitchen Back (`srflx/srflx`, `first_rtp_s = 5.551 s`, `first_frame_s = 6.312 s`) delivered `2,849` RTP packets and `178` complete `2304×1296` frames with `0` `asm_drops`, `0` `asm_late`, `0` net `seq_missing` (`8` single-packet out-of-order arrivals all recovered by `seq_late`), `0.291 s` max RTP gap, `0.427 s` max bridge frame gap, `0.522 s` max reader gap, and `0.392 s` max saved gap (`12.177 s` saved).
  - **Phase 2 (`Kitchen + Dining`, `38.956 s`):** Dining Area started at `+19.156 s`, got `wake_code=0` and `peer=true`, and timed out waiting for an SDP answer (`answer=false`) on both attempts (`common_overlap_s = 0 s`). Meanwhile, `5.6 s` after Dining woke, Kitchen Back's `attempt=1` stream suffered an `814 ms` RTP arrival freeze, **`20` permanently unreceived RTP sequence numbers (`seq_missing = 20`)** at `track.ReadRTP()`, `9` late assembly drops (`asm_late = 9`), and `795` total `asm_drops` (`814` `drops`), stalling frame output for `8.120 s` and triggering an `attempt=2` reconnect (`16.844 s` bridge frame gap, `16.872 s` reader gap, `16.672 s` saved gap; `kitchen_recovered_without_reconnecting = false`).
  - **Phase 3 (`Kitchen-only` after stopping Dining at `+58.110 s`, `32.003 s`):** Continuing on `attempt=2`, Kitchen Back delivered `8,090` RTP packets and `478` complete `2304×1296` frames with `0` `asm_drops`, `0` `asm_late`, `0` net `seq_missing` (`17/17` single-packet reorderings recovered), `0.266 s` max RTP gap, `0.470 s` max bridge frame gap (`0.432 s` steady state), `0.528 s` max reader gap (`0.281 s` steady state), and `0.371 s` max saved gap (`0.328 s` steady state; `31.975 s` saved out of `32.003 s`). All `9` Kitchen segments (`8,241,083` bytes, `998` frames) decoded at `2304×1296` with `0` errors (`writes_stable = true`).

### Receive-path diagnostics

`-TwoCamera` sets `VICO_RX_TRACE=1`. Each bridge progress line adds loop timings (`rd_wait_ms` blocked in `ReadRTP`, `rd_lag_ms` socket-to-loop queueing, `push_ms`, `pop_ms`, `wr_ms`, `busy_ms`, `wr_slow`) and socket-level counters (`rx_*`, measured under the ICE agent, independent of the loop). Large `rd_lag_ms`/`wr_ms` means the bridge was blocked; large `rd_wait_ms` with matching `rx_max_vgap_ms` means input was absent or delayed. `rx_skipped` versus `rx_late` shows whether skipped packets eventually arrived. The runner also writes `udp-counters.json` (host `netstat -s -p udp`); rising `Receive Errors` would mean OS buffer drops. Per-camera `*.bridge.log.rx.csv` holds only sequence, size, marker and time. Result: see [STATUS](../STATUS.md#kitchen-receive-stall-instrumentation-and-test--two-camera-20261107-181934).

### Reassembly experiments

Offline: `go test -p 2 ./cmd/devices -run LiveReassemblyWindow -v` (from `services/vico-bridge`) replays a synthetic delayed stream through the real recovery logic for several window/stall settings and prints discards, output gaps, added delay and the limit that fired. Live: set `VICO_REASSEMBLY_WINDOW_MS` and `VICO_FRAME_STALL_MS` in the PowerShell session before `-TwoCamera` (for example `$env:VICO_REASSEMBLY_WINDOW_MS="3100"; $env:VICO_FRAME_STALL_MS="4000"`), then clear them. The bridge logs `event=reassembly` with the values in use, and progress lines add `age_ms` (first packet of an emitted frame to output). Unset keeps the 500 ms / 1.5 s defaults. Results: [STATUS](../STATUS.md#reassembly-window-validation--offline-model-and-two-camera-20261107-182950).

### Powered-pair test (`-PoweredPair`)

```powershell
pwsh -NoProfile -File scripts/build-bridge.ps1
pwsh -NoProfile -File scripts/store-dashboard.ps1 -PoweredPair
```

Dining Area and Cash Register only. The runner refuses to start any camera session unless the account reports charging flag 1 for both (this flag does not prove the power supply is stable). Sequence: Dining records alone for 12 s after its first frame; Cash wakes and negotiates WebRTC with `--hold-start-live-file` (the bridge opens the data channel and waits); after 8 s connected the runner creates `cash_register.release`, and Cash sends `startLive`. Same 90 s and 128 MiB limits and bridge defaults. The summary lists each transition and Dining's metrics for each phase. The camera sends a low-resolution preview on connect, so RTP can flow during the hold; the hold only withholds the main-stream request. Result: [STATUS](../STATUS.md#powered-diningcash-test--powered-pair-20261109-114143). The owner directed the PC to stay on the existing 5 GHz Wi-Fi. The subsequent [Dining-only baseline](../STATUS.md#dining-only-wi-fi-baseline--dining-solo-20261109-115517) reproduced the store's single-camera workflow and showed that a continuity failure can occur without a second stream.

### Matched receiver comparison (`-ReceiverComparison`)

```powershell
pwsh -NoProfile -File scripts/build-bridge.ps1
pwsh -NoProfile -File scripts/store-dashboard.ps1 -ReceiverComparison
```

Runs a matched comparison between the Go bridge and an independent VicoHome WebRTC receiver (`vicostream`) on Dining Area with Cash Register streaming concurrently under identical network conditions, bridge settings, and 90-second limits. Verifies whether multi-second frame gaps originate in receiver buffering or the 2.4 GHz RF link. Result: [STATUS](../STATUS.md#current-project-state).

### Three-camera acceptance test (`-ThreeCamera`)

```powershell
pwsh -NoProfile -File scripts/build-bridge.ps1
pwsh -NoProfile -File scripts/store-dashboard.ps1 -ThreeCamera -ReassemblyWindowMs 1200 -FrameStallMs 1800
```

Runs the bounded three-camera local acceptance test across Dining Area, Cash Register, and Kitchen Back with staggered startup, battery level logging, 90-second wall limit, and 128 MiB disk budget. Verifies advancing live views, automatic continuous recording, shared $\ge 20$-second overlap across all three cameras, $\le 2.0$s max reader/saved gaps through cutoff, zero decode errors, and playback by camera and time in the dashboard. Result: [STATUS](../STATUS.md#current-project-state).
