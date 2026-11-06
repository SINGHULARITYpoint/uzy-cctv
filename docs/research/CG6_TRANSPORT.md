# CG6 transport research

**Source:** November 5, 2026 experiments with one owned spare GMK CG6 and VicoHome 4.7.0. Raw logs and video stayed in the old lab folder; this note contains the findings needed to continue without them.

## Device and protocol

- Retail model: GMK CG6 Smart Battery Camera; observed internal model family `CG625B-TN`.
- Label power input: 5 V DC, 1.5 A. The lab camera was externally powered during the long test.
- Main stream observed: H.264, 2304×1296. Preview: H.264, 640×360. PCMU audio packets were observed, but audio is outside the current MVP.
- The VicoHome app gets a short-lived WebRTC ticket from its API, uses a WebSocket for signaling, negotiates ICE/DTLS, then starts live media over WebRTC. The current Go bridge can reproduce this for the lab camera. This is **not full ecosystem independence**: account authentication, ticketing, and signaling still use vendor services.
- A VicoHome app LAN session selected a direct camera host candidate. A later Go test on the same home LAN selected local `srflx` and remote `relay`. The path varies; neither result predicts the VPS route.
- Logging into the bridge account from the CLI once invalidated the Android emulator's app session. A dedicated bridge account/session is needed.

## Test chronology

| Test | Result | Limit |
|---|---|---|
| Short independent capture | About 15 seconds, 3,253 video RTP packets; a packaged main-stream MP4 fully decoded. | Does not establish long-run continuity. |
| Powered endurance | 35 minutes, 253,782 video RTP packets, 117.8 MB raw H.264; 99.8% of observed seconds had packets; max gap 3.96 s; battery 99% → 100%. | Full raw-file decode reported errors. Only on home network. |
| Immediate restart | Multiple peer events but no SDP answer, zero video packets. | Triggered signaling re-offer change. |
| Reassembly follow-up | About one minute, 14,551 video RTP packets; selected remote relay; received samples. | Still reported decode errors and packet drops. Retry branch did not need to fire. |

The active 35-minute session continued beyond the ticket's 30-minute validity period. That does not prove a fresh ticket, indefinite streaming, or uninterrupted production recording.

## Engineering hypotheses to test

1. RTP packet loss or reordering before H.264 frame assembly.
2. Incorrect handling of the preview-to-main resolution switch and codec parameter sets.
3. An ICE relay path introducing loss or timing pressure. The relay observation is correlation, not demonstrated cause.
4. Session-wakeup and repeated `PEER_IN`/`PEER_OUT` handling on restart.

Use `ffmpeg` full decode, frame dimensions, RTP sequence/gap metrics, and selected ICE candidate types to discriminate these. The next decisive experiment is a clean one-camera session from the CX23 to the lab camera across networks.

## Public references

- [VicoHome continuous power guidance](https://support.vicohome.io/hc/en-us/articles/55982110155417-Can-I-Keep-My-Camera-Plugged-In-All-the-Time)
- [dydx/vico-cli](https://github.com/dydx/vico-cli), the MIT-licensed CLI base used by this prototype
- [bbielsa/vicostream](https://github.com/bbielsa/vicostream), a related WebRTC bridge reference
- [go2rtc exec input](https://github.com/AlexxIT/go2rtc/blob/master/internal/exec/README.md), a live restream candidate
