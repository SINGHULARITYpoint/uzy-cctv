# UZY CCTV

Private CCTV/NVR project for Uzy's NY Pizza. The first deliverable is a reliable way for the owner to view three store cameras live from anywhere and play back the previous seven days. Person detection and station activity come later.

## Start here

1. Read [current status](docs/STATUS.md) for what has actually been tested and what is still unproven.
2. Read the [MVP PRD](docs/PRD.md) for scope and acceptance criteria.
3. Read the [contributor onboarding map](docs/ONBOARDING.md) for the code paths, test commands, and immediate finish line.
4. Read [architecture](docs/ARCHITECTURE.md) and the [decision records](docs/decisions/).
5. Follow the [roadmap](docs/ROADMAP.md). The current milestone is the [three-camera store build on this PC](docs/handoffs/STORE_CAMERA_ONBOARDING.md).
6. Use the [device inventory](docs/DEVICES.md) for fleet context. More detailed LAN identifiers live in ignored `local/private/store-network.md` on this laptop.

The folder is a local Git repository. Open this root folder in VS Code, Antigravity, Claude Code, or another editor. `AGENTS.md` is the shared working context; `CLAUDE.md` and `GEMINI.md` point to it.

For human collaborators, [CONTRIBUTING.md](CONTRIBUTING.md) defines the branch, evidence, testing, and secret-handling workflow. A clone intentionally does not contain credentials, camera identities, private logs, packet captures, or footage; collaborators can run all offline tests without those assets.

## Home-camera dashboard

From the project root, start the local dashboard with the saved Windows lab credential.

```powershell
pwsh -NoProfile -File scripts/home-dashboard.ps1
```

Open [UZY CCTV](http://127.0.0.1:1984/) in the in-app browser, Edge, or Chrome on this PC. **Overview** shows the camera and recorder status. **Live View** focuses the feed. In **Playback**, choose a date and time or select a blue interval beneath the viewer. **Play latest** opens the newest saved video for the selected date. Gaps have no available footage. After selecting a time in Playback, use **Export** to download a clip.

**Storage** shows saved coverage and retention. **Integrations** shows the local services that supply live video and playback. The [dashboard feature map](docs/PRD.md#dashboard-layout) describes how the same sections will support the store cameras.

The launcher downloads verified go2rtc and MediaMTX builds on first run. MediaMTX records video while the local service and PC are running, including while the dashboard is closed. Recordings go under ignored `local/home-dashboard/recordings/` and expire after seven days. Stop both processes with `pwsh -NoProfile -File scripts/home-dashboard.ps1 -Stop`. Run the start command again after a Windows restart.

For the three store cameras, run `pwsh -NoProfile -File scripts/store-dashboard.ps1 -Live` on the Windows store PC and open the printed dashboard link. This local preview starts a short automatic recording and keeps live view and playback available for up to 15 minutes. See the [store runbook](docs/ops/STORE_WINDOWS.md#live-dashboard-preview).

## Layout

```text
docs/                 Product, architecture, status, roadmap, decisions, operations
services/vico-bridge/ Go prototype adapted from dydx/vico-cli (MIT)
scripts/              Repeatable local build and lab commands
infra/                VPS bootstrap template; generated config is ignored
local/                Ignored recordings, logs, caches, and temporary results
```

## Current state

- One powered GMK CG6 maintained an independently initiated live WebRTC session for 35 minutes in the home lab.
- Short captures can decode cleanly, but two instrumented three-camera VPS rounds showed multi-second bridge-output gaps while RTP continued. Continuous coverage and reconnect recovery remain unproven.
- Hetzner CX23 `uzy-bridge` was cleanly rebuilt with Ubuntu 24.04 on November 6, 2026. Its private go2rtc service remains an acceptance target. Deterministic bridge recovery and the single-camera Windows dashboard have been verified. On-site three-camera verification on this PC comes next. See [current status](docs/STATUS.md).

## Development

The Go module is under `services/vico-bridge`. The normal engineering loop runs here first. Add or reproduce a failure in a focused Go test, implement the bridge change, then run:

```powershell
cd services/vico-bridge
go test ./cmd/devices
go build -o bin/vico-cli.exe .
```

Use the dedicated home profile for single-camera regression checks and the store handoff for the current on-site workflow. The VPS migration follows owner review of the integrated local system. See the [roadmap](docs/ROADMAP.md).

The existing bridge is a **lab prototype**, not a deployable NVR. Do not infer continuous recording from the 35-minute result. Do not store credentials, token caches, camera footage, or generated binaries in Git. See `.gitignore` and [security notes](docs/ops/SECURITY.md).

## External source

The bridge began from [`dydx/vico-cli`](https://github.com/dydx/vico-cli), upstream commit `3506b73`, under the [MIT license](services/vico-bridge/LICENSE.md). Local CG6 ticket, signaling, and media-probe changes are included. This is not affiliated with VicoHome or GMK.
