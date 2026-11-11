"""Offline tests for bounded RTP-to-counters controlled comparison harness."""

import json
import os
from pathlib import Path
import sys
import tempfile
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parent))
import counters_compare as cc


FAKE_BRIDGE = """
import argparse, json, sys, time
from pathlib import Path

args = sys.argv[1:]
if args[:2] == ["devices", "list"]:
    print(json.dumps([
        {"deviceName": "Dining Area", "serialNumber": "SER_DINING", "batteryLevel": 100, "isCharging": 2},
        {"deviceName": "Cash Register", "serialNumber": "SER_CASH", "batteryLevel": 100, "isCharging": 2},
        {"deviceName": "Kitchen Back", "serialNumber": "SER_KITCHEN", "batteryLevel": 10, "isCharging": 0},
    ]))
    sys.exit(0)

p = argparse.ArgumentParser()
p.add_argument("cmd1")
p.add_argument("cmd2")
p.add_argument("serial")
p.add_argument("--continuous", action="store_true")
p.add_argument("--counters-only", action="store_true")
p.add_argument("--timeout", default="35s")
p.add_argument("--retry-limit", default="0")
p.add_argument("--progress-every", default="250ms")
p.add_argument("--health-log", required=True)
p.add_argument("--startup-lock-file", default="")
p.add_argument("--max-duration", default="10s")
ns = p.parse_args()

alias_map = {"SER_DINING": "dining", "SER_CASH": "cash", "SER_KITCHEN": "kitchen"}
tag = alias_map[ns.serial]
with open(ns.health_log, "w", encoding="utf-8") as f:
    f.write("2026/11/11 19:00:00.000000 event=attempt attempt=1 ts_utc=2026-11-11T19:00:00.000000Z\\n")
    f.write("2026/11/11 19:00:00.100000 event=wake wake_code=0 wake_ms=50 elapsed_ms=100 ts_utc=2026-11-11T19:00:00.100000Z\\n")
    f.write("2026/11/11 19:00:00.200000 event=counters_only pli=true profile=2k\\n")
    f.write("2026/11/11 19:00:00.250000 event=counters_ready elapsed_ms=250 first_rtp_ms=200 ice=relay/prflx profile=2k dim=2304x1296 ts_utc=2026-11-11T19:00:00.250000Z\\n")
    gaps = 4 if tag == "kitchen" else 2
    rx_errs = 2 if tag == "dining" else 0
    err_str = "read:ECONNRESET" if rx_errs else "-"
    f.write(
        f"2026/11/11 19:00:01.000000 event=progress auth=true peer=true answer=true channel=true start=true ack=true "
        f"packets=120 samples=0 drops=0 seq_gaps={gaps} seq_missing={gaps} seq_late=0 asm_drops=0 asm_late=0 frames=0 "
        f"last_rtp_ms=1000 last_frame_ms=-1 last_frame_utc=- max_rtp_gap_ms=420 max_frame_gap_ms=-1 state=connected "
        f"seq_dup=0 seq_unknown=0 profile=2k dim=2304x1296 post_kbps=1010.5 inst_kbps=1020.0 rtp_errs=0 "
        f"rd_wait_ms=8/12/20 rd_lag_ms=1.1/2.2/3.5 push_ms=0.0/0.0/0.0 pop_ms=0.0/0.0/0.0 wr_ms=0.0/0.0/0.0 "
        f"busy_ms=0.1/0.3/0.8 wr_slow=0 rx_udp=140 rx_rtp=120 rx_rtcp=10 rx_stun=8 rx_dtls=2 rx_vpk=120 "
        f"rx_gaps={gaps} rx_missing={gaps} rx_skipped={gaps} rx_late=0 rx_dup=0 rx_max_vgap_ms=420 rx_max_iat_ms=415 "
        f"rx_errs={rx_errs} rx_timeouts=1 rx_closed=0 rx_last_err={err_str}\\n"
    )
    f.write(
        f"2026/11/11 19:00:02.500000 event=session_end reason=max_duration offers=1 join_code=0 answer=true start_ack=true "
        f"packets=240 samples=0 drops=0 seq_gaps={gaps} seq_missing={gaps} seq_late=0 asm_drops=0 asm_late=0 frames=0 "
        f"last_frame_ms=-1 max_rtp_gap_ms=420 max_frame_gap_ms=-1 profile=2k dim=2304x1296 post_kbps=1005.0 "
        f"ice=relay/prflx rtp_errs=0 rx_udp=260 rx_rtp=240 rx_rtcp=10 rx_stun=8 rx_dtls=2 rx_vpk=240 "
        f"rx_gaps={gaps} rx_missing={gaps} rx_skipped={gaps} rx_late=0 rx_dup=0 rx_max_vgap_ms=420 rx_max_iat_ms=415 "
        f"rx_errs={rx_errs} rx_timeouts=1 rx_closed=2 rx_last_err={err_str}\\n"
    )
time.sleep(0.15)
"""


class CountersCompareOfflineTest(unittest.TestCase):
    def test_staggered_counters_compare_and_cleanup(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = Path(tmp)
            fake_cli = tmp_path / "fake_cli.py"
            fake_cli.write_text(FAKE_BRIDGE, encoding="utf-8")

            run_dir = tmp_path / "run"
            run_dir.mkdir()
            (run_dir / "auth").mkdir()
            (run_dir / "auth" / "auth.json").write_text("{}", encoding="utf-8")
            (run_dir / "inventory.json").write_text(json.dumps([
                {"deviceName": "Dining Area", "batteryLevel": 100, "isCharging": 2},
                {"deviceName": "Cash Register", "batteryLevel": 100, "isCharging": 2},
                {"deviceName": "Kitchen Back", "batteryLevel": 12, "isCharging": 0},
            ]), encoding="utf-8")

            base_dir = tmp_path / "baseline"
            base_dir.mkdir()
            (base_dir / "dining_area.bridge.log").write_text(
                "2026/11/11 18:00:00.000000 event=attempt attempt=1\n"
                "2026/11/11 18:00:01.000000 event=first_frame dim=2304x1296 ice=relay/prflx ts_utc=2026-11-11T18:00:01Z\n"
                "2026/11/11 18:00:03.000000 event=session_end packets=100 frames=10 seq_gaps=3 seq_missing=3 asm_drops=12 rx_gaps=3 rx_missing=3 rx_errs=0 rx_timeouts=0 rx_closed=0 rx_last_err=- rtp_errs=0\n",
                encoding="utf-8",
            )
            for other in ("cash_register", "kitchen_back"):
                (base_dir / f"{other}.bridge.log").write_text("", encoding="utf-8")

            udp_calls = [0]

            def fake_udp():
                udp_calls[0] += 1
                return {"received": 1000 * udp_calls[0], "no_ports": 0, "receive_errors": 0 if udp_calls[0] == 1 else 2}

            devices = {"dining_area": "SER_DINING", "cash_register": "SER_CASH", "kitchen_back": "SER_KITCHEN"}
            report = cc.run_counters_compare(
                run_dir, devices, dict(os.environ, VICOHOME_DEBUG="1"),
                wall_s=6.0, startup_timeout_s=5,
                bridge_cmd=[sys.executable, str(fake_cli)],
                udp_fn=fake_udp, baseline_dir=base_dir, poll_interval=0.05,
            )

            self.assertFalse((run_dir / "auth").exists())
            self.assertFalse((run_dir / "start.lock").exists())
            for alias in ("dining_area", "cash_register", "kitchen_back"):
                proc_log = run_dir / f"{alias}.proc.log"
                proc_txt = proc_log.read_text(encoding="utf-8", errors="replace") if proc_log.exists() else "<missing>"
                self.assertEqual(report["cameras"][alias].get("exit_code"), 0, msg=f"{alias} log:\n{proc_txt}")
            self.assertEqual(report["udp_delta"]["receive_errors"], 2)
            self.assertGreater(report["common_concurrent_s"], 1.5)
            d = report["cameras"]["dining_area"]
            self.assertEqual(d["total_packets"], 240)
            self.assertEqual(d["total_frames"], 0)
            self.assertEqual(d["asm_drops_sum"], 0)
            self.assertEqual(d["seq_gaps_sum"], 2)
            self.assertEqual(d["rx_gaps_sum"], 2)
            self.assertEqual(d["rx_errs_sum"], 2)
            self.assertEqual(d["rx_last_errs"], ["read:ECONNRESET"])
            self.assertEqual(report["battery_context"]["end"]["kitchen_back"]["battery"], 10)
            self.assertIn("comparison", report)
            self.assertEqual(report["comparison"]["dining_area"]["full_baseline"]["asm_drops_sum"], 12)


if __name__ == "__main__":
    unittest.main()
