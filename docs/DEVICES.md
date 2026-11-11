# Camera and network inventory

## Fleet

| Role | Count | Known facts | Remaining identity work |
|---|---:|---|---|
| Store cameras | 3 | The dedicated bridge account sees exactly three `CG625B-TN` units on firmware `1.19.900`; current app/network identities were cross-checked on November 6. | Recheck current account visibility and power on site. |
| Home lab camera | 1 | Owned spare CG6, label input 5 V/1.5 A; paired to a dedicated VicoHome lab account. | Keep separate from production camera names and credentials. |

The laptop was reverified on the shop Wi-Fi on November 6. The historical three LAN candidates were online but did not match the three MAC/IP identities returned by VicoHome, so they are no longer classified as cameras. The detailed current mapping and SSIDs are in ignored `local/private/store-network.md`; no Wi-Fi password is stored here.

## Store onboarding snapshot — November 6, 2026

| App label | Model / firmware | Power state after tests | Bridge result | Physical confirmation |
|---|---|---|---|---|
| Dining Area | CG625B-TN / 1.19.900 | 90%, API charging flag off at last poll | Updated bridge: 445 frames at 2304×1296; full decode clean. | Owner confirmed the view. |
| Cash Register | CG625B-TN / 1.19.900 | 100% after owner powered it; API charging flag remained off | Updated bridge: 447 frames at 2304×1296; full decode clean. | Owner confirmed the view. |
| Kitchen Back | CG625B-TN / 1.19.900 | 63%, API charging flag off at last poll | Updated bridge: 413 frames at 2304×1296; full decode clean. | Owner confirmed the view. |

All three updated short probes delivered fully decodable media on their first offer. Earlier attempts showed intermittent no-answer signaling on Kitchen and Cash. The updated probes still reported 626 assembler drops on Dining and 437 on Kitchen, while Cash reported zero; these counts need investigation before continuous recording. The owner elected to skip endurance testing. These short checks do not establish uninterrupted operation or remote VPS transport.

Remote access to store Wi-Fi is not literal Wi-Fi association from home. The current plan is vendor WebRTC media to the VPS. A store-side gateway is a measured fallback if the remote ICE path fails.

## To collect before three-camera deployment

- Recheck the three verified camera mappings and dedicated bridge-account visibility without interrupting the owner's regular app.
- Power method and whether each can remain powered during all store hours.
- Busy-shift bitrate, signal strength, and observed recording gaps.
- Store upload speed and whether cameras share a network with other critical devices.

Do not put camera serials, QR pairing codes, passwords, or app tokens in this file.
