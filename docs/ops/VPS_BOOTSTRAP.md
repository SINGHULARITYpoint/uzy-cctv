# VPS bootstrap runbook

**Target:** Hetzner project Uzy's NY Pizza, server `uzy-bridge`, ID `138377676`, CX23 in Nuremberg. The authorized clean rebuild completed on November 6, 2026, without an old-content backup.

The `uzy-cctv` CLI context is active. The owner-supplied one-time Desktop token file was deleted after verifying project access. Cloud-init completed with no errors; SSH key access, sudo, UFW SSH-only, disabled password/root login, and ffmpeg were verified. A Linux bridge binary was transferred under `/home/cameraops/bin/`. One short Dining Area stream from the store to this VPS passed full H.264 decode; no recording service or dashboard has been deployed.

## 1. Local CLI access (completed)

`hcloud` 1.70.2 is installed at `C:\Users\arshd\AppData\Local\Microsoft\WinGet\Packages\HetznerCloud.CLI_Microsoft.Winget.Source_8wekyb3d8bbwe\hcloud.exe`. The `uzy-cctv` context was created and verified on November 6. Its one-time Desktop token file was deleted. Use the full CLI path if `hcloud` is not on PATH. The original setup commands were:

```powershell
hcloud context create uzy-cctv
hcloud context list
```

Do not paste a token into a chat, command argument, committed file, or terminal transcript. Do not create another token for routine use of the existing context.

## 2. Verify the exact destructive target (completed before rebuild)

Run read-only checks first:

```powershell
hcloud context list
hcloud server describe 138377676
hcloud server list
hcloud volume list
hcloud firewall list
```

Confirm the context is the Uzy's NY Pizza project and the target is still `uzy-bridge`, CX23, Nuremberg. Check whether a volume is attached and whether any other resources share this host. A rebuild wipes the server's local disk; the owner has authorized that loss. Do not take a snapshot or enable backups for the old content. Do not delete any separate volume merely because it exists; inspect its attachment and purpose first.

## 3. Generate cloud-init locally

The default local key is `~/.ssh/id_ed25519.pub`, whose private half was present during workspace setup. Generate the actual cloud-init file; it goes under ignored `infra/generated/`:

```powershell
pwsh -NoProfile -File scripts/generate-cloud-init.ps1
```

The file was generated on November 6 at ignored `infra/generated/cloud-init.yaml` (SHA-256 `3875EE246B7B81B7FE3B3BFA68E838E939A7E5BD531CD96F619718CE5C2132CC`). Review its user and SSH-key fields without sharing the private key. It creates `cameraops`, disables password and root SSH login, installs base diagnostic packages, and enables UFW with SSH allowed. It does not install camera services or put credentials on the server.

## 4. Rebuild through the CLI (completed)

Verify that the desired Ubuntu LTS image is available using `hcloud image list`. The planned image is `ubuntu-24.04`; adjust only if availability or compatibility requires it. From the project root:

```powershell
hcloud server rebuild --image ubuntu-24.04 --user-data-from-file infra/generated/cloud-init.yaml 138377676
```

This is the destructive step already authorized for this exact server. Record the actual image, time, and result in `docs/STATUS.md`. The server's SSH host key will change. Use the fresh IP from `hcloud server describe 138377676`, not a stale value from notes.

## 5. Verify first boot (completed)

Connect with the matching private key:

```powershell
ssh -i $HOME/.ssh/id_ed25519 cameraops@<verified-server-ip>
```

If SSH reports a changed host key, verify the IP and rebuild status first, then remove only that host's stale known-hosts entry. On the server:

```bash
cloud-init status --wait
cat /etc/os-release
sudo ufw status verbose
df -h /
ffmpeg -version
```

Verify `cameraops` can use sudo, password SSH is disabled, and no public camera or dashboard endpoint is open. Then update `docs/STATUS.md`. If cloud-init fails, use Hetzner's console/rescue access and repair or rebuild; the old contents are intentionally unrecoverable.

## 6. Run the short three-camera gate

Enter this section only after the same bridge revision passes the local promotion gate in the [roadmap](../ROADMAP.md).

Use the verified rebuild-specific SSH pin from ignored `local/private/uzy-bridge-known_hosts`. On Windows, use that relative path from the project root in the `UserKnownHostsFile` option so OpenSSH handles a workspace path with spaces. Verify `systemctl is-active uzy-go2rtc`, `sudo ufw status`, and `ss -lntu` before installing a bridge update. The three go2rtc listeners must be on loopback and UFW must permit only SSH. Check for active viewers through the loopback `/api/streams` endpoint before restarting the service. Its response includes private source commands.

Run `go test ./...` from `services/vico-bridge` with loopback available. Cross-build with `GOOS=linux GOARCH=amd64 go build -o bin/vico-cli-linux-amd64 .` in a shell that supports inline environment variables. Generate the private YAML from `infra/go2rtc.template.yaml` and the ignored camera inventory. Keep the existing account environment file on the server. Stage the binary, YAML, and `scripts/test-restream-concurrency.py` in a private directory, install the binary at `/home/cameraops/bin/vico-cli` and the config at `/home/cameraops/.config/uzy-cctv/go2rtc.yaml`, then run `sudo systemctl restart uzy-go2rtc`. Keep the previous binary and config available during installation for rollback if the service fails to start.

Run the gate on the VPS from the directory containing the staged test script.

```bash
umask 077
stage=$(mktemp -d /home/cameraops/probes/concurrency.XXXXXXXX)
mkdir -m 700 "$stage/round-1"
timeout -k 5s 8m python3 test-restream-concurrency.py "$stage/round-1" --solo-first
```

Round one runs short solo captures before opening all three RTSP readers together. Use a second fresh round directory and omit `--solo-first` if a second overlap is needed. The reader uses TCP, a 48-second socket timeout, and a 72-second capture ceiling per group. It allows an additional three seconds of capture after the required common window. The gate checks saved video duration, 2304×1296 dimensions, full video decode, and concurrent reader progress from advancing `out_time_us`. It rejects gaps over two seconds in reader progress or saved video timestamps. Decoder output gets a separate frame clock so remux timestamp issues are measured from the sample and do not become false decoder errors. If a stalled reader needs a forced stop and leaves an unfinalized MP4, record a failed sample and use bridge and go2rtc progress to locate the stall.

Collect each `summary.json`, MP4, reader and decoder log, and progress file under ignored `local/store-results/concurrency/`. The bridge writes UTC last-frame times, RTP and output gap maxima, assembler drops, and retry events to private `~/.config/uzy-cctv/*.health.log` files. Its startup budget covers auth through first main-stream output, with at most two consecutive failed starts and a three-second retry delay. An eight-second output stall triggers a new session. Live RTP reassembly keeps the 2048-packet capacity for large IDRs and expires a blocked head after 500 ms of advancing RTP timestamps. Assembler drops count sequence positions discarded by reassembly and can include discarded fragments. Sample the loopback API counters during the capture to compare producer and consumer progress with bridge and reader progress.

Once the samples and logs are copied, remove the exact temporary staging directory after verifying its path under `/home/cameraops/probes/`. Remove the test cache file at `~/.config/uzy-cctv/cache/auth.json` after the readers disconnect. Confirm the temp files are gone, the service is active, and the listeners and UFW still match the private configuration. See [current status](../STATUS.md) for measured results.
