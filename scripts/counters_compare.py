"""Bounded RTP-to-counters controlled comparison using our bridge signaling and feedback."""

import argparse
import importlib.util
import json
import math
import os
from pathlib import Path
import shutil
import subprocess
import sys
import time

ROOT = Path(__file__).resolve().parent.parent
DASHBOARD_MOD_PATH = Path(__file__).resolve().with_name("store-dashboard.py")


def load_store_dashboard():
    spec = importlib.util.spec_from_file_location("store_dashboard_mod", DASHBOARD_MOD_PATH)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def _parse_fields(line):
    parts = line.split()
    if len(parts) < 3:
        return None, None, {}
    dt = _parse_utc(f"{parts[0]} {parts[1]}")
    ts = dt.timestamp() if dt is not None else None
    fields = dict(item.split("=", 1) for item in parts[2:] if "=" in item)
    return ts, parts[0] + "T" + parts[1], fields


_parse_utc_fn = None


def _parse_utc(text):
    global _parse_utc_fn
    if _parse_utc_fn is None:
        _parse_utc_fn = load_store_dashboard().parse_utc_timestamp
    return _parse_utc_fn(text)


def _to_int(val, default=0):
    try:
        return int(val)
    except (TypeError, ValueError):
        return default


def _to_float(val, default=0.0):
    try:
        return float(val)
    except (TypeError, ValueError):
        return default


def _quantile(vals, q):
    if not vals:
        return None
    s = sorted(vals)
    idx = min(len(s) - 1, max(0, int((len(s) - 1) * q)))
    return round(s[idx], 2)


def parse_bridge_telemetry(log_path):
    """Extract session startup, loss, error, latency, and active interval telemetry from a health log."""
    result = {
        "attempts": 0,
        "reconnects": 0,
        "wakes": 0,
        "answers": 0,
        "channels": 0,
        "start_acks": 0,
        "peer_outs": 0,
        "startup_timeouts": 0,
        "rtp_stalls": 0,
        "output_stalls": 0,
        "ice_fails": 0,
        "first_ready_utc": None,
        "first_gap_utc": None,
        "ice_pairs": [],
        "dims": [],
        "total_packets": 0,
        "total_frames": 0,
        "max_post_kbps": 0.0,
        "seq_gaps_sum": 0,
        "seq_missing_max": 0,
        "seq_missing_end_sum": 0,
        "seq_late_sum": 0,
        "seq_dup_sum": 0,
        "asm_drops_sum": 0,
        "asm_late_sum": 0,
        "rx_udp_sum": 0,
        "rx_rtp_sum": 0,
        "rx_gaps_sum": 0,
        "rx_missing_max": 0,
        "rx_skipped_sum": 0,
        "rx_late_sum": 0,
        "rx_dup_sum": 0,
        "rx_max_vgap_ms": 0,
        "rx_max_iat_ms": 0,
        "max_rtp_gap_ms": 0,
        "rx_errs_sum": 0,
        "rx_timeouts_sum": 0,
        "rx_closed_sum": 0,
        "rtp_errs_sum": 0,
        "rx_last_errs": [],
        "rd_lag_ms": [],
        "wr_ms": [],
        "pop_ms": [],
        "push_ms": [],
        "busy_ms": [],
        "wr_slow_max": 0,
        "active_ranges": [],
        "events_timeline": [],
    }
    if not log_path.is_file():
        return result

    cur = {}
    ever_live = False
    ready_ts = None
    last_ts = None

    def flush_attempt(end_ts=None):
        nonlocal cur, ready_ts
        if not cur:
            return
        result["total_packets"] += cur.get("packets", 0)
        result["total_frames"] += cur.get("frames", 0)
        result["seq_gaps_sum"] += cur.get("seq_gaps", 0)
        result["seq_missing_end_sum"] += cur.get("seq_missing", 0)
        result["seq_late_sum"] += cur.get("seq_late", 0)
        result["seq_dup_sum"] += cur.get("seq_dup", 0)
        result["asm_drops_sum"] += cur.get("asm_drops", 0)
        result["asm_late_sum"] += cur.get("asm_late", 0)
        result["rx_udp_sum"] += cur.get("rx_udp", 0)
        result["rx_rtp_sum"] += cur.get("rx_rtp", 0)
        result["rx_gaps_sum"] += cur.get("rx_gaps", 0)
        result["rx_skipped_sum"] += cur.get("rx_skipped", 0)
        result["rx_late_sum"] += cur.get("rx_late", 0)
        result["rx_dup_sum"] += cur.get("rx_dup", 0)
        result["rx_errs_sum"] += cur.get("rx_errs", 0)
        result["rx_timeouts_sum"] += cur.get("rx_timeouts", 0)
        result["rx_closed_sum"] += cur.get("rx_closed", 0)
        result["rtp_errs_sum"] += cur.get("rtp_errs", 0)
        if ready_ts is not None and (end_ts or last_ts) is not None and (end_ts or last_ts) > ready_ts:
            result["active_ranges"].append((ready_ts, end_ts or last_ts))
        cur = {}
        ready_ts = None

    for line in log_path.read_text(encoding="utf-8", errors="replace").splitlines():
        ts, iso_tag, fields = _parse_fields(line)
        if not fields:
            continue
        if ts is not None:
            last_ts = ts
        event = fields.get("event")
        if event == "attempt":
            flush_attempt(ts)
            result["attempts"] += 1
            if ever_live:
                result["reconnects"] += 1
            cur = {"packets": 0, "frames": 0}
        elif event == "wake" and _to_int(fields.get("wake_code"), -1) == 0:
            result["wakes"] += 1
        elif event == "answer_received":
            result["answers"] += 1
        elif event == "channel_open":
            result["channels"] += 1
        elif event == "start_live_ack":
            result["start_acks"] += 1
        elif event == "peer_out":
            result["peer_outs"] += 1
            result["events_timeline"].append({"ts": ts, "utc": iso_tag, "event": event})
        elif event == "startup_timeout":
            result["startup_timeouts"] += 1
            result["events_timeline"].append({"ts": ts, "utc": iso_tag, "event": event})
        elif event == "rtp_stall":
            result["rtp_stalls"] += 1
            result["events_timeline"].append({"ts": ts, "utc": iso_tag, "event": event})
        elif event == "output_stall":
            result["output_stalls"] += 1
            result["events_timeline"].append({"ts": ts, "utc": iso_tag, "event": event})
        elif event == "ice_failed":
            result["ice_fails"] += 1
            result["events_timeline"].append({"ts": ts, "utc": iso_tag, "event": event})
        elif event in ("counters_ready", "first_frame"):
            ever_live = True
            ready_ts = ts
            if result["first_ready_utc"] is None:
                result["first_ready_utc"] = fields.get("ts_utc") or iso_tag
            ice = fields.get("ice")
            if ice and ice != "/" and ice not in result["ice_pairs"]:
                result["ice_pairs"].append(ice)
            dim = fields.get("dim")
            if dim and dim != "-" and dim not in result["dims"]:
                result["dims"].append(dim)
            result["events_timeline"].append({"ts": ts, "utc": fields.get("ts_utc") or iso_tag, "event": event})
        elif event in ("progress", "session_end"):
            for k in ("packets", "frames", "seq_gaps", "seq_missing", "seq_late", "seq_dup",
                      "asm_drops", "asm_late", "rx_udp", "rx_rtp", "rx_gaps", "rx_missing",
                      "rx_skipped", "rx_late", "rx_dup", "rx_errs", "rx_timeouts", "rx_closed", "rtp_errs"):
                if k in fields:
                    cur[k] = _to_int(fields[k], cur.get(k, 0))
            result["seq_missing_max"] = max(result["seq_missing_max"], cur.get("seq_missing", 0))
            result["rx_missing_max"] = max(result["rx_missing_max"], cur.get("rx_missing", 0))
            if "rx_max_vgap_ms" in fields:
                result["rx_max_vgap_ms"] = max(result["rx_max_vgap_ms"], _to_int(fields["rx_max_vgap_ms"]))
            if "rx_max_iat_ms" in fields:
                result["rx_max_iat_ms"] = max(result["rx_max_iat_ms"], _to_int(fields["rx_max_iat_ms"]))
            if "max_rtp_gap_ms" in fields:
                result["max_rtp_gap_ms"] = max(result["max_rtp_gap_ms"], _to_int(fields["max_rtp_gap_ms"]))
            if "post_kbps" in fields:
                result["max_post_kbps"] = max(result["max_post_kbps"], _to_float(fields["post_kbps"]))
            err_tag = fields.get("rx_last_err")
            if err_tag and err_tag != "-" and err_tag not in result["rx_last_errs"]:
                result["rx_last_errs"].append(err_tag)
            if (cur.get("seq_gaps", 0) > 0 or cur.get("rx_gaps", 0) > 0) and result["first_gap_utc"] is None:
                result["first_gap_utc"] = iso_tag
                result["events_timeline"].append({"ts": ts, "utc": iso_tag, "event": "first_gap"})
            for field, dest in (("rd_lag_ms", "rd_lag_ms"), ("wr_ms", "wr_ms"),
                                ("pop_ms", "pop_ms"), ("push_ms", "push_ms"), ("busy_ms", "busy_ms")):
                if field in fields:
                    result[dest].append(_to_float(fields[field]))
            if "wr_slow" in fields:
                result["wr_slow_max"] = max(result["wr_slow_max"], _to_int(fields["wr_slow"]))
            if event == "session_end":
                dim = fields.get("dim")
                if dim and dim != "-" and dim not in result["dims"]:
                    result["dims"].append(dim)
                flush_attempt(ts)

    flush_attempt(last_ts)

    summary = {k: v for k, v in result.items()
               if k not in ("rd_lag_ms", "wr_ms", "pop_ms", "push_ms", "busy_ms", "active_ranges", "events_timeline")}
    summary["latencies_ms"] = {
        name: {"p50": _quantile(result[name], 0.50), "p95": _quantile(result[name], 0.95),
               "max": round(max(result[name]), 2) if result[name] else None, "n": len(result[name])}
        for name in ("rd_lag_ms", "wr_ms", "pop_ms", "push_ms", "busy_ms")
    }
    summary["active_ranges"] = result["active_ranges"]
    summary["events_timeline"] = result["events_timeline"]
    return summary


def summarize_directory(sd, directory):
    """Aggregate per-camera telemetry and 3-way overlap for any bridge log directory."""
    per_cam = {}
    cov = {}
    timeline = []
    for alias in sd.CAMERAS:
        cam_sum = parse_bridge_telemetry(directory / f"{alias}.bridge.log")
        cov[alias] = cam_sum.pop("active_ranges")
        for ev in cam_sum.pop("events_timeline"):
            if ev.get("ts") is not None:
                timeline.append({"camera": alias, **ev})
        cam_sum["active_s"] = round(sum(e - s for s, e in sd.union(cov[alias], join=0.5)), 3)
        per_cam[alias] = cam_sum
    timeline.sort(key=lambda row: row["ts"])
    merged_cov = {alias: sd.union(ranges, join=0.5) for alias, ranges in cov.items()}
    common_spans = []
    aliases = list(sd.CAMERAS)
    cur = merged_cov[aliases[0]]
    for alias in aliases[1:]:
        cur = sd.intersect(cur, merged_cov[alias])
    common_spans = cur
    return {
        "cameras": per_cam,
        "common_concurrent_s": round(max((e - s for s, e in common_spans), default=0.0), 3),
        "common_concurrent_total_s": round(sum(e - s for s, e in common_spans), 3),
        "timeline": timeline,
    }


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


def run_counters_compare(directory, devices, env, wall_s=85.0, startup_timeout_s=35,
                         bridge_cmd=None, udp_fn=None, baseline_dir=None, poll_interval=0.25):
    """Run bounded 3-camera --counters-only diagnostic and summarize stages cleanly."""
    sd = load_store_dashboard()
    bridge_cmd = list(bridge_cmd) if bridge_cmd is not None else [str(sd.BRIDGE)]
    udp_fn = udp_fn or sd.udp_counters
    directory.mkdir(parents=True, exist_ok=True)
    lock_path = directory / "start.lock"
    lock_path.unlink(missing_ok=True)

    run_env = dict(env, VICO_RX_TRACE="1")
    run_env.setdefault("VICOHOME_CACHE_DIR", str(directory / "auth"))
    run_env.pop("VICOHOME_DEBUG", None)

    inv_path = directory / "inventory.json"
    start_battery = {}
    if inv_path.is_file():
        by_name = {row["deviceName"]: row for row in json.loads(inv_path.read_text(encoding="utf-8"))}
        for alias, name in sd.CAMERAS.items():
            row = by_name.get(name, {})
            start_battery[alias] = {"battery": row.get("batteryLevel"), "charging": row.get("isCharging")}

    udp_before = udp_fn()
    startup_order = ["dining_area", "cash_register", "kitchen_back"]
    procs = {}
    ready_at = {}
    started_at = {}
    open_files = []
    started = time.monotonic()
    stop_at = started + wall_s
    cutoff = "wall_limit"
    creation_flags = subprocess.CREATE_NO_WINDOW | getattr(subprocess, "CREATE_NEW_PROCESS_GROUP", 0)

    def launch(alias):
        elapsed = time.monotonic() - started
        rem = max(10, int(math.ceil(stop_at - time.monotonic())))
        serial = devices[alias]
        cmd = bridge_cmd + [
            "devices", "media-probe", serial,
            "--continuous", "--counters-only",
            "--timeout", f"{startup_timeout_s}s",
            "--retry-limit", "0",
            "--progress-every", "250ms",
            "--health-log", f"{alias}.bridge.log",
            "--startup-lock-file", "start.lock",
            "--max-duration", f"{rem}s",
        ]
        log_fh = (directory / f"{alias}.proc.log").open("wb")
        open_files.append(log_fh)
        procs[alias] = subprocess.Popen(
            cmd, cwd=directory, env=run_env, stdout=log_fh, stderr=subprocess.STDOUT,
            creationflags=creation_flags, stdin=subprocess.DEVNULL,
        )
        started_at[alias] = elapsed
        print(f"Started counters-only {sd.CAMERAS[alias]} at +{elapsed:.2f}s (max {rem}s).", flush=True)

    try:
        launch(startup_order[0])
        while True:
            now_mono = time.monotonic()
            elapsed = now_mono - started
            if now_mono >= stop_at:
                break
            if len(procs) == len(startup_order) and all(p.poll() is not None for p in procs.values()):
                cutoff = "processes_exited"
                break

            for idx in range(len(startup_order) - 1):
                cur_alias = startup_order[idx]
                next_alias = startup_order[idx + 1]
                if cur_alias in procs and next_alias not in procs:
                    if cur_alias not in ready_at:
                        t_ready = sd.first_event_time(directory / f"{cur_alias}.bridge.log", "counters_ready")
                        if t_ready is not None:
                            ready_at[cur_alias] = time.monotonic()
                            print(f"{sd.CAMERAS[cur_alias]} counters_ready at +{elapsed:.2f}s.", flush=True)
                    fallback_elapsed = 20.0 if idx == 0 else 40.0
                    if ((cur_alias in ready_at and (now_mono - ready_at[cur_alias]) >= 2.0)
                            or elapsed >= fallback_elapsed):
                        launch(next_alias)
            time.sleep(poll_interval)

        # Wait up to 8s for clean self-exit from --max-duration before signaling cleanly.
        grace_deadline = time.monotonic() + 8.0
        for proc in procs.values():
            rem = max(0.1, grace_deadline - time.monotonic())
            if proc.poll() is None:
                try:
                    proc.wait(timeout=rem)
                except subprocess.TimeoutExpired:
                    pass
        _graceful_stop(procs)
    finally:
        for fh in open_files:
            fh.close()
        lock_path.unlink(missing_ok=True)
        end_battery = {}
        try:
            end_res = sd.command(bridge_cmd + ["devices", "list", "--format", "json"],
                                 directory / "inventory_end.log", 20, run_env)
            if end_res.returncode == 0:
                by_name = {row["deviceName"]: row for row in json.loads(end_res.stdout)}
                for alias, name in sd.CAMERAS.items():
                    row = by_name.get(name, {})
                    end_battery[alias] = {"battery": row.get("batteryLevel"), "charging": row.get("isCharging")}
        except Exception:
            pass
        shutil.rmtree(directory / "auth", ignore_errors=True)

    udp_after = udp_fn()
    udp_delta = None
    if udp_before and udp_after:
        udp_delta = {k: udp_after.get(k, 0) - udp_before.get(k, 0) for k in udp_before}

    agg = summarize_directory(sd, directory)
    for alias, proc in procs.items():
        agg["cameras"][alias]["exit_code"] = proc.returncode
        agg["cameras"][alias]["started_offset_s"] = round(started_at.get(alias, 0.0), 3)

    report = {
        "mode": "counters_only",
        "wall_s": round(time.monotonic() - started, 2),
        "cutoff": cutoff,
        "battery_context": {"start": start_battery, "end": end_battery},
        "udp_before": udp_before,
        "udp_after": udp_after,
        "udp_delta": udp_delta,
        **agg,
    }
    if baseline_dir is not None and baseline_dir.is_dir():
        base = summarize_directory(sd, baseline_dir)
        report["baseline_dir"] = baseline_dir.name
        report["comparison"] = {
            alias: {
                "counters_only": {
                    k: agg["cameras"][alias][k]
                    for k in ("total_packets", "seq_gaps_sum", "seq_missing_max", "rx_gaps_sum",
                              "rx_missing_max", "rx_max_vgap_ms", "rx_errs_sum", "rx_timeouts_sum",
                              "rtp_errs_sum", "asm_drops_sum", "reconnects", "active_s")
                },
                "full_baseline": {
                    k: base["cameras"][alias][k]
                    for k in ("total_packets", "total_frames", "seq_gaps_sum", "seq_missing_max",
                              "rx_gaps_sum", "rx_missing_max", "rx_max_vgap_ms", "rx_errs_sum",
                              "rx_timeouts_sum", "rtp_errs_sum", "asm_drops_sum", "reconnects", "active_s")
                },
            }
            for alias in sd.CAMERAS
        }
    sd.save_json(directory / "summary.json", report)
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--wall-s", type=float, default=85.0)
    parser.add_argument("--startup-timeout-s", type=int, default=35)
    parser.add_argument("--baseline-dir", type=Path, default=None)
    parser.add_argument("--out-dir", type=Path, default=None)
    args = parser.parse_args()

    sd = load_store_dashboard()
    stamp = time.strftime("%Y%m%d-%H%M%S")
    directory = args.out_dir or (sd.RUNTIME / f"counters-compare-{stamp}")
    directory.mkdir(parents=True, exist_ok=True)
    env = dict(os.environ)
    try:
        devices = sd.authenticated_inventory(directory, env)
        report = run_counters_compare(
            directory, devices, env,
            wall_s=args.wall_s,
            startup_timeout_s=args.startup_timeout_s,
            baseline_dir=args.baseline_dir,
        )
        print(f"Counters compare completed: {directory.name}; common_concurrent_s={report['common_concurrent_s']}", flush=True)
    finally:
        shutil.rmtree(directory / "auth", ignore_errors=True)


if __name__ == "__main__":
    main()
