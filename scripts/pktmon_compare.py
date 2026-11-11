"""Filtered Windows Packet Monitor capture & Dining->Dining+Cash 60s correlator runner."""

import argparse
import ctypes
from datetime import datetime, timezone
import ipaddress
import json
import math
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import time

DASHBOARD_MOD_PATH = Path(__file__).resolve().with_name("store-dashboard.py")


def load_store_dashboard():
    import importlib.util
    spec = importlib.util.spec_from_file_location("store_dashboard_mod", DASHBOARD_MOD_PATH)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def is_admin():
    try:
        return bool(ctypes.windll.shell32.IsUserAnAdmin())
    except Exception:
        return False


def _utc_iso(ts=None):
    return datetime.fromtimestamp(ts if ts is not None else time.time(), tz=timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%fZ")


def run_tool(args, timeout=20):
    return subprocess.run(
        args, capture_output=True, text=True, encoding="utf-8", errors="replace",
        timeout=timeout, creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0),
    )


def pktmon_components(listing):
    groups = json.loads(listing) if isinstance(listing, str) else listing
    if isinstance(groups, dict):
        groups = [groups]
    return [item for group in groups for item in group.get("Components", [group])]


def resolve_wifi_adapter(pktmon_list_raw, net_adapter_json):
    """Resolve the active Wi-Fi interface IP, MAC, and matching PktMon component ID."""
    adapters = json.loads(net_adapter_json) if isinstance(net_adapter_json, str) else net_adapter_json
    if isinstance(adapters, dict):
        adapters = [adapters]
    wifi = None
    for row in adapters:
        name = str(row.get("Name") or "")
        desc = str(row.get("InterfaceDescription") or "")
        if str(row.get("Status") or "").lower() == "up" and (
            "wi-fi" in name.lower() or "wireless" in desc.lower() or "wi-fi" in desc.lower()
        ):
            wifi = row
            break
    if wifi is None:
        raise RuntimeError("No active Wi-Fi adapter found (expected Status=Up Wi-Fi adapter).")

    mac_norm = re.sub(r"[^0-9a-f]", "", str(wifi.get("MacAddress") or "").lower())
    desc_norm = str(wifi.get("InterfaceDescription") or "").lower()
    comp_id = None
    for item in pktmon_components(pktmon_list_raw):
        raw_mac = re.sub(r"[^0-9a-f]", "", str(item.get("MacAddress") or "").lower())
        item_name = str(item.get("Name") or "").lower()
        if (mac_norm and raw_mac == mac_norm) or (desc_norm and desc_norm in item_name):
            comp_id = int(item["Id"])
            break
    if comp_id is None:
        raise RuntimeError("Could not match active Wi-Fi adapter to a PktMon component ID.")
    ip_addr = str(wifi.get("IPAddress") or "")
    if not ip_addr:
        raise RuntimeError("Could not determine IPv4 address for active Wi-Fi adapter.")
    return {"component_id": comp_id, "ip": ip_addr, "mac": wifi.get("MacAddress"), "name": wifi.get("Name")}


def check_pktmon_clean(tool_fn):
    """Verify PktMon is reachable, not running an unrelated capture, and has no active filters."""
    st = tool_fn(["pktmon", "status"])
    if st.returncode != 0:
        msg = (st.stderr or st.stdout or "").strip()
        raise RuntimeError(f"PktMon status failed (exit {st.returncode}): {msg}")
    if "not running" not in (st.stdout or "").lower():
        raise RuntimeError("An unrelated Packet Monitor session is already active; refusing to disturb it.")
    fl = tool_fn(["pktmon", "filter", "list"])
    if fl.returncode != 0:
        raise RuntimeError(f"PktMon filter list failed: {(fl.stderr or fl.stdout).strip()}")
    if re.fullmatch(r"\s*Packet Filters:\s*None\s*", fl.stdout or "", flags=re.IGNORECASE):
        return
    out_lower = (fl.stdout or "").lower()
    if "no filters" not in out_lower and "all packets allowed" not in out_lower and fl.stdout.strip():
        lines = [ln.strip() for ln in fl.stdout.splitlines() if ln.strip() and not ln.strip().startswith("-")]
        if len(lines) > 1:
            raise RuntimeError("Pre-existing PktMon filters are active; refusing to overwrite existing filters.")


def build_session_filter_cmds(adapter_ip, endpoints_doc):
    """Build narrow UDP pktmon filter commands from Dining's resolved flows and session ICE servers."""
    cmds = []
    seen = set()

    def add_rule(label, remote_ip, port_a=0, port_b=0):
        try:
            ipaddress.ip_address(remote_ip)
        except ValueError:
            return
        key = (remote_ip, port_a, port_b)
        if key in seen:
            return
        seen.add(key)
        cmd = ["pktmon", "filter", "add", label, "-d", "IPv4", "-t", "UDP", "-i", adapter_ip, remote_ip]
        if port_a > 0 and port_b > 0:
            cmd.extend(["-p", str(port_a), str(port_b)])
        elif port_a > 0:
            cmd.extend(["-p", str(port_a)])
        cmds.append(cmd)

    idx = 0
    for f in endpoints_doc.get("flows") or []:
        rip = str(f.get("remote_ip") or "")
        lport = int(f.get("local_port") or 0)
        rport = int(f.get("remote_port") or 0)
        if rip and lport > 0 and rport > 0:
            add_rule(f"uzy_flow_{idx}", rip, lport, rport)
            idx += 1

    for srv in endpoints_doc.get("ice_servers") or []:
        if ":" in srv:
            host, _, port_str = srv.rpartition(":")
            host = host.strip("[]")
            port = int(port_str) if port_str.isdigit() else 0
            if port > 0:
                add_rule(f"uzy_ice_{idx}", host, port, 0)
                idx += 1
    if not cmds:
        raise RuntimeError("No valid IPv4 UDP session endpoints or ICE servers resolved for PktMon filter.")
    return cmds


def _graceful_stop(procs, timeout_s=8.0):
    break_sig = getattr(__import__("signal"), "CTRL_BREAK_EVENT", None)
    for proc in procs.values():
        if proc.poll() is None and break_sig is not None:
            try:
                os.kill(proc.pid, break_sig)
            except OSError:
                pass
    deadline = time.monotonic() + timeout_s
    for proc in procs.values():
        if proc.poll() is None:
            rem = max(0.1, deadline - time.monotonic())
            try:
                proc.wait(timeout=rem)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait(timeout=5)


def run_pktmon_compare(directory, devices, env, solo_s=10.0, pair_s=30.0, wall_s=60.0,
                       bridge_cmd=None, tool_fn=None, admin_fn=None, adapter_probe_fn=None, poll_interval=0.25):
    """Run filtered PktMon + 60s Dining-solo -> Dining+Cash --counters-only capture and correlate."""
    sd = load_store_dashboard()
    bridge_cmd = list(bridge_cmd) if bridge_cmd is not None else [str(sd.BRIDGE)]
    tool_fn = tool_fn or run_tool
    admin_fn = admin_fn or is_admin

    if not admin_fn():
        raise PermissionError(
            "Windows Packet Monitor (pktmon) requires Administrator elevation (Access is denied / System error 5). "
            "Run this diagnostic from an elevated Administrator PowerShell prompt."
        )

    directory.mkdir(parents=True, exist_ok=True)
    lock_path = directory / "start.lock"
    lock_path.unlink(missing_ok=True)
    release_file = directory / "dining.release"
    release_file.unlink(missing_ok=True)

    run_env = dict(env, VICO_RX_TRACE="1")
    run_env.setdefault("VICOHOME_CACHE_DIR", str(directory / "auth"))
    run_env.pop("VICOHOME_DEBUG", None)

    check_pktmon_clean(tool_fn)

    if adapter_probe_fn is not None:
        wifi = adapter_probe_fn()
        (directory / "pktmon_comp_list.json").write_text(json.dumps(wifi.get("comp_list") or []), encoding="utf-8")
    else:
        ps_net = (
            "$a = Get-NetAdapter -Physical | Where-Object { $_.Status -eq 'Up' }; "
            "$rows = foreach ($x in $a) { "
            "$ip = (Get-NetIPAddress -InterfaceIndex $x.InterfaceIndex -AddressFamily IPv4 -ErrorAction SilentlyContinue | Select-Object -First 1 -ExpandProperty IPAddress); "
            "[pscustomobject]@{Name=$x.Name;InterfaceDescription=$x.InterfaceDescription;InterfaceIndex=$x.InterfaceIndex;MacAddress=$x.MacAddress;Status=[string]$x.Status;IPAddress=$ip} "
            "}; $rows | ConvertTo-Json"
        )
        net_res = tool_fn(["pwsh", "-NoProfile", "-Command", ps_net])
        if net_res.returncode != 0:
            raise RuntimeError(f"Failed to query Windows network adapters: {net_res.stderr}")
        comp_res = tool_fn(["pktmon", "list", "--json"])
        if comp_res.returncode != 0:
            raise RuntimeError(f"Failed to query pktmon components: {comp_res.stderr or comp_res.stdout}")
        (directory / "pktmon_comp_list.json").write_text(json.dumps(pktmon_components(comp_res.stdout)), encoding="utf-8")
        wifi = resolve_wifi_adapter(comp_res.stdout, net_res.stdout)

    etl_path = directory / "pktmon.etl"
    pcap_path = directory / "pktmon.pcapng"
    drop_pcap_path = directory / "pktmon_drops.pcapng"
    filters_path = directory / "filters_active.txt"
    counters_path = directory / "pktmon_counters.json"
    etl_stats_path = directory / "etl_stats.txt"

    creation_flags = subprocess.CREATE_NO_WINDOW | getattr(subprocess, "CREATE_NEW_PROCESS_GROUP", 0)
    procs = {}
    open_files = []
    filters_installed = False
    capture_active = False

    first_wake_mono = time.monotonic()
    first_wake_utc = _utc_iso()
    capture_start_utc = None
    deadline_mono = first_wake_mono + wall_s

    def launch_cam(alias, extra_args):
        rem = max(5, int(math.ceil(deadline_mono - time.monotonic())))
        cmd = bridge_cmd + [
            "devices", "media-probe", devices[alias],
            "--continuous", "--counters-only",
            "--timeout", "30s",
            "--retry-limit", "0",
            "--progress-every", "250ms",
            "--health-log", f"{alias}.bridge.log",
            "--startup-lock-file", "start.lock",
            "--max-duration", f"{rem}s",
        ] + extra_args
        fh = (directory / f"{alias}.proc.log").open("wb")
        open_files.append(fh)
        procs[alias] = subprocess.Popen(
            cmd, cwd=directory, env=run_env, stdout=fh, stderr=subprocess.STDOUT,
            creationflags=creation_flags, stdin=subprocess.DEVNULL,
        )

    try:
        # 1. Start Dining held before startLive so initial UDP endpoints resolve cleanly before measured capture.
        launch_cam("dining_area", ["--hold-start-live-file", "dining.release"])
        hold_deadline = min(deadline_mono - min(25.0, wall_s * 0.4), time.monotonic() + 20.0)
        ep_path = directory / "dining_area.bridge.log.rx.csv.endpoints.json"
        while time.monotonic() < hold_deadline:
            if sd.first_event_time(directory / "dining_area.bridge.log", "hold_ready") is not None and ep_path.is_file():
                break
            if procs["dining_area"].poll() is not None:
                raise RuntimeError("Dining Area exited before hold_ready/endpoint resolution.")
            time.sleep(0.1)
        if not ep_path.is_file():
            raise RuntimeError("Dining Area did not flush endpoints before cutoff.")

        endpoints_doc = json.loads(ep_path.read_text(encoding="utf-8"))
        for cmd in build_session_filter_cmds(wifi["ip"], endpoints_doc):
            res = tool_fn(cmd)
            if res.returncode != 0:
                raise RuntimeError(f"Failed adding PktMon filter: {(res.stderr or res.stdout).strip()}")
            filters_installed = True

        fl_res = tool_fn(["pktmon", "filter", "list"])
        filters_path.write_text(fl_res.stdout or "", encoding="utf-8")

        capture_start_utc = _utc_iso()
        start_res = tool_fn([
            "pktmon", "start", "--capture",
            "--comp", str(wifi["component_id"]),
            "--type", "all",
            "--pkt-size", "160",
            "--flags", "0x03F",
            "--file-name", str(etl_path),
            "--file-size", "32",
            "--log-mode", "circular",
        ])
        if start_res.returncode != 0:
            raise RuntimeError(f"pktmon start failed: {(start_res.stderr or start_res.stdout).strip()}")
        capture_active = True

        # 2. Release Dining for measured 10s solo interval, then launch Cash for up to 30s pair interval.
        release_file.write_text("go\n", encoding="utf-8")
        dining_ready_at = None
        pair_ready_at = None
        while time.monotonic() < deadline_mono:
            now_m = time.monotonic()
            if dining_ready_at is None:
                if sd.first_event_time(directory / "dining_area.bridge.log", "counters_ready") is not None:
                    dining_ready_at = now_m
                    print(f"Dining counters_ready at +{now_m - first_wake_mono:.2f}s.", flush=True)
            elif "cash_register" not in procs and (now_m - dining_ready_at) >= solo_s:
                launch_cam("cash_register", [])
                print(f"Started Cash Register at +{now_m - first_wake_mono:.2f}s.", flush=True)
            elif "cash_register" in procs and pair_ready_at is None:
                if sd.first_event_time(directory / "cash_register.bridge.log", "counters_ready") is not None:
                    pair_ready_at = now_m
                    print(f"Cash counters_ready at +{now_m - first_wake_mono:.2f}s.", flush=True)
            elif pair_ready_at is not None:
                pair_elapsed = now_m - pair_ready_at
                if pair_elapsed >= pair_s:
                    break
                # Stop early once a reproducible concurrent socket loss window (>=8s overlap + >=3 gaps) is captured.
                if pair_elapsed >= 8.0:
                    d_log = (directory / "dining_area.bridge.log").read_text(encoding="utf-8", errors="replace")
                    c_log = (directory / "cash_register.bridge.log").read_text(encoding="utf-8", errors="replace")
                    if ("seq_gaps=" in d_log and not d_log.strip().endswith("seq_gaps=0")) and any(
                        int(m.group(1)) >= 3 for txt in (d_log, c_log)
                        for m in re.finditer(r"seq_gaps=(\d+)", txt)
                    ):
                        # Only count gaps AFTER Cash came up if Dining was 0 before!
                        lines_after_pair = c_log.splitlines()[-6:]
                        if any(int(m.group(1)) >= 3 for ln in lines_after_pair for m in re.finditer(r"seq_gaps=(\d+)", ln)):
                            print(f"Stopping early after reproducible concurrent loss at +{now_m - first_wake_mono:.2f}s.", flush=True)
                            break
            time.sleep(poll_interval)
    finally:
        _graceful_stop(procs)
        for fh in open_files:
            fh.close()
        release_file.unlink(missing_ok=True)
        lock_path.unlink(missing_ok=True)
        if capture_active:
            cnt_res = tool_fn(["pktmon", "counters", "--json", "--drop-reason"])
            counters_path.write_text(cnt_res.stdout or "[]", encoding="utf-8")
            tool_fn(["pktmon", "stop"])
        if filters_installed:
            tool_fn(["pktmon", "filter", "remove"])
        shutil.rmtree(directory / "auth", ignore_errors=True)

    # Convert ETL -> PCAPNG + drop-only PCAPNG + ETL stats, then run offline correlator.
    if etl_path.is_file():
        st_res = tool_fn(["pktmon", "etl2txt", str(etl_path), "--stats"])
        etl_stats_path.write_text((st_res.stdout or "") + (st_res.stderr or ""), encoding="utf-8")
        tool_fn(["pktmon", "etl2pcap", str(etl_path), "--out", str(pcap_path)])
        tool_fn(["pktmon", "etl2pcap", str(etl_path), "--drop-only", "--out", str(drop_pcap_path)])

    corr_cmd = bridge_cmd + [
        "devices", "correlate-capture",
        "--pcapng", str(pcap_path),
        "--counters-json", str(counters_path),
        "--etl-stats", str(etl_stats_path),
        "--filter-list", str(filters_path),
        "--comp-list", str(directory / "pktmon_comp_list.json"),
        "--adapter-ip", wifi["ip"],
        "--first-wake-utc", first_wake_utc,
        "--camera", f"dining_area={directory / 'dining_area.bridge.log.rx.csv.endpoints.json'},{directory / 'dining_area.bridge.log.rx.csv.flows.csv'}",
    ]
    if drop_pcap_path.is_file():
        corr_cmd.extend(["--drop-pcapng", str(drop_pcap_path)])
    if capture_start_utc:
        corr_cmd.extend(["--capture-start-utc", capture_start_utc])
    cash_ep = directory / "cash_register.bridge.log.rx.csv.endpoints.json"
    cash_fl = directory / "cash_register.bridge.log.rx.csv.flows.csv"
    if cash_ep.is_file() and cash_fl.is_file():
        corr_cmd.extend(["--camera", f"cash_register={cash_ep},{cash_fl}"])

    corr_res = tool_fn(corr_cmd, timeout=30)
    if corr_res.returncode != 0:
        raise RuntimeError(f"correlate-capture failed: {(corr_res.stderr or corr_res.stdout).strip()}")
    summary = json.loads(corr_res.stdout)
    sd.save_json(directory / "summary.json", summary)
    return summary


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--solo-s", type=float, default=10.0)
    parser.add_argument("--pair-s", type=float, default=30.0)
    parser.add_argument("--wall-s", type=float, default=60.0)
    parser.add_argument("--out-dir", type=Path, default=None)
    args = parser.parse_args()

    sd = load_store_dashboard()
    stamp = time.strftime("%Y%m%d-%H%M%S")
    directory = args.out_dir or (sd.RUNTIME / f"pktmon-compare-{stamp}")
    directory.mkdir(parents=True, exist_ok=True)
    env = dict(os.environ)
    try:
        devices = sd.authenticated_inventory(directory, env)
        summary = run_pktmon_compare(
            directory, devices, env,
            solo_s=args.solo_s, pair_s=args.pair_s, wall_s=args.wall_s,
        )
        print(f"PktMon compare complete: {directory.name}; boundary={summary.get('firstLossBoundary')}", flush=True)
    finally:
        shutil.rmtree(directory / "auth", ignore_errors=True)


if __name__ == "__main__":
    main()
