# ADR 0004: go2rtc for private live restream

**Status:** Accepted for the live restream layer on November 6, 2026.

Use [go2rtc](https://github.com/AlexxIT/go2rtc) v1.9.15 (MIT) for local RTSP/API/WebRTC restreaming. Its [exec pipe source](https://github.com/AlexxIT/go2rtc/blob/master/internal/exec/README.md) accepts raw H.264. Keep the existing CG6 bridge for vendor authentication, wake, signaling, and RTP assembly; `vicostream` also emits H.264 but would require a second, unverified camera integration and an externally supplied token.

The bridge's `--continuous --output -` mode retries after signal closure or stalled media. Its stdout begins at the main-stream SPS/PPS and IDR after the CG6 preview-to-main transition. go2rtc starts the bridge on demand for each named camera. A systemd unit starts go2rtc at boot. API, RTSP, and WebRTC listen on loopback only; UFW exposes only SSH. The three source names and their serials are in a private server config, and dedicated-account credentials are in a mode-600 environment file. No public viewer is available yet.

One Dining RTSP remux produced 202 main-resolution frames and a clean raw-frame decode. Cash worked alone. A brief three-source overlap failed its concurrency gate: Kitchen recorded about 10 seconds, Dining about 3.6 seconds, and Cash produced no playable track. This may reflect simultaneous wake/session behavior; the test does not isolate the cause. Individual decoded frames in the saved clips were clean, but missing time is a recording outage. Do not mark this layer as continuous CCTV or start archive retention from these results.

Per-camera startup, last-frame health, bounded readers, and two concurrent rounds localized repeated stalls to the bridge output or earlier. Continue bridge recovery development locally with deterministic RTP loss tests and a brief owned-camera keyframe-recovery check. Promote a passing revision to the loopback-only VPS restream for the bounded three-camera acceptance round. A private owner access method is required before exposing the go2rtc API or live video.
