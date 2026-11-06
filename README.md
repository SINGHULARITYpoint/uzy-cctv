# UZY CCTV

Private CCTV/NVR project for Uzy's NY Pizza. The first deliverable is a reliable way for the owner to view three store cameras live from anywhere and play back the previous seven days. Person detection and station activity come later.

## Start here

1. Read [current status](docs/STATUS.md) for what has actually been tested and what is still unproven.
2. Read the [MVP PRD](docs/PRD.md) for scope and acceptance criteria.
3. Read [architecture](docs/ARCHITECTURE.md) and the [decision records](docs/decisions/).
4. Follow the [roadmap](docs/ROADMAP.md). The next gate is a clean VPS bootstrap followed by a remote one-camera stream test.
5. Use the [device inventory](docs/DEVICES.md) for fleet context. More detailed LAN identifiers live in ignored `local/private/store-network.md` on this laptop.

The folder is a local Git repository. Open this root folder in VS Code, Antigravity, Claude Code, or another editor. `AGENTS.md` is the shared working context; `CLAUDE.md` and `GEMINI.md` point to it.

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
- The current recorder still produced H.264 decode errors. Restart recovery and remote VPS transport are not yet proven.
- Hetzner CX23 `uzy-bridge` was identified, but **was not rebuilt** as of the last verified check on November 6, 2026. The owner authorized wiping its current contents without a backup.
- The Hetzner CLI was installed locally; its project context had not yet been created at the last check. See [VPS bootstrap](docs/ops/VPS_BOOTSTRAP.md).

## Development

The Go module is under `services/vico-bridge`. Install a Go toolchain compatible with `go.mod`, then:

```powershell
cd services/vico-bridge
go test ./cmd/devices
go build -o bin/vico-cli.exe .
```

The existing bridge is a **lab prototype**, not a deployable NVR. Do not infer continuous recording from the 35-minute result. Do not store credentials, token caches, camera footage, or generated binaries in Git. See `.gitignore` and [security notes](docs/ops/SECURITY.md).

## External source

The bridge began from [`dydx/vico-cli`](https://github.com/dydx/vico-cli), upstream commit `3506b73`, under the [MIT license](services/vico-bridge/LICENSE.md). Local CG6 ticket, signaling, and media-probe changes are included. This is not affiliated with VicoHome or GMK.
