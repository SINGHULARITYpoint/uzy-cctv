"""Offline tests for filtered PktMon orchestration, preflight guards, and cleanup."""

import json
import os
from pathlib import Path
import sys
import tempfile
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parent))
import pktmon_compare as pc


FAKE_HOLD_CLI = """
import argparse, json, sys, time
from datetime import datetime, timezone
from pathlib import Path

args = sys.argv[1:]
p = argparse.ArgumentParser()
p.add_argument("cmd1")
p.add_argument("cmd2")
p.add_argument("serial")
p.add_argument("--continuous", action="store_true")
p.add_argument("--counters-only", action="store_true")
p.add_argument("--timeout", default="30s")
p.add_argument("--retry-limit", default="0")
p.add_argument("--progress-every", default="250ms")
p.add_argument("--health-log", required=True)
p.add_argument("--startup-lock-file", default="")
p.add_argument("--max-duration", default="10s")
p.add_argument("--hold-start-live-file", default="")
ns = p.parse_args()

alias = "dining_area" if ns.serial == "SER_DINING" else "cash_register"
rx_csv = Path(ns.health_log + ".rx.csv")
flows_csv = Path(ns.health_log + ".rx.csv.flows.csv")
ep_json = Path(ns.health_log + ".rx.csv.endpoints.json")
now_iso = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%fZ")

lport = 51000 if alias == "dining_area" else 52000
ep_json.write_text(json.dumps({
    "start_utc": now_iso,
    "ice_servers": ["203.0.113.10:3478"],
    "flows": [{"id": 0, "dir": "rx", "local_ip": "192.168.1.213", "local_port": lport, "remote_ip": "203.0.113.10", "remote_port": 3478, "first_us": 1, "last_us": 1000, "packets": 10, "bytes": 1000}],
}), encoding="utf-8")
flows_csv.write_text(f"# start_utc={now_iso} fields=t_us,dir,flow_id,kind,encap,ssrc,seq,bytes,marker,fmt,meta\\n", encoding="utf-8")
rx_csv.write_text(f"# start_utc={now_iso} fields=t_us,stream,seq,bytes,marker\\n", encoding="utf-8")

with open(ns.health_log, "w", encoding="utf-8") as f:
    f.write("2026/11/11 20:00:00.000000 event=attempt attempt=1\\n")
    if ns.hold_start_live_file:
        f.write("2026/11/11 20:00:00.100000 event=hold_ready elapsed_ms=100\\n")
        f.flush()
        while not Path(ns.hold_start_live_file).exists():
            time.sleep(0.03)
    f.write(f"2026/11/11 20:00:00.200000 event=counters_ready elapsed_ms=200 ts_utc={now_iso}\\n")
    f.write(f"2026/11/11 20:00:01.000000 event=progress packets=100 seq_gaps={'3' if alias=='cash_register' else '0'}\\n")
    f.flush()
time.sleep(0.2)
"""


class PktMonCompareOfflineTest(unittest.TestCase):
    def test_resolves_wifi_from_windows_grouped_components_and_flat_components(self):
        adapter = {"Name": "Wi-Fi", "InterfaceDescription": "Example Wi-Fi adapter",
                   "Status": "Up", "MacAddress": "02-00-00-00-00-01", "IPAddress": "192.0.2.10"}
        component = {"Id": 13, "Name": "Example Wi-Fi adapter", "Type": "Miniport"}
        grouped = [{"Group": "Example Wi-Fi adapter", "Components": [component,
                    {"Id": 67, "Name": "TCPIP", "Type": "Protocol"}]}]
        for listing in (grouped, [component]):
            with self.subTest(grouped="Components" in listing[0]):
                actual = pc.resolve_wifi_adapter(json.dumps(listing), json.dumps(adapter))
                self.assertEqual(actual["component_id"], 13)
                self.assertEqual(actual["ip"], adapter["IPAddress"])
                self.assertEqual(pc.pktmon_components(json.dumps(listing))[0], component)

    def test_windows_empty_filter_listing_and_existing_filter_guard(self):
        class Result:
            returncode = 0
            stderr = ""

            def __init__(self, stdout):
                self.stdout = stdout

        def query_with_listing(listing):
            return lambda args: Result("Logger status: Not running." if args[1] == "status" else listing)

        pc.check_pktmon_clean(query_with_listing("\r\nPacket Filters:\r\n    None\r\n"))
        with self.assertRaisesRegex(RuntimeError, "Pre-existing PktMon filters"):
            pc.check_pktmon_clean(query_with_listing("Packet Filters:\n    ExistingCapture UDP\n"))

    def test_refuses_non_admin_and_clobbering_unrelated_session(self):
        with tempfile.TemporaryDirectory() as tmp:
            with self.assertRaises(PermissionError):
                pc.run_pktmon_compare(
                    Path(tmp), {"dining_area": "A", "cash_register": "B"}, os.environ,
                    admin_fn=lambda: False,
                )

            class R:
                def __init__(self, rc=0, out="", err=""):
                    self.returncode, self.stdout, self.stderr = rc, out, err

            # Must abort cleanly if another pktmon session is running
            with self.assertRaises(RuntimeError) as ctx:
                pc.run_pktmon_compare(
                    Path(tmp), {"dining_area": "A", "cash_register": "B"}, os.environ,
                    admin_fn=lambda: True,
                    tool_fn=lambda args, **kw: R(0, "Logger status: Running"),
                )
            self.assertIn("already active", str(ctx.exception))

    def test_filter_generation_and_lifecycle_with_mocked_pktmon(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = Path(tmp)
            fake_cli = tmp_path / "fake_hold_cli.py"
            fake_cli.write_text(FAKE_HOLD_CLI, encoding="utf-8")
            run_dir = tmp_path / "run"
            run_dir.mkdir()
            (run_dir / "auth").mkdir()
            (run_dir / "auth" / "auth.json").write_text("{}", encoding="utf-8")

            calls = []
            state = {"running": False, "filters": []}

            class R:
                def __init__(self, rc=0, out="", err=""):
                    self.returncode, self.stdout, self.stderr = rc, out, err

            def fake_tool(args, timeout=20):
                calls.append(args)
                if args[:2] == ["pktmon", "status"]:
                    return R(0, "Logger status: Not running." if not state["running"] else "Logger status: Running.")
                if args[:3] == ["pktmon", "filter", "list"]:
                    if not state["filters"]:
                        return R(0, "All packets allowed (no filters active)")
                    rows = "\n".join(f"{i+1}  {name}  UDP  192.168.1.213, 203.0.113.10  3478" for i, name in enumerate(state["filters"]))
                    return R(0, "#  Name  Protocol  IP  Port\n" + rows)
                if args[:3] == ["pktmon", "filter", "add"]:
                    state["filters"].append(args[3])
                    return R(0, "")
                if args[:3] == ["pktmon", "filter", "remove"]:
                    state["filters"].clear()
                    return R(0, "")
                if args[:2] == ["pktmon", "start"]:
                    self.assertIn("--pkt-size", args)
                    self.assertEqual(args[args.index("--pkt-size") + 1], "160")
                    self.assertIn("--file-size", args)
                    self.assertEqual(args[args.index("--file-size") + 1], "32")
                    self.assertEqual(args[args.index("--log-mode") + 1], "circular")
                    state["running"] = True
                    Path(args[args.index("--file-name") + 1]).write_bytes(b"ETL")
                    return R(0, "")
                if args[:2] == ["pktmon", "counters"]:
                    return R(0, "[]")
                if args[:2] == ["pktmon", "stop"]:
                    state["running"] = False
                    return R(0, "")
                if args[:2] == ["pktmon", "etl2txt"]:
                    return R(0, "Events lost: 0\nBuffers lost: 0\n")
                if args[:2] == ["pktmon", "etl2pcap"]:
                    Path(args[args.index("--out") + 1]).write_bytes(b"PCAPNG")
                    return R(0, "")
                if "correlate-capture" in args:
                    return R(0, json.dumps({"firstLossBoundary": "inconclusive_no_reproduced_loss", "filterCoverageComplete": True}))
                return R(0, "")

            summary = pc.run_pktmon_compare(
                run_dir,
                {"dining_area": "SER_DINING", "cash_register": "SER_CASH"},
                os.environ,
                solo_s=0.15, pair_s=0.3, wall_s=4.0,
                bridge_cmd=[sys.executable, str(fake_cli)],
                tool_fn=fake_tool,
                admin_fn=lambda: True,
                adapter_probe_fn=lambda: {"component_id": 5, "ip": "192.168.1.213", "mac": "4C-82-A9-A8-E3-7F", "name": "Wi-Fi", "comp_list": [{"Id": 5, "Name": "MediaTek Wi-Fi"}]},
                poll_interval=0.04,
            )

            self.assertEqual(summary["firstLossBoundary"], "inconclusive_no_reproduced_loss")
            self.assertFalse((run_dir / "auth").exists())
            self.assertFalse((run_dir / "start.lock").exists())
            self.assertFalse((run_dir / "dining.release").exists())
            self.assertFalse(state["running"])
            self.assertEqual(state["filters"], [])
            # Verify filter add & pktmon start happened in order
            verbs2 = [" ".join(c[:2]) for c in calls if c[0] == "pktmon"]
            self.assertIn("pktmon filter", verbs2)
            self.assertIn("pktmon start", verbs2)
            self.assertLess(verbs2.index("pktmon filter"), verbs2.index("pktmon start"))
            self.assertLess(verbs2.index("pktmon start"), verbs2.index("pktmon stop"))
            self.assertEqual(verbs2[-4:], ["pktmon filter", "pktmon etl2txt", "pktmon etl2pcap", "pktmon etl2pcap"])


if __name__ == "__main__":
    unittest.main()
