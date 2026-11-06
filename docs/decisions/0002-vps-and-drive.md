# ADR 0002: VPS host and Drive archive trial

**Status:** provisional, November 6, 2026.

## Decision

Use the existing dedicated Hetzner CX23 for bridge, recording orchestration, indexing, and private dashboard. Trial the owner's Google Drive account (reported 5 TB, about 60 GB used) as a seven-day archive for finalized video segments. Keep a bounded local upload queue. Do not record directly onto a Drive mount.

## Why and conditions

The owner already has both resources. The 40 GB VPS disk cannot hold seven days of three feeds, while Drive capacity appears sufficient at measured and plausible bitrates. Capacity alone does not prove upload/retrieval speed, API behavior, or suitable playback. M4 must verify those end to end. A Hetzner Volume is an option if local outage buffering needs more space.

## Revisit if

Remote media is unreliable, three-feed CPU/memory exceeds the CX23, or Drive upload and history playback fail their acceptance tests.
