# Local scripts

- `generate-cloud-init.ps1`: inserts the local SSH **public** key into ignored `infra/generated/cloud-init.yaml`. It does not contact Hetzner.
- `build-bridge.ps1`: runs the focused Go device tests and builds an ignored Windows binary.
- `home-dashboard.ps1`: starts the Windows home-camera dashboard at `http://127.0.0.1:1984/` using the saved lab credential and starts continuous MediaMTX recording. Use `-Stop` to stop both processes and the bridge. Runtime files, logs, and the rolling seven-day archive go under ignored `local/home-dashboard/`.
- `store-dashboard.ps1`: runs the separate Windows store profile. Use `-Preflight` for a private account and mapping check plus sequential smoke probes; `-KitchenAB` for the bounded Kitchen-only standalone-vs-`go2rtc` A/B comparison; `-Round 1` and, if needed, `-Round 2` for bounded MediaMTX recording and analysis; and `-Review 1` or `-Review 2` for time-based playback without live ingest or recording. Requires the existing verified home-install assets, Python, FFmpeg, and FFprobe. See the [Windows store runbook](../docs/ops/STORE_WINDOWS.md).
- `list-store-cameras.ps1`: privately prompts for the dedicated bridge account, writes the visible camera inventory under ignored `local/private/`, and deletes the temporary auth token afterward.
- `prepare-restream.ps1`: generates ignored private go2rtc source and environment files from the verified store inventory. Transfer them securely to the VPS; the temporary local environment file should be deleted after installation. The server copy must remain mode 600 and be updated when the bridge-account password rotates.
- `test-restream-concurrency.py`: runs the bounded VPS restream acceptance capture and verifies duration, resolution, decoder output, and gaps. Use it only after the local bridge promotion gate passes.
- `test-cg6-endurance.ps1`: prompts for lab credentials, tests an owned camera, writes ignored results under `local/lab-results/`, and deletes the cached auth token afterward. It requires the built bridge binary.

Run `pwsh -NoProfile -File scripts/test-cg6-endurance.ps1 -RecoveryProbe` for a 25-second home-camera check after the local build. It discards one non-IDR packet inside the receiver shortly after a keyframe, records PLI and IDR timings, and verifies the resulting video with FFmpeg. Add `-UseSavedCredential` on the Windows account that saved the ignored lab credential.

Run scripts from PowerShell 7 with `pwsh -NoProfile -File scripts/<name>.ps1`. Do not pass account passwords as command-line arguments.

For the current on-site milestone, follow the [store handoff](../docs/handoffs/STORE_CAMERA_ONBOARDING.md). The home launcher is a separate single-camera profile. The VPS scripts apply after the local store gate and owner review.
