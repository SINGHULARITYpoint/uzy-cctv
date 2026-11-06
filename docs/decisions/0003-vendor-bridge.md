# ADR 0003: Use a VicoHome compatibility bridge first

**Status:** provisional, November 6, 2026.

## Decision

Continue the Go WebRTC bridge for the owned CG6 cameras. It obtains VicoHome account authentication and signaling tickets, then receives media directly or through the ICE path selected for each session. Replacing camera firmware or eliminating every vendor service is outside the CCTV MVP.

## Why and conditions

A short independent capture decoded successfully, and a powered session lasted 35 minutes. The existing hardware may therefore support the owner's goal without replacement. Longer recording quality and remote VPS transport still fail or lack proof. A store-side gateway is the fallback if remote ICE cannot meet the quality gate.

## Consequence

Vendor API changes and account-session behavior remain operational risks. The dashboard should report bridge failures distinctly from camera offline events. Keep the protocol adapter isolated so camera hardware can be changed later.
