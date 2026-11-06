# Current project state

**Last verified:** November 6, 2026. This is the first file to update after a test or deployment.

## What exists

- Dedicated local Git workspace with the Go lab bridge under `services/vico-bridge` and product/engineering documentation under `docs/`.
- One owned home-lab GMK CG6 paired to a dedicated VicoHome lab account. Three owned cameras remain at the store. Do not put account credentials or device serials in this repository.
- A sanitized [device inventory](DEVICES.md) is committed; detailed survey-time LAN identifiers are in ignored `local/private/store-network.md` on this laptop.
- Go bridge prototype can authenticate, obtain a WebRTC ticket, signal, receive H.264 RTP, open the data channel, and send `startLive` for the lab camera.
- A 35-minute powered lab session received 253,782 video RTP packets and about 117.8 MB of raw H.264. Packet-bearing seconds were 99.8% of the observed interval; longest measured packet gap was 3.96 seconds. Battery reported 99% at start and 100% at end. The active session outlived the ticket's 30-minute validity.
- The long file had decode errors. A one-minute SampleBuilder follow-up also had decode errors and selected local `srflx` / remote `relay` ICE candidates. The recorder is **not production ready**.
- A failed immediate restart led to a signaling re-offer change. The real-camera test after that change succeeded on its first offer, so the specific retry branch remains unproven.

## VPS and storage state

- Hetzner project: Uzy's NY Pizza. Target server: `uzy-bridge`, ID `138377676`, CX23 x86, Nuremberg, 2 shared vCPU, 4 GB RAM, 40 GB local disk.
- Owner authorized wiping this dedicated server's current contents and explicitly declined a backup of that old content. **No rebuild or wipe had occurred at the last verified check.** No old-content backup was made.
- Hetzner CLI `hcloud` 1.70.2 was installed on the Windows laptop. A new project read/write token was created in the console, but browser security blocked transferring its one-time value into the local CLI. The last verified `hcloud context list` was empty. The owner was asked to run `hcloud context create uzy-cctv` locally and paste the token directly at its prompt. Recheck this; do not assume it happened.
- Go 1.27.3 is now installed through Scoop. `scripts/build-bridge.ps1` passed the focused device tests and built the bridge on this machine. The first sandboxed test attempt could not bind its loopback test socket; the subsequent unrestricted local test passed.
- The owner reports 5 TB Google Drive capacity and about 60 GB used. No archive folder, OAuth integration, upload queue, or playback service has been built.

## Next engineering gate

1. **At the shop now:** verify the current Wi-Fi connection, remap the three store CG6 devices, and test each with the existing Go bridge. Follow the [fresh-chat handoff](handoffs/STORE_CAMERA_ONBOARDING.md). Historical IPs may have changed; do not assume the previous survey still matches.
2. Fix H.264 frame assembly/stream transitions and retry behavior. Require clean decoded segments.
3. Then verify the Hetzner CLI context and exact server state; rebuild the authorized server with a current Ubuntu LTS image. See [bootstrap runbook](ops/VPS_BOOTSTRAP.md).
4. Run a CG6 bridge on the VPS across networks. Record selected ICE path, bitrate, gaps, decoded quality, and reconnects. Decide whether the VPS-only path is viable or a store-side gateway is needed.
5. Only then implement the private live viewer, continuous recording, and Drive-backed seven-day history.

## What is deliberately absent

No camera footage, emulator image, packet capture, raw app log, cached token, binary, or VPS backup was copied into this project. The original experiments remain in the prior Codex work folder, which is not a dependency of this workspace. The summarized observations needed to continue are in [CG6 transport research](research/CG6_TRANSPORT.md).

## Update format

After each meaningful change, append a dated entry with: action, environment, exact result, evidence path or command, and remaining limitation. Keep claims narrower than the test.

### Log

- **2026-11-06:** Created dedicated workspace and imported source-only Go bridge. Installed Go 1.27.3; focused tests and build passed. VPS and camera services remain unchanged.
- **2026-11-06:** Owner moved work to the shop and prioritized three-camera on-site discovery and connection tests before the Hetzner CLI token handoff. No new store-camera mapping or live test has been performed yet.
