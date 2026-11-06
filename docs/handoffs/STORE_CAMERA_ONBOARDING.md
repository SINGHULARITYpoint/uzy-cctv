# Fresh-chat prompt: onboard the three store cameras

Use the text below as the opening request for a new chat in this project. The owner says they are physically at Uzy's NY Pizza with the laptop and the three owned GMK CG6 cameras. Verify the laptop's actual connection before relying on that statement.

---

We are at Uzy's NY Pizza and want to use this on-site time to onboard our three existing GMK CG6 cameras. Work in the `UZY CCTV` project folder. Read `AGENTS.md`, `docs/STATUS.md`, `docs/PRD.md`, `docs/DEVICES.md`, `docs/research/CG6_TRANSPORT.md`, and the ignored `local/private/store-network.md` first. The previous home-lab camera test and Go bridge are already in this repository under `services/vico-bridge`.

Focus this chat on the store cameras. Defer the Hetzner CLI token handoff, VPS rebuild, Google Drive integration, dashboard, and AI activity work until the on-site findings are documented.

First verify the laptop's current Wi-Fi interface, SSID, subnet, gateway, and route. The old store IP/MAC inventory is historical; identify the three current camera clients without assuming their DHCP addresses are unchanged. Use the known candidate MACs to narrow discovery and cross-check the VicoHome device list. Help me map each device to its physical location and app name, using a brief live view or a deliberate walk-by if needed. Keep exact camera identifiers and network details in ignored `local/private/`; put only a sanitized summary in `docs/DEVICES.md` and `docs/STATUS.md`.

Then determine whether the dedicated bridge account can see all three store cameras. If sharing is needed, guide me through the VicoHome app's normal sharing flow; do not ask me to paste passwords or verification codes into chat or project files. Avoid signing the CLI into an account that would kick out the owner's regular app session. Do not reset, reflash, unpair, or change the cameras' Wi-Fi or production settings.

Build the existing Go bridge with `scripts/build-bridge.ps1`. For each camera, run a short ticket/signaling/media probe sequentially, starting with a 15–60 second test. Capture model/firmware, power/battery state, selected ICE path, resolution, RTP gaps, and whether a full `ffmpeg` decode succeeds. Keep any raw footage and credential cache under ignored `local/`; remove temporary auth tokens after tests. If short tests are clean, use our available shop time for a longer powered endurance run on one camera. Do not claim the system is production-ready from short tests.

Finish by giving me a three-camera mapping and a per-camera pass/fail table with precise evidence and blockers. Update the project status and roadmap with the results and the next engineering step. We have authorized targeted discovery and testing of our owned cameras on our shop network; proceed autonomously with read-only discovery and reversible tests, asking for my help only when a physical identification or app-sharing step requires me.
