# Contributing to UZY CCTV

This is a private collaboration repository for an owner-only CCTV/NVR system. The current engineering finish line is reliable three-camera live view, recording, and playback on the Windows store PC. Do not start analytics or VPS promotion until the acceptance gates in `docs/PRD.md` pass.

## Before changing code

Read these files in order:

1. `AGENTS.md`
2. `README.md`
3. `docs/STATUS.md`
4. `docs/PRD.md`
5. `docs/ARCHITECTURE.md`
6. `docs/ONBOARDING.md`

Treat `docs/STATUS.md` as the evidence record and `docs/PRD.md` as the acceptance contract. Keep results from the local Windows product gate separate from later VPS deployment evidence.

## Working agreement

- Create a short-lived branch from the current default branch. Use names such as `fix/pktmon-capture`, `test/udp-correlation`, or `docs/store-runbook`.
- Keep commits focused and use conventional prefixes such as `fix:`, `test:`, `docs:`, and `chore:`.
- Open a pull request for review. State the exact test command, result, evidence limits, and whether live cameras or the VPS were touched.
- Prefer deterministic offline reproduction before using a live-camera test. A live pass must be bounded by the relevant runbook.
- Make the smallest change that tests the current hypothesis. Do not combine transport, recording, dashboard, archive, and analytics changes in one experiment.
- Update `docs/STATUS.md` after a live test or deployment. Add or update an ADR when an architectural decision changes.

## Required checks

From `services/vico-bridge`:

```powershell
go test -p 2 -count=1 ./...
go build -o bin/vico-cli.exe .
```

From the repository root, run the focused Python harnesses when their code paths change:

```powershell
python scripts/test_counters_compare.py -v
python scripts/test_pktmon_compare.py -v
python scripts/test-restream-concurrency.py --help
```

The generated executable belongs under ignored `services/vico-bridge/bin/`. Live verification commands and limits are in `docs/ops/STORE_WINDOWS.md`; do not improvise an unbounded camera run.

## Secrets and evidence

Never commit or paste credentials, API tokens, Wi-Fi details, camera serials, pairing codes, private endpoint data, footage, packet captures, auth caches, or raw account logs. Store local runtime material only in ignored `local/` or another explicitly ignored path.

Before every commit, inspect:

```powershell
git status --short
git diff --check
git diff --cached
```

Use sanitized aggregate metrics in documentation. Missing footage is a recording outage, never evidence of worker inactivity. Preserve `services/vico-bridge/LICENSE.md` and the upstream attribution in the service README.

## Pull-request handoff

Every pull request should answer:

1. What measured failure or acceptance gate does this address?
2. What is the smallest behavioral change?
3. Which offline tests passed?
4. Was a live camera, private account, or VPS used?
5. What remains unproven?
