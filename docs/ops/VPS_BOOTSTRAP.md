# VPS bootstrap runbook

**Target:** Hetzner project Uzy's NY Pizza, server `uzy-bridge`, ID `138377676`, CX23 in Nuremberg. The owner explicitly authorized a clean rebuild and declined a backup of its existing contents. As of the last verified check, the server was **not yet rebuilt**.

## 1. Finish local CLI access

`hcloud` 1.70.2 was installed on the Windows laptop. A new project read/write token was created in Hetzner Console; the one-time token was not copied into this repository or chat. The last verified CLI context list was empty. On the laptop, the owner was asked to copy the token from the open console dialog and run:

```powershell
hcloud context create uzy-cctv
hcloud context list
```

Do not paste a token into a chat, command argument, committed file, or terminal transcript. If the one-time dialog was closed before copying, create a new token in the correct project and revoke the unusable one.

## 2. Verify the exact destructive target

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

Review the generated file's user and SSH-key fields without sharing the private key. It creates `cameraops`, disables password and root SSH login, installs base diagnostic packages, and enables UFW with SSH allowed. It does not install camera services or put credentials on the server.

## 4. Rebuild through the CLI

Verify that the desired Ubuntu LTS image is available using `hcloud image list`. The planned image is `ubuntu-24.04`; adjust only if availability or compatibility requires it. From the project root:

```powershell
hcloud server rebuild --image ubuntu-24.04 --user-data-from-file infra/generated/cloud-init.yaml 138377676
```

This is the destructive step already authorized for this exact server. Record the actual image, time, and result in `docs/STATUS.md`. The server's SSH host key will change. Use the fresh IP from `hcloud server describe 138377676`, not a stale value from notes.

## 5. Verify first boot

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

## Next after bootstrap

Upload and run the Go bridge **only for the lab camera** with a dedicated account. Measure clean decoded media and the remote ICE route before scheduling three-camera store capture. See [roadmap](../ROADMAP.md).
