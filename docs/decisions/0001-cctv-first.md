# ADR 0001: Ship CCTV before activity analysis

**Status:** accepted by owner, November 5, 2026.

## Decision

The first release provides private remote live viewing of three store cameras and seven days of continuous history. AI detection and employee activity features are deferred until the CCTV acceptance test passes.

## Why

The owner needs a dependable camera system first. Recording integrity, remote transport, and retrieval are currently unproven. Adding analytics now would obscure the reliability work and spend limited CX23 capacity.

## Consequences

The MVP PRD contains no detector dependency. Later activity work gets its own PRD and compute benchmark. The dashboard must show outages explicitly so later analysis cannot mistake missing footage for inactivity.
