# Codebase orientation and collaborator handoff

## One-line summary

UZY CCTV is a Windows-first Go and browser prototype that obtains owned GMK CG6 camera video through VicoHome ticket/signaling services, restreams H.264 locally, records it, and exposes live and time-based playback in an owner dashboard.

## Five-minute explanation

- **Current finish line:** make Dining Area, Cash Register, and Kitchen Back advance together through the UZY dashboard, save at least 20 seconds of shared playable coverage with gaps no larger than two seconds, and play each camera by time. The gate currently fails.
- **Immediate diagnostic:** validate Windows Packet Monitor capture/export and Wi-Fi frame decoding with known synthetic UDP traffic. The November 11 adapter capture counted thousands of packets but exported only 11 packet blocks, so it cannot yet locate the receive loss.
- **Inputs:** VicoHome account credentials entered at private runtime prompts, owned camera device records, vendor ticket/WebSocket signaling, WebRTC/ICE media, and local recorder/playback requests.
- **Outputs:** H.264 from the Go bridge, loopback restreams from go2rtc, fragmented MP4 recordings from MediaMTX, sanitized health/coverage summaries, and the static browser dashboard.
- **Privacy boundary:** a clone contains source, tests, runbooks, and sanitized results. It intentionally omits `local/`, credentials, exact camera/network identities, logs, captures, footage, and generated binaries.

If you read only five files after this one, read `AGENTS.md`, `docs/STATUS.md`, `docs/PRD.md`, `docs/ARCHITECTURE.md`, and `docs/ops/STORE_WINDOWS.md`.

## Repository map

| Path | Responsibility |
|---|---|
| `services/vico-bridge/` | Go CLI for device lookup, ticketing, signaling, WebRTC media receipt, H.264 recovery, health instrumentation, startup locking, cleanup, and packet correlation. |
| `services/home-dashboard/` | Static owner dashboard shared by the home and store profiles; selects cameras, reports live state, lists saved intervals, seeks by time, and exports bounded MP4 clips. |
| `scripts/` | Windows orchestration, private credential prompting, local service startup, bounded camera tests, saved-media analysis, and diagnostic harnesses. |
| `infra/` | Sanitized templates for go2rtc, MediaMTX, systemd, and VPS bootstrap. Generated/private variants are ignored. |
| `docs/` | Product contract, architecture, evidence status, ADRs, operations, research, and task handoffs. |
| `local/` | Ignored private runtime state: camera mappings, credentials, recordings, logs, captures, and test evidence. It is not present in a clone. |

## Concrete execution paths

### Bridge command path

1. `services/vico-bridge/main.go` calls `cmd.Execute()`.
2. `services/vico-bridge/cmd/root.go` registers the `devices` and `events` command groups.
3. `services/vico-bridge/cmd/devices/root.go` registers device listing, ticket, signaling, media, and capture-correlation commands.
4. `services/vico-bridge/cmd/devices/media_probe.go` orchestrates authentication-derived device work, camera wake/signaling, WebRTC setup, stream selection, retries, health output, and shutdown.
5. `live_assembler.go`, `h264_live_gate.go`, `h264_nal.go`, and the matching tests own H.264 packet/frame recovery and the valid main-stream output gate.
6. `rx_diag.go` records socket/RTP timing and loss counters; `pktmon_correlator.go` compares an adapter capture with the private socket trace.
7. `session_cleanup.go` and `startup_lock_windows.go` keep camera-session teardown and cross-process startup serialization separate from media assembly.

### Local CCTV path

1. `scripts/home-dashboard.ps1` starts the single-camera regression profile.
2. `scripts/store-dashboard.ps1` validates the selected bounded store mode, obtains credentials privately, and calls `scripts/store-dashboard.py`.
3. `scripts/store-dashboard.py` generates private per-run configuration, starts the Go bridge through go2rtc, starts MediaMTX recording/playback, serves the dashboard, and writes sanitized summaries under ignored `local/store-dashboard/`.
4. `services/home-dashboard/profile.js` defines the home/store camera selection; `viewer.js` handles live views; `archive.js` lists coverage, seeks, plays saved video, and exports clips.
5. Media remains on loopback during the local gate. Browser close/open does not define whether recording is active; MediaMTX readers do.

### Adapter-loss diagnostic path

1. `scripts/store-dashboard.ps1 -PktmonCompare` selects the bounded comparison mode.
2. `scripts/pktmon_compare.py` configures Packet Monitor, launches the Dining/Cash comparison, converts the capture, and invokes the correlator.
3. The Go bridge writes private bidirectional flow and RTP sequence traces through `rx_diag.go`.
4. `vico-cli devices correlate-capture`, implemented by `pktmon_correlator.go`, classifies whether missing RTP was present at the adapter boundary.
5. The current capture-format coverage is insufficient, so the next change must first prove capture and decoding with known synthetic UDP request/response traffic.

## Current evidence, without overclaiming

- One powered home camera held a 35-minute independently initiated WebRTC session; this is not 24/7 proof.
- Each of the three store cameras and one store-to-VPS path produced a short clean decode after deeper RTP reassembly.
- The home Windows dashboard has demonstrated live view, automatic recording, and playback by time.
- Multiple three-camera runs produced recording gaps and failed the shared-coverage/continuity gate.
- Counters-only tests moved the first observed loss to the socket boundary or earlier, but the November 11 Packet Monitor export did not provide enough adapter packets to distinguish host receive loss from camera/Wi-Fi/relay delivery loss.
- VPS, Google Drive archive, remote owner access, seven-day retrieval, and unattended busy-shift reliability are not accepted yet.

The detailed measurements and the latest test hashes remain in `docs/STATUS.md`.

## Safe first contribution

Work on the synthetic UDP preflight for `scripts/pktmon_compare.py` and its offline tests. The proof should establish both packet directions at a named capture component, validate the exported packet layout, and make the parser reject insufficient coverage before any camera starts. Do not run another live camera comparison until that preflight passes.

## Offline development loop

From `services/vico-bridge`:

```powershell
go test -p 2 -count=1 ./...
go build -o bin/vico-cli.exe .
```

From the repository root:

```powershell
python scripts/test_counters_compare.py -v
python scripts/test_pktmon_compare.py -v
```

The Python tests and Go media/correlator tests use synthetic or saved sanitized structures and do not require camera credentials. Live commands, private prerequisites, time limits, and stop conditions are documented in `docs/ops/STORE_WINDOWS.md`.

## Files inspected for this map

This orientation was grounded in the current `main.go`, command routers, bridge README, store/home launchers, dashboard profile/playback code, script README, Windows store runbook, Go module, PRD, architecture, roadmap, status, device inventory, and security notes. It does not claim that ignored private evidence is available to a collaborator.
