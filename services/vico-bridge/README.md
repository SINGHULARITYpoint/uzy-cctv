# VicoHome CG6 bridge prototype

This Go module began from [`dydx/vico-cli`](https://github.com/dydx/vico-cli) at commit `3506b73` (MIT license). Local additions request WebRTC tickets, probe signaling, and receive owned CG6 camera media. The source still has the upstream module path; rename it only when there is a deliberate module migration.

The bridge is **experimental**. Increasing RTP reassembly depth enabled clean short captures from all three store cameras and a clean one-camera VPS capture. `devices media-probe SERIAL --continuous --timeout 90s --output -` emits H.264 to stdout for a private go2rtc exec source and retries after a stalled or closed session. It waits for the main-stream SPS/PPS and IDR before output. The current three-camera overlap did not provide full coverage; there is no segment recorder, outage index, or dashboard.

## Build and focused test

From this directory, with a compatible Go toolchain:

```powershell
go test ./cmd/devices
go build -o bin/vico-cli.exe .
```

This PC is the primary development environment. Reproduce transport failures with deterministic RTP/H.264 tests, then use the owned home-lab camera for device behavior such as keyframe requests. A passing local revision can be cross-built for the bounded VPS smoke and three-camera acceptance tests. See the repository [roadmap](../../docs/ROADMAP.md).

Use `scripts/test-cg6-endurance.ps1` from the repository root for the owned lab camera. It prompts for credentials and writes into ignored `local/`. Never commit auth caches or raw video.

## Current CLI probes

`devices ticket SERIAL_NUMBER` returns redacted ticket metadata. `devices signal-probe SERIAL_NUMBER` checks the signaling handshake. `devices media-probe SERIAL_NUMBER` receives WebRTC media and can write raw H.264 with `--output`; `--observe-for`, `--timeout`, and `--progress-every` control endurance runs. Use `--help` for current flags. The CLI is a diagnostic tool, not yet the final service.

The bridge remains dependent on VicoHome account and ticket/signaling services; video takes the selected WebRTC/ICE path, which may be direct or relayed.
