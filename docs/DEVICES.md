# Camera and network inventory

## Fleet

| Role | Count | Known facts | Remaining identity work |
|---|---:|---|---|
| Store cameras | 3 | Owned GMK CG6 units from a wireless four-pack; three matching LAN candidates were reachable during the November 5 store survey. | Match each physical location and VicoHome name to its device/MAC; confirm power and placement. |
| Home lab camera | 1 | Owned spare CG6, label input 5 V/1.5 A; paired to a dedicated VicoHome lab account. | Keep separate from production camera names and credentials. |

The store survey observed a Spectrum `SAX1V1R` router on a `192.168.1.0/24` LAN. Camera candidates did not answer standard HTTP/HTTPS, SSDP, or ONVIF discovery. That does not rule out a vendor-specific local service or the later WebRTC path. DHCP addresses may change. The detailed MAC/IP mapping and SSIDs are in ignored `local/private/store-network.md` for use on this laptop; no Wi-Fi password is stored here.

Remote access to store Wi-Fi is not literal Wi-Fi association from home. The current plan is vendor WebRTC media to the VPS. A store-side gateway is a measured fallback if the remote ICE path fails.

## To collect before three-camera deployment

- Camera name and physical view for each of the three store units.
- VicoHome sharing or dedicated bridge-account access for all three, without interrupting the owner's regular app.
- Power method and whether each can remain powered during all store hours.
- Busy-shift bitrate, signal strength, and observed recording gaps.
- Store upload speed and whether cameras share a network with other critical devices.

Do not put camera serials, QR pairing codes, passwords, or app tokens in this file.
