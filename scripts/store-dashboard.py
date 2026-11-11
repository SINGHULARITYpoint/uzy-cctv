"""Bounded Windows store NVR test. Private artifacts are written under ignored local/."""

import argparse
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import re
import shutil
import socket
import subprocess
import threading
import time
from urllib.error import HTTPError, URLError
from urllib.parse import urlencode
from urllib.request import Request, urlopen


ROOT = Path(__file__).resolve().parent.parent
LOCAL = ROOT / "local"
RUNTIME = LOCAL / "store-dashboard"
BRIDGE = ROOT / "services/vico-bridge/bin/vico-cli.exe"
CAMERAS = {"dining_area": "Dining Area", "cash_register": "Cash Register", "kitchen_back": "Kitchen Back"}
TARGET = [2304, 1296]
FLAGS = subprocess.CREATE_NO_WINDOW
HTTP, RTSP, PLAYBACK, API = 11984, 19554, 19996, 19997
DISK_LIMIT = 128 * 1024**2
WALL_LIMIT = 90
MAX_GAP = 2


def save_json(path, data):
    temp = path.with_suffix(".tmp")
    temp.write_text(json.dumps(data, indent=2) + "\n", encoding="utf-8")
    # A Windows static-file reader can hold the destination open during a refresh.
    for attempt in range(8):
        try:
            temp.replace(path)
            return
        except PermissionError:
            if attempt == 7:
                raise
            time.sleep(.03)


def command(args, log, timeout, env=None, output=subprocess.PIPE):
    with log.open("w", encoding="utf-8") as errors:
        return subprocess.run(args, cwd=ROOT, env=env, stdout=output, stderr=errors,
                              encoding="utf-8", errors="replace", timeout=timeout, creationflags=FLAGS)


def authenticated_inventory(directory, env):
    env["VICOHOME_CACHE_DIR"] = str(directory / "auth")
    env.pop("VICOHOME_DEBUG", None)
    result = command([BRIDGE, "devices", "list", "--format", "json"], directory / "inventory.log", 20, env)
    if result.returncode:
        raise RuntimeError("Dedicated account listing failed; see private inventory log.")
    devices = json.loads(result.stdout)
    expected = json.loads((LOCAL / "private/store-device-list.json").read_text(encoding="utf-8-sig"))
    known = {row["deviceName"]: row["serialNumber"] for row in expected}
    current = {row["deviceName"]: row for row in devices}
    save_json(directory / "inventory.json", devices)
    if len(devices) != len(CAMERAS) or set(current) != set(CAMERAS.values()) or any(
        row["serialNumber"] != known.get(name) or not re.fullmatch(r"[A-Za-z0-9_-]+", row["serialNumber"])
        for name, row in current.items()
    ):
        print(f"Account returned {len(devices)} camera(s), "
              f"{sum(name in current for name in CAMERAS.values())} expected labels and "
              f"{sum(row['serialNumber'] in known.values() for row in devices)} known device identities.", flush=True)
        raise RuntimeError("Account listing did not match all three verified cameras; no camera sessions started.")
    for name, row in current.items():
        print(f"{name}: battery {row.get('batteryLevel', '?')}%, charging flag {row.get('isCharging', '?')}", flush=True)
    # The parent login seeds a common cache. Children may not trigger a competing login.
    if not (directory / "auth/auth.json").is_file():
        raise RuntimeError("Could not seed the private dedicated-account cache.")
    env.pop("VICOHOME_EMAIL", None)
    env.pop("VICOHOME_PASSWORD", None)
    return {alias: current[name]["serialNumber"] for alias, name in CAMERAS.items()}


def inspect_video(path, decode=True):
    info = command(["ffprobe", "-v", "error", "-select_streams", "v:0",
                    "-show_entries", "stream=width,height:packet=pts_time,duration_time,flags",
                    "-of", "json", path], path.with_suffix(".probe.log"), 20)
    data = json.loads(info.stdout or "{}")
    stream = next(iter(data.get("streams", [])), {})
    pts = sorted((float(p["pts_time"]), float(p.get("duration_time", "0").replace("N/A", "0")),
                  "K" in p.get("flags", "")) for p in data.get("packets", []) if "pts_time" in p)
    dimensions = None
    frame_count = 0
    exit_code = None
    errors = 0
    if decode and stream:
        log = path.with_suffix(".decode.log")
        # Decode the original frames. Give the disposable output its own clock so
        # muxer timing errors cannot be counted as decoder errors.
        check = command(["ffmpeg", "-nostdin", "-v", "level+info", "-xerror", "-err_detect", "explode", "-i", path,
                         "-map", "0:v:0", "-vf", "showinfo,settb=1/90000,setpts=N", "-enc_time_base", "1:90000",
                         "-fps_mode", "passthrough", "-f", "null", "-"],
                        log, 40)
        report = log.read_text(encoding="utf-8", errors="replace")
        shapes = re.findall(r"\bn:\s*\d+\s+pts:.*?\bs:(\d+)x(\d+)", report)
        dimensions = sorted({tuple(map(int, pair)) for pair in shapes})
        frame_count = len(shapes)
        exit_code = check.returncode
        errors = sum("[error]" in line or "[fatal]" in line for line in report.splitlines())
    return {"pts": pts, "resolution": [stream["width"], stream["height"]] if stream else None,
            "probe_exit": info.returncode, "decode_exit": exit_code, "decode_errors": errors,
            "decoded_dimensions": dimensions, "frames": frame_count,
            "duplicate_pts": sum(a[0] == b[0] for a, b in zip(pts, pts[1:]))}


def preflight(directory, devices, env):
    results = {}
    for alias, serial in devices.items():
        video = directory / f"{alias}.h264"
        result = command([BRIDGE, "devices", "media-probe", serial, "--timeout", "24s",
                          "--observe-for", "12s", "--output", video],
                         directory / f"{alias}.smoke.log", 40, env)
        details = json.loads(result.stdout) if result.returncode == 0 else {}
        if details:
            save_json(directory / f"{alias}.smoke.json", details)
        check = inspect_video(video) if video.exists() and video.stat().st_size else {}
        check.pop("pts", None)
        row = {**check, "offers": details.get("offerAttempts"), "drops": details.get("droppedVideoPackets"),
               "first_rtp_ms": details.get("firstVideoMillis"), "rtp_gap_ms": details.get("largestVideoGapMillis"),
               "result": details.get("endReason"), "exit": result.returncode}
        # A standalone CG6 probe contains the initial preview and the main stream.
        row["pass"] = (tuple(TARGET) in (row.get("decoded_dimensions") or []) and row.get("decode_exit") == 0
                       and row.get("decode_errors") == 0 and row.get("frames", 0) > 0)
        results[alias] = row
        save_json(directory / "summary.json", results)
        print(f"{CAMERAS[alias]} smoke: {json.dumps(row)}", flush=True)
        time.sleep(2)
    return all(row["pass"] for row in results.values())


def request(port, path, data=None):
    req = Request(f"http://127.0.0.1:{port}{path}", method="PATCH" if data is not None else "GET",
                  data=json.dumps(data).encode() if data is not None else None,
                  headers={"Content-Type": "application/json"} if data is not None else {})
    with urlopen(req, timeout=0.65) as response:
        return response.read()


def check_ports(ports):
    for port in ports:
        with socket.socket() as probe:
            probe.setsockopt(socket.SOL_SOCKET, socket.SO_EXCLUSIVEADDRUSE, 1)
            probe.bind(("127.0.0.1", port))


def wait_http(port, path, process):
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline and process.poll() is None:
        try:
            return request(port, path)
        except (URLError, TimeoutError):
            time.sleep(.1)
    raise RuntimeError(f"Service on local port {port} did not start.")


def start_service(directory, name, args, env=None):
    with (directory / f"{name}.log").open("wb") as log:
        return subprocess.Popen(args, cwd=directory, env=env, stdout=log, stderr=subprocess.STDOUT,
                                creationflags=FLAGS, stdin=subprocess.DEVNULL)


def stop_service(process, tree=False):
    if process is not None and process.poll() is None:
        if tree:
            subprocess.run(["taskkill.exe", "/PID", str(process.pid), "/T", "/F"],
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, creationflags=FLAGS, timeout=8)
        if process.poll() is None:
            process.kill()
        process.wait(timeout=8)


def prepare(directory, devices=None, sources=None, stagger=False, hold=None, dining_receiver="bridge", continuous_timeout=20, startup_lock=False, read_timeout_s=90, stream_profile="2k"):
    assets = LOCAL / "home-dashboard"
    go2rtc = assets / "go2rtc.exe"
    mediamtx = assets / "mediamtx/mediamtx.exe"
    web = directory / "www"
    web.mkdir(exist_ok=True)
    for alias in CAMERAS:
        (directory / "recordings" / alias).mkdir(parents=True, exist_ok=True)
    for file in ("index.html", "dashboard.css", "profile.js", "viewer.js", "archive.js"):
        shutil.copyfile(ROOT / "services/home-dashboard" / file, web / file)
    index = web / "index.html"
    index.write_text(index.read_text(encoding="utf-8").replace('data-profile="home"', 'data-profile="store"'),
                     encoding="utf-8")
    shutil.copyfile(assets / "www/video-rtc.js", web / "video-rtc.js")
    if not (web / "coverage.json").exists():
        save_json(web / "coverage.json", {})
    if not (web / "status.json").exists():
        save_json(web / "status.json", {"mode": "live" if devices else "review", "streamProfile": stream_profile, "cameras": {}})
    # Relative paths keep Windows paths with spaces out of the go2rtc exec command.
    rel_bridge = os.path.relpath(BRIDGE, directory).replace("\\", "/")
    rel_vicostream = os.path.relpath(ROOT / "local/vicostream/vicostream.exe", directory).replace("\\", "/")
    if devices is not None:
        force_direct = os.environ.get("VICO_ICE_FORCE_DIRECT") == "1"
        # A held camera waits for a release file before startLive; its startup budget grows by the hold.
        sources = {}
        for alias, serial in devices.items():
            if alias == "dining_area" and dining_receiver == "vicostream":
                sources[alias] = (f"exec:{rel_vicostream} -sn {serial} "
                                  f"-health-log {alias}.bridge.log -resolution 2304x1296 -timeout 90s")
            else:
                timeout_s = 40 if hold and alias in hold else continuous_timeout
                sources[alias] = (f"exec:{rel_bridge} devices media-probe {serial} --continuous "
                                  f"--timeout {timeout_s}s "
                                  f"--progress-every 250ms --output - --health-log {alias}.bridge.log"
                                  + (f" --stream-profile {stream_profile}" if stream_profile != "2k" else "")
                                  + (" --startup-lock-file start.lock" if startup_lock else "")
                                  + (f" --hold-start-live-file {hold[alias]}" if hold and alias in hold else "")
                                  + (" --force-direct" if force_direct else ""))
    if sources is None:
        sources = {}
    streaming = bool(sources)
    go_config = {"api": {"listen": f"127.0.0.1:{HTTP}", "static_dir": "www",
                         "allow_paths": ["/", "/api/ws", "/api/streams"]},
                 "rtsp": {"listen": f"127.0.0.1:{RTSP}" if streaming else ""},
                 "webrtc": {"listen": "", "ice_servers": []}, "streams": sources}
    if streaming:
        go_config["exec"] = {"allow_paths": [source.split()[0][5:] for source in sources.values()
                                                if source.startswith("exec:")]}
    save_json(directory / ("go2rtc.json" if streaming else "go2rtc-review.json"), go_config)

    recorder = {"readTimeout": f"{read_timeout_s}s", "rtsp": False, "rtmp": False, "hls": False, "webrtc": False,
                "srt": False, "moq": False, "api": streaming, "apiAddress": f"127.0.0.1:{API}",
                "playback": True, "playbackAddress": f"127.0.0.1:{PLAYBACK}",
                "playbackAllowOrigins": [f"http://127.0.0.1:{HTTP}"],
                "authInternalUsers": [{"user": "any", "ips": ["127.0.0.1"], "permissions":
                                       [{"action": "playback", "path": alias} for alias in CAMERAS]
                                       + ([{"action": "api"}] if streaming else [])}],
                "pathDefaults": {"recordPath": "recordings/%path/%Y-%m-%d_%H-%M-%S-%f_%z",
                                 "recordFormat": "fmp4", "recordPartDuration": "1s",
                                 "recordMaxPartSize": "4M", "recordSegmentDuration": "8s",
                                 "recordDeleteAfter": "0s"},
                "paths": {alias: {"source": f"rtsp://127.0.0.1:{RTSP}/{alias}", "rtspTransport": "tcp",
                                  "record": True} if streaming and not stagger else {} for alias in CAMERAS}}
    save_json(directory / ("mediamtx.json" if streaming else "mediamtx-review.json"), recorder)
    return go2rtc, mediamtx


def files(directory, alias=None):
    base = directory / "recordings"
    return sorted((base / alias).glob("*.mp4")) if alias else sorted(base.glob("*/*.mp4"))


def snapshot(directory):
    return {f: (f.stat().st_size, f.stat().st_mtime_ns) for f in files(directory)}


def max_step(values):
    return max((b - a for a, b in zip(values, values[1:])), default=0)


def union(ranges, join=0):
    out = []
    for begin, end in sorted(ranges):
        if out and begin <= out[-1][1] + join:
            out[-1] = (out[-1][0], max(out[-1][1], end))
        elif end > begin:
            out.append((begin, end))
    return out


def intersect(left, right):
    return union((max(a, c), min(b, d)) for a, b in left for c, d in right)


def shared_recording_span(coverage):
    camera_ranges = [coverage.get(alias, {}).get("ranges") or [] for alias in CAMERAS]
    common = camera_ranges[0]
    for ranges in camera_ranges[1:]:
        common = intersect(common, ranges)
    return max((end - begin for begin, end in common), default=0.0)


def segment_start(file):
    return datetime.strptime(file.stem, "%Y-%m-%d_%H-%M-%S-%f_%z").timestamp()


def saved_coverage(directory, cache, decode=False):
    """Use packet timestamps for holes within a MediaMTX segment, not its total duration."""
    result = {}
    coverage = {}
    for alias in CAMERAS:
        segments = []
        ranges = []
        stamps = []
        for file in files(directory, alias):
            try:
                key = (file, file.stat().st_size, decode)
                if key not in cache:
                    cache[key] = inspect_video(file, decode)
                info = cache[key]
                origin = segment_start(file)
            except (OSError, ValueError, subprocess.TimeoutExpired):
                if decode:
                    segments.append({"file": file.name, "probe_exit": -1, "frames": 0})
                continue
            pts = info["pts"]
            stamps.extend(origin + stamp for stamp, _, _ in pts)
            if pts:
                begin, end = pts[0][0], pts[0][0]
                in_coverage = pts[0][2]
                for i, (stamp, duration, keyframe) in enumerate(pts):
                    if i and stamp - pts[i - 1][0] > MAX_GAP:
                        if in_coverage:
                            ranges.append((origin + begin, origin + end))
                        in_coverage = False
                    if not in_coverage and keyframe:
                        begin = stamp
                        in_coverage = True
                    # A long sample duration must not paint over the outage.
                    end = stamp + (duration if 0 < duration <= .25 else 0)
                if in_coverage:
                    ranges.append((origin + begin, origin + end))
            if decode:
                segments.append({k: v for k, v in info.items() if k != "pts"} | {"file": file.name})
        merged = union(ranges, join=.25)
        coverage[alias] = [[round(start * 1000), round(end * 1000)] for start, end in merged]
        result[alias] = {"segments": segments, "ranges": merged, "max_saved_gap_s": max_step(sorted(stamps))}
    save_json(directory / "www/coverage.json", coverage)
    return result


def udp_counters():
    """Windows UDP socket counters (host-wide). Receive Errors rises when datagrams are dropped for lack of buffer."""
    try:
        text = subprocess.run(["netstat.exe", "-s", "-p", "udp"], capture_output=True, text=True, timeout=5).stdout
    except (OSError, subprocess.TimeoutExpired):
        return None
    out = {}
    for key, label in (("received", "Datagrams Received"), ("no_ports", "No Ports"), ("receive_errors", "Receive Errors")):
        m = re.search(rf"{label}\s*=\s*(\d+)", text)
        if m:
            out[key] = int(m.group(1))
    return out or None


def count_frames(health):
    runs = []
    retries = 0
    startup = None
    first_rtp_s = None
    ice_candidates = None
    silent_output_ms = 0
    for line in health.read_text(encoding="utf-8", errors="replace").splitlines() if health.exists() else ():
        fields = dict(re.findall(r"(\w+)=([^\s]+)", line))
        event = fields.get("event")
        if event == "attempt":
            runs.append({})
        elif event == "retry":
            retries += 1
        elif event == "first_frame" and startup is None:
            startup = int(fields["elapsed_ms"]) / 1000
            if "first_rtp_ms" in fields and int(fields["first_rtp_ms"]) >= 0:
                first_rtp_s = int(fields["first_rtp_ms"]) / 1000
            if fields.get("ice") and fields.get("ice") != "/":
                ice_candidates = fields["ice"]
        if runs and event in ("progress", "session_end", "error"):
            runs[-1].update(fields)
            if first_rtp_s is None and int(fields.get("packets", 0)) > 0 and int(fields.get("last_rtp_ms", -1)) >= 0:
                first_rtp_s = int(fields["last_rtp_ms"]) / 1000
            if fields.get("ice") and fields.get("ice") != "/":
                ice_candidates = fields["ice"]
        if event in ("progress", "output_stall") and int(fields.get("last_frame_ms", -1)) >= 0:
            silent_output_ms = max(silent_output_ms, int(fields["last_rtp_ms"]) - int(fields["last_frame_ms"]))
    values = lambda key: [int(row.get(key, 0)) for row in runs]
    return {"first_rtp_s": first_rtp_s, "first_frame_s": startup, "ice_candidates": ice_candidates,
            "assembler_drops": sum(values("drops")),
            "rtp_seq_gaps": sum(values("seq_gaps")),
            "rtp_seq_missing": sum(values("seq_missing")),
            "rtp_seq_late": sum(values("seq_late")),
            "assembly_drops": sum(values("asm_drops")),
            "assembly_late": sum(values("asm_late")),
            "bridge_retries": retries,
            "bridge_gap_s": max(values("max_frame_gap_ms"), default=0) / 1000,
            "rtp_gap_s": max(values("max_rtp_gap_ms"), default=0) / 1000,
            "rtp_after_last_frame_s": silent_output_ms / 1000,
            "rtp_packets": sum(values("packets")), "bridge_frames": sum(values("frames")),
            "answer": any(row.get("answer") == "true" for row in runs),
            "channel": any(row.get("channel") == "true" for row in runs),
            "start_ack": any(row.get("ack") == "true" or row.get("start_ack") == "true" for row in runs),
            "attempts": len(runs)}


def toggle_recording(enabled, active_cameras=CAMERAS):
    for alias in active_cameras:
        request(API, f"/v3/config/paths/patch/{alias}", {"record": enabled})


def reader_metrics(observations, start, end):
    samples = [row for row in observations if start <= row["at"] <= end]
    arrivals = []
    previous = None
    for row in samples:
        current = row["bytes"]
        if current is not None and current > (previous or 0):
            arrivals.append(row["at"])
        if current is not None:
            previous = current
    return {"reader_first_s": arrivals[0] - start if arrivals else None,
            "reader_span_s": arrivals[-1] - arrivals[0] if arrivals else 0,
            "max_reader_gap_s": max(max_step(arrivals), end - arrivals[-1]) if arrivals else None,
            "go2rtc_seen": any(row["go2rtc"] for row in samples)}


def telemetry(directory, observations, clock, active_cameras=CAMERAS):
    # API snapshots include addresses and private exec URLs; persist only advancing counters.
    restream, recorder = {}, {}
    try:
        restream = json.loads(request(HTTP, "/api/streams"))
    except (URLError, TimeoutError):
        pass
    try:
        payload = json.loads(request(API, "/v3/paths/list"))
        recorder = {row["name"]: row for row in payload.get("items") or []}
    except (URLError, TimeoutError):
        pass
    at = clock()
    for alias in active_cameras:
        stream = restream.get(alias) or {}
        producers = [p for p in stream.get("producers") or [] if "bytes_recv" in p]
        consumers = [c for c in stream.get("consumers") or [] if "bytes_send" in c]
        observations[alias].append({"at": at, "bytes": recorder.get(alias, {}).get("bytesReceived"),
                                    "go2rtc": any(p["bytes_recv"] > 0 for p in producers),
                                    "producer_bytes": sum(p["bytes_recv"] for p in producers),
                                    "consumer_bytes": sum(c["bytes_send"] for c in consumers)})


def summary(directory, observations, start, end, gap_start=None, active_cameras=CAMERAS, extra_report=None, camera_windows=None):
    disk = saved_coverage(directory, {}, decode=True)
    common = [(start, end)]
    results = {}
    for alias in active_cameras:
        cam_start, cam_end = (camera_windows or {}).get(alias, (start, end))
        stats = disk[alias]
        all_segments = stats["segments"]
        primary = [seg for seg in all_segments if gap_start is None or
                   segment_start(Path(seg["file"])) < gap_start]
        before_gap = intersect(stats["ranges"], [(cam_start, cam_end)])
        common = intersect(common, intersect(stats["ranges"], [(start, end)]))
        reader = reader_metrics(observations[alias], cam_start, cam_end)
        bridge = count_frames(directory / f"{alias}.bridge.log")
        primary_times = []
        for seg in primary:
            path = directory / "recordings" / alias / seg["file"]
            pts = inspect_video(path, decode=False)["pts"]
            primary_times.extend(
                t for stamp, _, _ in pts
                if cam_start <= (t := segment_start(path) + stamp) <= cam_end
            )
        duration = sum(b - a for a, b in before_gap)
        gap = max_step(sorted(primary_times))
        errors = sum(seg.get("decode_errors", 0) for seg in all_segments)
        decoded = bool(all_segments) and all(seg.get("decode_exit") == 0 and seg.get("probe_exit") == 0 and
                    seg["frames"] > 0 for seg in all_segments) and not errors
        resolution = sorted({tuple(shape) for seg in all_segments for shape in seg.get("decoded_dimensions") or []})
        passed = (decoded and resolution == [tuple(TARGET)] and duration >= 20 and gap <= MAX_GAP
                  and reader["reader_span_s"] >= 20 and reader["max_reader_gap_s"] is not None
                  and reader["max_reader_gap_s"] <= MAX_GAP)
        if passed:
            stage = None
        elif not bridge["answer"] and bridge["attempts"]:
            stage = "SDP (signaling before SDP answer)"
        elif not bridge["channel"] and bridge["attempts"]:
            stage = "ICE/data channel (SDP answered, data channel never opened)"
        elif bridge["rtp_packets"] == 0 and bridge["attempts"]:
            stage = "RTP (before video RTP ingress)"
        elif bridge["bridge_frames"] == 0 and bridge["rtp_packets"] > 0:
            stage = "bridge output (received RTP but emitted no frames)"
        elif bridge["rtp_after_last_frame_s"] > MAX_GAP:
            stage = "bridge output (H.264 output stalled while RTP continued)"
        elif bridge["bridge_gap_s"] > MAX_GAP:
            stage = "RTP" if bridge["rtp_gap_s"] > MAX_GAP else "bridge output"
        elif bridge["bridge_frames"] and not reader["go2rtc_seen"]:
            stage = "go2rtc (between bridge output and go2rtc producer)"
        elif not reader["reader_span_s"] or reader["max_reader_gap_s"] > MAX_GAP:
            stage = "recorder (between go2rtc and MediaMTX reader)"
        else:
            stage = "recorder (saved coverage, resolution, or decode)"
        results[alias] = {"pass": passed, "stage": stage, **bridge, **reader,
                          "saved_duration_s": duration, "max_saved_gap_s": gap if primary_times else None,
                          "decode_pass": decoded, "decode_errors": errors, "resolution": resolution,
                          "decoded_frames": sum(seg["frames"] for seg in all_segments),
                          "segments": all_segments, "ranges": stats["ranges"]}
    overlap = max((end - start for start, end in common), default=0)
    report = {"pass": overlap >= 20 and all(row["pass"] for row in results.values()),
              "common_overlap_s": overlap, **(extra_report or {}), "cameras": results}
    save_json(directory / "summary.json", report)
    print(json.dumps({"common_overlap_s": overlap, "media_gate_pass": report["pass"], **(extra_report or {}), "cameras":
                      {alias: {k: v for k, v in row.items() if k not in ("ranges", "segments")}
                       for alias, row in results.items()}}, indent=2), flush=True)
    return report["pass"]


def run_round(directory, devices, env, sources=None, stagger=False):
    if shutil.disk_usage(directory).free < 2 * DISK_LIMIT:
        raise RuntimeError("Not enough disk space for the bounded capture.")
    check_ports([HTTP, RTSP, PLAYBACK, API])
    go2rtc, mediamtx = prepare(directory, devices, sources, stagger)
    restream = recorder = None
    observations = {alias: [] for alias in CAMERAS}
    cache = {}
    index_task = None
    pool = ThreadPoolExecutor(max_workers=1)
    gap_start = None
    resumed = False
    window_end = None
    media_started = None
    cutoff = None
    pending = list(CAMERAS) if stagger else []
    try:
        restream = start_service(directory, "go2rtc", [go2rtc, "-config", "go2rtc.json"], env)
        wait_http(HTTP, "/api/streams", restream)
        print(f"Store dashboard: http://127.0.0.1:{HTTP}/ · {WALL_LIMIT}s cutoff · "
              f"{DISK_LIMIT // 1024**2} MiB recording budget", flush=True)
        clock = time.time
        media_started = clock()
        started = time.monotonic()
        recorder = start_service(directory, "mediamtx", [mediamtx, "mediamtx.json"], env)
        wait_http(API, "/v3/paths/list", recorder)
        while recorder.poll() is None and restream.poll() is None:
            elapsed = time.monotonic() - started
            size = sum(row[0] for row in snapshot(directory).values())
            if size >= DISK_LIMIT - 3 * 4 * 1024**2:
                cutoff = "disk budget"
                break
            if elapsed >= WALL_LIMIT:
                cutoff = "wall limit"
                break
            if pending and elapsed >= (len(CAMERAS) - len(pending)) * 9:
                alias = pending.pop(0)
                request(API, f"/v3/config/paths/patch/{alias}",
                        {"source": f"rtsp://127.0.0.1:{RTSP}/{alias}", "rtspTransport": "tcp", "record": True})
                print(f"Started recorder input for {CAMERAS[alias]} at +{elapsed:.1f}s.", flush=True)
            if elapsed >= (60 if stagger else 48) and gap_start is None:
                window_end = clock()
                gap_start = window_end
                toggle_recording(False)
                print("Deliberate recorder gap started; live ingest stays active.", flush=True)
            elif gap_start is not None and not resumed and clock() - gap_start >= 8:
                toggle_recording(True)
                print("Recording resumed.", flush=True)
                resumed = True
            if index_task is None or index_task.done():
                if index_task is not None:
                    index_task.result()
                index_task = pool.submit(saved_coverage, directory, cache)
            telemetry(directory, observations, clock)
            time.sleep(.25)
    finally:
        stop_at = time.time()
        if recorder is not None and recorder.poll() is None:
            try:
                toggle_recording(False)
            except (URLError, TimeoutError):
                pass
        stop_service(recorder)
        stop_service(restream, tree=True)
        (directory / "start.lock").unlink(missing_ok=True)
        pool.shutdown(wait=True, cancel_futures=True)
        last = snapshot(directory)
        time.sleep(1)
        still = snapshot(directory)
        save_json(directory / "reader-telemetry.json", observations)
        stopped = last == still and all(proc is None or proc.poll() is not None for proc in (recorder, restream))
        print(f"Recorder and restream stopped; writes stable={stopped}; {len(still)} segments, "
              f"{sum(v[0] for v in still.values())} bytes; stop={cutoff or 'service exit'}", flush=True)
    if media_started is None:
        return False
    return summary(directory, observations, media_started, window_end or stop_at,
                   gap_start) and stopped and resumed and cutoff == "wall limit"


def review(directory, seconds=300):
    """Serve already recorded files for up to five minutes with no camera connections."""
    if not directory.is_dir():
        raise RuntimeError("Requested round does not exist.")
    check_ports([HTTP, PLAYBACK])
    go2rtc, mediamtx = prepare(directory)
    restream = recorder = None
    try:
        restream = start_service(directory, "go2rtc-review", [go2rtc, "-config", "go2rtc-review.json"])
        recorder = start_service(directory, "mediamtx-review", [mediamtx, "mediamtx-review.json"])
        wait_http(HTTP, "/", restream)
        print(f"Playback-only review: http://127.0.0.1:{HTTP}/#playback (up to {seconds} seconds)", flush=True)
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline and restream.poll() is None and recorder.poll() is None:
            time.sleep(.25)
        return time.monotonic() >= deadline
    finally:
        stop_service(recorder)
        stop_service(restream, tree=True)


def has_main_idr(data):
    """Return True once Annex-B bytes contain main-stream SPS (level 50), PPS, and an IDR."""
    main_sps = main_pps = idr = False
    for part in re.split(rb"\x00\x00\x01|\x00\x00\x00\x01", data):
        if not part:
            continue
        nal_type = part[0] & 0x1F
        if nal_type == 7:
            main_sps = len(part) >= 4 and part[3] == 50
            main_pps = False
        elif nal_type == 8 and main_sps:
            main_pps = True
        elif nal_type == 5 and main_sps and main_pps:
            idr = True
            break
    return idr


def run_kitchen_arm_a(directory, serial, env, startup_limit_s=20, observe_s=12, wall_limit_s=36):
    if shutil.disk_usage(directory).free < 2 * DISK_LIMIT:
        raise RuntimeError("Not enough disk space for Arm A capture.")
    video = directory / "kitchen_a.h264"
    stderr_log = directory / "kitchen_a.stderr.log"
    args = [str(BRIDGE), "devices", "media-probe", serial,
            "--timeout", f"{startup_limit_s}s",
            "--observe-for", f"{observe_s}s",
            "--progress-every", "100ms",
            "--output", str(video)]
    progress_lines = []
    stdout_chunks = []
    started_mono = time.monotonic()
    started_utc = datetime.now(timezone.utc)
    process = subprocess.Popen(args, cwd=ROOT, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                               text=True, encoding="utf-8", errors="replace", creationflags=FLAGS)

    def read_stdout():
        try:
            stdout_chunks.append(process.stdout.read())
        finally:
            process.stdout.close()

    def read_stderr():
        try:
            with stderr_log.open("w", encoding="utf-8") as log_file:
                for line in process.stderr:
                    elapsed_ms = round((time.monotonic() - started_mono) * 1000)
                    log_file.write(f"{elapsed_ms}ms {line}")
                    log_file.flush()
                    progress_lines.append((elapsed_ms, line.strip()))
        finally:
            process.stderr.close()

    stdout_thread = threading.Thread(target=read_stdout, daemon=True)
    stderr_thread = threading.Thread(target=read_stderr, daemon=True)
    stdout_thread.start()
    stderr_thread.start()

    first_preview_ms = None
    first_main_ms = None
    first_main_mono = None
    cutoff = None
    try:
        while process.poll() is None:
            now_mono = time.monotonic()
            elapsed_s = now_mono - started_mono
            if video.exists():
                size = video.stat().st_size
                if size > 0 and first_preview_ms is None:
                    first_preview_ms = round(elapsed_s * 1000)
                if size > 0 and first_main_ms is None:
                    try:
                        if has_main_idr(video.read_bytes()):
                            first_main_mono = now_mono
                            first_main_ms = round(elapsed_s * 1000)
                    except OSError:
                        pass
                if size >= DISK_LIMIT:
                    cutoff = "disk_budget"
                    process.kill()
                    break
            if first_main_mono is None and elapsed_s >= startup_limit_s + 2:
                cutoff = "startup_timeout"
                process.kill()
                break
            if first_main_mono is not None and (now_mono - first_main_mono) >= observe_s + 5:
                cutoff = "observation_wall_limit"
                process.kill()
                break
            if elapsed_s >= wall_limit_s:
                cutoff = "wall_limit"
                process.kill()
                break
            time.sleep(0.02)
        process.wait(timeout=5)
    finally:
        stop_service(process, tree=True)
        stdout_thread.join(timeout=2)
        stderr_thread.join(timeout=2)

    stdout_text = "".join(stdout_chunks)
    details = json.loads(stdout_text) if process.returncode == 0 and stdout_text.strip() else {}
    if details:
        save_json(directory / "kitchen_a.probe.json", details)

    transitions = {}
    ice_states = []
    for host_elapsed_ms, line in progress_lines:
        if "live probe:" not in line:
            continue
        fields = dict(re.findall(r"(\w+)=([^\s]+)", line))
        elapsed_ms = int(fields["elapsed_ms"]) if "elapsed_ms" in fields else host_elapsed_ms
        if "first_progress_ms" not in transitions:
            transitions["first_progress_ms"] = elapsed_ms
        for key in ("auth", "peer", "answer", "channel", "start", "ack"):
            if fields.get(key) == "true" and f"{key}_ms" not in transitions:
                transitions[f"{key}_ms"] = elapsed_ms
        if int(fields.get("video", "0")) > 0 and "video_progress_ms" not in transitions:
            transitions["video_progress_ms"] = elapsed_ms
        state = fields.get("state")
        if state and (not ice_states or ice_states[-1][1] != state):
            ice_states.append((elapsed_ms, state))

    ticket_wake_ms = details.get("ticketMillis")
    if ticket_wake_ms is None and "first_progress_ms" in transitions:
        ticket_wake_ms = max(0, transitions["first_progress_ms"] - 100)

    first_video_ms = details.get("firstVideoMillis", -1)
    first_rtp_ms = first_video_ms if (first_video_ms is not None and first_video_ms >= 0) else transitions.get("video_progress_ms")
    bridge_first_frame_ms = details.get("firstFrameMillis", -1)
    first_complete_frame_ms = bridge_first_frame_ms if (bridge_first_frame_ms is not None and bridge_first_frame_ms >= 0) else first_main_ms

    check = inspect_video(video, decode=True) if video.exists() and video.stat().st_size else {}
    pts = check.pop("pts", [])
    pts_stamps = [p[0] for p in pts]
    passed = (tuple(TARGET) in (check.get("decoded_dimensions") or [])
              and check.get("decode_exit") == 0
              and check.get("decode_errors") == 0
              and check.get("frames", 0) > 0
              and details.get("endReason") == "observation_complete")

    return {
        "arm": "A_standalone",
        "pass": passed,
        "started_utc": started_utc.isoformat(),
        "exit_code": process.returncode,
        "cutoff": cutoff or details.get("endReason"),
        "wake_code": details.get("wakeCode"),
        "join_code": details.get("joinCode"),
        "offer_attempts": details.get("offerAttempts"),
        "ticket_wake_ms": ticket_wake_ms,
        "peer_online_ms": transitions.get("peer_ms"),
        "sdp_answer_ms": transitions.get("answer_ms"),
        "ice_state_transitions": ice_states,
        "ice_connected_ms": next((ms for ms, st in ice_states if st == "connected"), None),
        "selected_candidates": f"{details.get('localCandidateType', '')}/{details.get('remoteCandidateType', '')}",
        "data_channel_open_ms": transitions.get("channel_ms"),
        "start_live_ms": transitions.get("start_ms"),
        "start_live_ack_ms": transitions.get("ack_ms"),
        "start_live_ack": details.get("startLiveAck"),
        "first_rtp_ms": first_rtp_ms,
        "first_preview_h264_frame_ms": first_preview_ms,
        "first_complete_h264_frame_ms": first_complete_frame_ms,
        "first_reader_frame_ms": None,
        "video_packets": details.get("videoPackets"),
        "video_samples": details.get("videoSamples"),
        "assembler_drops": details.get("droppedVideoPackets"),
        "bridge_frames": details.get("outputFrames"),
        "largest_rtp_gap_ms": details.get("largestVideoGapMillis"),
        "largest_frame_gap_ms": details.get("largestFrameGapMillis"),
        "max_pts_gap_s": round(max_step(pts_stamps), 3) if pts_stamps else None,
        **check,
    }


def parse_bridge_health_log(health_path, reader_observations, go2rtc_observations):
    """Parse microsecond UTC health log from continuous bridge run."""
    attempts = []
    retries = 0
    current = None

    if health_path.exists():
        for line in health_path.read_text(encoding="utf-8", errors="replace").splitlines():
            m = re.match(r"^(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}\.\d{6})\s+(.*)$", line)
            if not m:
                continue
            stamp_utc = datetime.strptime(m.group(1), "%Y/%m/%d %H:%M:%S.%f").replace(tzinfo=timezone.utc)
            fields = dict(re.findall(r"(\w+)=([^\s]+)", m.group(2)))
            event = fields.get("event")
            if event == "attempt":
                current = {
                    "attempt": int(fields.get("attempt", len(attempts) + 1)),
                    "attempt_utc_dt": stamp_utc,
                    "attempt_utc": stamp_utc.isoformat(),
                    "ticket_wake_ms": None,
                    "first_rtp_ms": None,
                    "first_frame_ms": None,
                    "transitions": {},
                    "ice_states": [],
                    "session_end": {},
                    "stalls": [],
                }
                attempts.append(current)
                continue
            if event == "retry":
                retries += 1
                continue
            if current is None:
                continue
            elapsed_ms = round((stamp_utc - current["attempt_utc_dt"]).total_seconds() * 1000)
            if event == "ticket" and current["ticket_wake_ms"] is None:
                current["ticket_wake_ms"] = int(fields.get("elapsed_ms", elapsed_ms))
            elif event == "first_frame" and current["first_frame_ms"] is None:
                current["first_frame_ms"] = int(fields.get("elapsed_ms", elapsed_ms))
                current["first_rtp_ms"] = int(fields.get("first_rtp_ms", -1))
            elif event == "progress":
                for key in ("auth", "peer", "answer", "channel", "start", "ack"):
                    if fields.get(key) == "true" and f"{key}_ms" not in current["transitions"]:
                        current["transitions"][f"{key}_ms"] = elapsed_ms
                if int(fields.get("packets", "0")) > 0 and current["first_rtp_ms"] is None:
                    current["first_rtp_ms"] = int(fields.get("last_rtp_ms", elapsed_ms))
                state = fields.get("state")
                if state and (not current["ice_states"] or current["ice_states"][-1][1] != state):
                    current["ice_states"].append((elapsed_ms, state))
                current["last_progress"] = {"elapsed_ms": elapsed_ms, **fields}
            elif event in ("video_gap", "output_stall", "startup_timeout", "error"):
                current["stalls"].append({"elapsed_ms": elapsed_ms, **fields})
            elif event == "session_end":
                current["session_end"] = {"elapsed_ms": elapsed_ms, **fields}

    chosen = next((a for a in attempts if a["first_frame_ms"] is not None or int(a["session_end"].get("packets", 0)) > 0),
                  attempts[0] if attempts else None)
    attempt_utc = chosen["attempt_utc_dt"] if chosen else None
    transitions = chosen["transitions"] if chosen else {}
    ice_states = chosen["ice_states"] if chosen else []
    session_end = (chosen["session_end"] or chosen.get("last_progress", {})) if chosen else {}

    first_producer_ms = None
    first_consumer_ms = None
    if attempt_utc is not None:
        for row in go2rtc_observations:
            rel_ms = round((datetime.fromtimestamp(row["at"], tz=timezone.utc) - attempt_utc).total_seconds() * 1000)
            if row["producer_bytes"] > 0 and first_producer_ms is None:
                first_producer_ms = rel_ms
            if row["consumer_bytes"] > 0 and first_consumer_ms is None:
                first_consumer_ms = rel_ms

    first_reader_frame_ms = None
    if attempt_utc is not None and reader_observations:
        first_reader_utc = datetime.fromtimestamp(reader_observations[0]["unix_s"], tz=timezone.utc)
        first_reader_frame_ms = round((first_reader_utc - attempt_utc).total_seconds() * 1000)

    clean_attempts = [
        {k: v for k, v in a.items() if k != "attempt_utc_dt"}
        for a in attempts
    ]

    return {
        "attempt_utc": attempt_utc.isoformat() if attempt_utc else None,
        "attempts_count": len(attempts),
        "ticket_wake_ms": chosen["ticket_wake_ms"] if chosen else None,
        "peer_online_ms": transitions.get("peer_ms"),
        "sdp_answer_ms": transitions.get("answer_ms"),
        "ice_state_transitions": ice_states,
        "ice_connected_ms": next((ms for ms, st in ice_states if st == "connected"), None),
        "selected_candidates": session_end.get("ice"),
        "data_channel_open_ms": transitions.get("channel_ms"),
        "start_live_ms": transitions.get("start_ms"),
        "start_live_ack_ms": transitions.get("ack_ms"),
        "start_live_ack": session_end.get("start_ack") == "true" or session_end.get("ack") == "true" or "ack_ms" in transitions,
        "first_rtp_ms": chosen["first_rtp_ms"] if chosen else None,
        "first_complete_h264_frame_ms": chosen["first_frame_ms"] if chosen else None,
        "first_go2rtc_producer_ms": first_producer_ms,
        "first_go2rtc_consumer_ms": first_consumer_ms,
        "first_reader_frame_ms": first_reader_frame_ms,
        "end_reason": session_end.get("reason"),
        "join_code": int(session_end["join_code"]) if "join_code" in session_end else None,
        "offer_attempts": int(session_end["offers"]) if "offers" in session_end else None,
        "video_packets": int(session_end["packets"]) if "packets" in session_end else None,
        "video_samples": int(session_end["samples"]) if "samples" in session_end else None,
        "assembler_drops": int(session_end["drops"]) if "drops" in session_end else None,
        "bridge_frames": int(session_end["frames"]) if "frames" in session_end else None,
        "largest_rtp_gap_ms": int(session_end["max_rtp_gap_ms"]) if "max_rtp_gap_ms" in session_end else None,
        "largest_frame_gap_ms": int(session_end["max_frame_gap_ms"]) if "max_frame_gap_ms" in session_end else None,
        "bridge_retries": retries,
        "stalls": chosen["stalls"] if chosen else [],
        "attempts": clean_attempts,
    }


def run_kitchen_arm_b(directory, serial, env, startup_limit_s=20, observe_s=12, wall_limit_s=36):
    if shutil.disk_usage(directory).free < 2 * DISK_LIMIT:
        raise RuntimeError("Not enough disk space for Arm B capture.")
    check_ports([HTTP, RTSP])
    go2rtc = LOCAL / "home-dashboard/go2rtc.exe"
    rel_bridge = os.path.relpath(BRIDGE, directory).replace("\\", "/")
    health_name = "kitchen_b.bridge.log"
    health_path = directory / health_name
    source = (f"exec:{rel_bridge} devices media-probe {serial} --continuous "
              f"--timeout {startup_limit_s}s --observe-for {observe_s}s "
              f"--progress-every 100ms --output - --health-log {health_name}")
    go_config = {
        "api": {"listen": f"127.0.0.1:{HTTP}", "allow_paths": ["/api/streams"]},
        "rtsp": {"listen": f"127.0.0.1:{RTSP}"},
        "webrtc": {"listen": "", "ice_servers": []},
        "streams": {"kitchen_back": source},
        "exec": {"allow_paths": [rel_bridge]},
    }
    save_json(directory / "go2rtc-kitchen.json", go_config)

    video = directory / "kitchen_b.mp4"
    reader_log = directory / "kitchen_b.reader.log"
    reader_observations = []
    go2rtc_observations = []
    restream = None
    reader = None
    reader_thread = None
    cutoff = None
    reader_started_mono = None
    attempt_mono = None
    first_frame_mono = None

    def watch_reader(proc):
        last_pts = -1
        try:
            for line in proc.stdout:
                key, _, value = line.strip().partition("=")
                if key == "out_time_us" and value not in ("N/A", ""):
                    try:
                        pts_us = int(value)
                    except ValueError:
                        continue
                    if pts_us > last_pts:
                        last_pts = pts_us
                        reader_observations.append({
                            "elapsed_ms": round((time.monotonic() - reader_started_mono) * 1000),
                            "unix_s": time.time(),
                            "out_time_us": pts_us,
                        })
        finally:
            proc.stdout.close()

    try:
        restream = start_service(directory, "go2rtc-kitchen", [go2rtc, "-config", "go2rtc-kitchen.json"], env)
        wait_http(HTTP, "/api/streams", restream)
        reader_started_mono = time.monotonic()
        with reader_log.open("w", encoding="utf-8") as err_out:
            reader = subprocess.Popen([
                "ffmpeg", "-hide_banner", "-v", "warning", "-nostats",
                "-stats_period", "0.1", "-progress", "pipe:1", "-rtsp_transport", "tcp",
                "-timeout", "25000000", "-analyzeduration", "1000000", "-probesize", "524288",
                "-i", f"rtsp://127.0.0.1:{RTSP}/kitchen_back", "-map", "0:v:0", "-c:v", "copy",
                "-movflags", "+frag_keyframe+empty_moov+default_base_moof",
                str(video),
            ], cwd=directory, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=err_out,
               text=True, encoding="utf-8", errors="replace", creationflags=FLAGS)
        reader_thread = threading.Thread(target=watch_reader, args=(reader,), daemon=True)
        reader_thread.start()

        while reader.poll() is None and restream.poll() is None:
            now_mono = time.monotonic()
            elapsed_s = now_mono - reader_started_mono
            if video.exists() and video.stat().st_size >= DISK_LIMIT:
                cutoff = "disk_budget"
                break
            try:
                streams = json.loads(request(HTTP, "/api/streams"))
                st = streams.get("kitchen_back") or {}
                producers = [p for p in st.get("producers") or [] if "bytes_recv" in p]
                consumers = [c for c in st.get("consumers") or [] if "bytes_send" in c]
                go2rtc_observations.append({
                    "at": time.time(),
                    "producer_bytes": sum(p["bytes_recv"] for p in producers),
                    "consumer_bytes": sum(c["bytes_send"] for c in consumers),
                })
            except (URLError, TimeoutError, ValueError):
                pass
            if health_path.exists():
                log_text = health_path.read_text(encoding="utf-8", errors="replace")
                if attempt_mono is None and "event=attempt" in log_text:
                    attempt_mono = now_mono
                if first_frame_mono is None and "event=first_frame" in log_text:
                    first_frame_mono = now_mono
                if "event=session_end" in log_text:
                    time.sleep(0.3)
                    cutoff = "observation_complete" if "reason=observation_complete" in log_text else "session_end"
                    break
            origin_mono = attempt_mono if attempt_mono is not None else reader_started_mono
            if first_frame_mono is None and (now_mono - origin_mono) >= startup_limit_s + 2:
                cutoff = "startup_timeout"
                break
            if first_frame_mono is not None and (now_mono - first_frame_mono) >= observe_s + 5:
                cutoff = "observation_wall_limit"
                break
            if elapsed_s >= wall_limit_s:
                cutoff = "wall_limit"
                break
            time.sleep(0.1)
    finally:
        stop_service(restream, tree=True)
        if reader is not None and reader.poll() is None:
            try:
                reader.stdin.write("q\n")
                reader.stdin.flush()
            except OSError:
                pass
            try:
                reader.wait(timeout=4)
            except subprocess.TimeoutExpired:
                stop_service(reader)
        if reader_thread is not None:
            reader_thread.join(timeout=2)

    save_json(directory / "kitchen_b.reader-progress.json", reader_observations)
    save_json(directory / "kitchen_b.go2rtc-telemetry.json", go2rtc_observations)
    bridge_metrics = parse_bridge_health_log(directory / health_name, reader_observations, go2rtc_observations)
    check = inspect_video(video, decode=True) if video.exists() and video.stat().st_size else {}
    pts = check.pop("pts", [])
    pts_stamps = [p[0] for p in pts]
    reader_times_s = [row["elapsed_ms"] / 1000.0 for row in reader_observations]
    reader_span_s = round(reader_times_s[-1] - reader_times_s[0], 3) if reader_times_s else 0
    max_reader_gap_s = round(max_step(reader_times_s), 3) if reader_times_s else None
    max_pts_gap_s = round(max_step(pts_stamps), 3) if pts_stamps else None

    passed = (check.get("decoded_dimensions") == [tuple(TARGET)]
              and check.get("decode_exit") == 0
              and check.get("decode_errors") == 0
              and check.get("frames", 0) > 0
              and bridge_metrics.get("end_reason") == "observation_complete"
              and reader_span_s >= 6.0
              and (max_reader_gap_s is not None and max_reader_gap_s <= MAX_GAP)
              and (max_pts_gap_s is not None and max_pts_gap_s <= MAX_GAP))

    return {
        "arm": "B_go2rtc_continuous",
        "pass": passed,
        "reader_exit_code": reader.returncode if reader is not None else None,
        "cutoff": cutoff,
        "reader_first_s_from_launch": round(reader_times_s[0], 3) if reader_times_s else None,
        "reader_span_s": reader_span_s,
        "max_reader_gap_s": max_reader_gap_s,
        "max_pts_gap_s": max_pts_gap_s,
        **bridge_metrics,
        **check,
    }


def kitchen_ab(directory, devices, env):
    serial = devices["kitchen_back"]
    print("Running Arm A: standalone Go bridge probe for Kitchen Back...", flush=True)
    arm_a = run_kitchen_arm_a(directory, serial, env)
    save_json(directory / "arm-a.json", arm_a)
    print(f"Arm A complete: pass={arm_a['pass']} first_rtp_ms={arm_a['first_rtp_ms']} "
          f"first_frame_ms={arm_a['first_complete_h264_frame_ms']} frames={arm_a.get('frames')}", flush=True)
    time.sleep(5)
    print("Running Arm B: go2rtc continuous source + one loopback reader for Kitchen Back...", flush=True)
    arm_b = run_kitchen_arm_b(directory, serial, env)
    save_json(directory / "arm-b.json", arm_b)
    print(f"Arm B complete: pass={arm_b['pass']} first_rtp_ms={arm_b['first_rtp_ms']} "
          f"first_frame_ms={arm_b['first_complete_h264_frame_ms']} "
          f"first_reader_frame_ms={arm_b['first_reader_frame_ms']} frames={arm_b.get('frames')}", flush=True)
    comparison = {"arm_a": arm_a, "arm_b": arm_b, "both_pass": bool(arm_a["pass"] and arm_b["pass"])}
    save_json(directory / "summary.json", comparison)
    print(json.dumps(comparison, indent=2), flush=True)
    return comparison["both_pass"]


def stop_camera_bridge(serial):
    if not serial:
        return
    subprocess.run([
        "pwsh", "-NoProfile", "-Command",
        f"Get-CimInstance Win32_Process -Filter \"Name = 'vico-cli.exe'\" | "
        f"Where-Object {{ $_.CommandLine -like '*{serial}*' }} | "
        f"ForEach-Object {{ Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }}"
    ], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=5, creationflags=FLAGS)


def analyze_phase(directory, observations, disk, alias, phase_start, phase_end, stream_active_at_start=False):
    health_path = directory / f"{alias}.bridge.log"
    snaps = []
    recoveries = []
    retries_ts = []
    attempt_num = 0
    attempt_start_ts = None

    if health_path.exists():
        for line in health_path.read_text(encoding="utf-8", errors="replace").splitlines():
            m = re.match(r"^(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}\.\d{6})\s+(.*)$", line)
            if not m:
                continue
            ts = datetime.strptime(m.group(1), "%Y/%m/%d %H:%M:%S.%f").replace(tzinfo=timezone.utc).timestamp()
            fields = dict(re.findall(r"(\w+)=([^\s]+)", m.group(2)))
            event = fields.get("event")
            if event == "attempt":
                attempt_num = int(fields.get("attempt", attempt_num + 1))
                attempt_start_ts = ts
            elif event == "retry":
                retries_ts.append(ts)
            elif event == "video_recovered" and "gap_ms" in fields:
                recoveries.append((ts, int(fields["gap_ms"]) / 1000.0))
            elif event in ("progress", "session_end", "output_stall") and attempt_start_ts is not None:
                last_rtp_ms = int(fields.get("last_rtp_ms", -1))
                last_frame_ms = int(fields.get("last_frame_ms", -1))
                rtp_utc = attempt_start_ts + last_rtp_ms / 1000.0 if last_rtp_ms >= 0 else None
                frame_utc = attempt_start_ts + last_frame_ms / 1000.0 if last_frame_ms >= 0 else None
                snaps.append({
                    "ts": ts,
                    "attempt": attempt_num,
                    "event": event,
                    "fields": fields,
                    "rtp_utc": rtp_utc,
                    "frame_utc": frame_utc,
                    "last_rtp_ms": last_rtp_ms,
                    "last_frame_ms": last_frame_ms,
                })

    before = [s for s in snaps if s["ts"] <= phase_start and s["event"] in ("progress", "session_end")]
    inside = [s for s in snaps if phase_start <= s["ts"] <= phase_end]
    inside_prog = [s for s in inside if s["event"] in ("progress", "session_end")]

    def counter_delta(key):
        if not inside_prog:
            return 0
        by_attempt = {}
        if before:
            b = before[-1]
            by_attempt[b["attempt"]] = [int(b["fields"].get(key, 0)), int(b["fields"].get(key, 0))]
        for s in inside_prog:
            att = s["attempt"]
            val = int(s["fields"].get(key, 0))
            if att not in by_attempt:
                by_attempt[att] = [0, val]
            else:
                by_attempt[att][1] = max(by_attempt[att][1], val)
        return sum( hi - lo for lo, hi in by_attempt.values() )

    rtp_utcs = sorted({
        s["rtp_utc"] for s in inside_prog
        if s["rtp_utc"] is not None and phase_start - 1.0 <= s["rtp_utc"] <= phase_end + 1.0
    })
    max_rtp_gap_s = max_step(rtp_utcs) if len(rtp_utcs) >= 2 else 0.0

    frame_utcs = sorted({
        s["frame_utc"] for s in inside_prog
        if s["frame_utc"] is not None and phase_start <= s["frame_utc"] <= phase_end
    })
    if stream_active_at_start and before and before[-1]["frame_utc"] is not None:
        anchor = max(phase_start, before[-1]["frame_utc"])
        frame_utcs = sorted({anchor, *frame_utcs})
    max_bridge_frame_gap_s = max_step(frame_utcs) if len(frame_utcs) >= 2 else 0.0
    if frame_utcs:
        if stream_active_at_start:
            max_bridge_frame_gap_s = max(max_bridge_frame_gap_s, frame_utcs[0] - phase_start)
        max_bridge_frame_gap_s = max(max_bridge_frame_gap_s, phase_end - frame_utcs[-1])
    for r_ts, gap_s in recoveries:
        if phase_start <= r_ts <= phase_end:
            max_bridge_frame_gap_s = max(max_bridge_frame_gap_s, gap_s)

    max_rtp_after_last_frame_s = 0.0
    for s in inside:
        if s["last_frame_ms"] >= 0 and s["last_rtp_ms"] >= s["last_frame_ms"]:
            max_rtp_after_last_frame_s = max(
                max_rtp_after_last_frame_s,
                (s["last_rtp_ms"] - s["last_frame_ms"]) / 1000.0
            )

    reader = reader_metrics(observations[alias], phase_start, phase_end)

    stats = disk[alias]
    phase_ranges = intersect(stats["ranges"], [(phase_start, phase_end)])
    saved_duration_s = sum(b - a for a, b in phase_ranges)
    phase_pts = []
    for path in files(directory, alias):
        pts = inspect_video(path, decode=False)["pts"]
        phase_pts.extend(
            t for stamp, _, _ in pts
            if phase_start <= (t := segment_start(path) + stamp) <= phase_end
        )
    phase_pts.sort()
    max_saved_gap_s = max_step(phase_pts) if phase_pts else None
    if phase_pts:
        if stream_active_at_start:
            max_saved_gap_s = max(max_saved_gap_s, phase_pts[0] - phase_start)
        max_saved_gap_s = max(max_saved_gap_s, phase_end - phase_pts[-1])

    attempts_seen = sorted({s["attempt"] for s in inside_prog})
    retries_in_phase = sum(1 for ts in retries_ts if phase_start <= ts <= phase_end)

    return {
        "phase_start_utc": datetime.fromtimestamp(phase_start, tz=timezone.utc).isoformat(),
        "phase_end_utc": datetime.fromtimestamp(phase_end, tz=timezone.utc).isoformat(),
        "phase_duration_s": round(phase_end - phase_start, 3),
        "attempts_seen": attempts_seen,
        "retries_in_phase": retries_in_phase,
        "rtp_packets_delta": counter_delta("packets"),
        "bridge_frames_delta": counter_delta("frames"),
        "assembler_drops_delta": counter_delta("drops"),
        "rtp_seq_gaps_delta": counter_delta("seq_gaps"),
        "rtp_seq_missing_delta": counter_delta("seq_missing"),
        "rtp_seq_late_delta": counter_delta("seq_late"),
        "assembly_drops_delta": counter_delta("asm_drops"),
        "assembly_late_delta": counter_delta("asm_late"),
        "max_rtp_gap_s": round(max_rtp_gap_s, 3),
        "max_bridge_frame_gap_s": round(max_bridge_frame_gap_s, 3),
        "max_rtp_after_last_frame_s": round(max_rtp_after_last_frame_s, 3),
        "max_reader_gap_s": round(reader["max_reader_gap_s"], 3) if reader["max_reader_gap_s"] is not None else None,
        "reader_span_s": round(reader["reader_span_s"], 3),
        "saved_duration_s": round(saved_duration_s, 3),
        "max_saved_gap_s": round(max_saved_gap_s, 3) if max_saved_gap_s is not None else None,
    }


def run_two_camera(directory, devices, env):
    active_cameras = ("kitchen_back", "dining_area")
    if shutil.disk_usage(directory).free < 2 * DISK_LIMIT:
        raise RuntimeError("Not enough disk space for the bounded two-camera capture.")
    check_ports([HTTP, RTSP, PLAYBACK, API])
    two_devices = {alias: devices[alias] for alias in active_cameras}
    go2rtc, mediamtx = prepare(directory, two_devices, stagger=True)
    restream = recorder = None
    observations = {alias: [] for alias in active_cameras}
    cache = {}
    index_task = None
    pool = ThreadPoolExecutor(max_workers=1)
    window_end = None
    media_started = None
    cutoff = None
    kitchen_first_frame_at = None
    kitchen_first_frame_elapsed_s = None
    dining_started_at = None
    dining_started_elapsed_s = None
    dining_first_frame_at = None
    dining_first_frame_elapsed_s = None
    dining_stopped_at = None
    dining_stopped_elapsed_s = None
    kitchen_before_dining = None
    stopped = False
    baseline_s = 12.0
    concurrent_after_dining_frame_s = 25.0
    max_dining_stop_elapsed_s = 58.0
    env = dict(env, VICO_RX_TRACE="1")
    env.setdefault("VICO_REASSEMBLY_WINDOW_MS", "1200")
    env.setdefault("VICO_FRAME_STALL_MS", "1800")  # header-only socket trace beside each private bridge log
    udp_samples = []
    last_udp = 0.0
    try:
        restream = start_service(directory, "go2rtc", [go2rtc, "-config", "go2rtc.json"], env)
        wait_http(HTTP, "/api/streams", restream)
        print(f"Three-phase store test (Kitchen -> Kitchen+Dining -> Kitchen): http://127.0.0.1:{HTTP}/ · "
              f"{WALL_LIMIT}s cutoff · {DISK_LIMIT // 1024**2} MiB recording budget", flush=True)
        clock = time.time
        media_started = clock()
        started = time.monotonic()
        recorder = start_service(directory, "mediamtx", [mediamtx, "mediamtx.json"], env)
        wait_http(API, "/v3/paths/list", recorder)
        request(API, "/v3/config/paths/patch/kitchen_back",
                {"source": f"rtsp://127.0.0.1:{RTSP}/kitchen_back", "rtspTransport": "tcp", "record": True})
        print("Phase 1: Started recorder input for Kitchen Back at +0.0s; waiting for first 2304x1296 frame...", flush=True)
        kitchen_log = directory / "kitchen_back.bridge.log"
        dining_log = directory / "dining_area.bridge.log"
        while recorder.poll() is None and restream.poll() is None:
            elapsed = time.monotonic() - started
            size = sum(row[0] for row in snapshot(directory).values())
            if size >= DISK_LIMIT - 3 * 4 * 1024**2:
                window_end = clock()
                cutoff = "disk budget"
                break
            if elapsed >= WALL_LIMIT:
                window_end = clock()
                cutoff = "wall limit"
                break
            now_wall = clock()
            if kitchen_first_frame_at is None and kitchen_log.exists():
                log_text = kitchen_log.read_text(encoding="utf-8", errors="replace")
                if "event=first_frame" in log_text:
                    kitchen_first_frame_at = now_wall
                    kitchen_first_frame_elapsed_s = round(elapsed, 3)
                    print(f"Phase 1: Kitchen Back reached first 2304x1296 frame at +{elapsed:.2f}s; "
                          f"collecting {baseline_s:.0f}s solo baseline...", flush=True)
            if (kitchen_first_frame_at is not None and dining_started_at is None
                    and (now_wall - kitchen_first_frame_at) >= baseline_s):
                dining_started_at = now_wall
                dining_started_elapsed_s = round(elapsed, 3)
                kitchen_before_dining = count_frames(kitchen_log)
                request(API, "/v3/config/paths/patch/dining_area",
                        {"source": f"rtsp://127.0.0.1:{RTSP}/dining_area", "rtspTransport": "tcp", "record": True})
                print(f"Phase 2: Started recorder input for Dining Area at +{elapsed:.2f}s while keeping Kitchen open.", flush=True)
            if dining_started_at is not None and dining_first_frame_at is None and dining_log.exists():
                d_text = dining_log.read_text(encoding="utf-8", errors="replace")
                if "event=first_frame" in d_text:
                    dining_first_frame_at = now_wall
                    dining_first_frame_elapsed_s = round(elapsed, 3)
                    print(f"Phase 2: Dining Area reached first 2304x1296 frame at +{elapsed:.2f}s.", flush=True)
            if dining_started_at is not None and dining_stopped_at is None:
                overlap_ready = (dining_first_frame_at is not None
                                 and (now_wall - dining_first_frame_at) >= concurrent_after_dining_frame_s)
                if overlap_ready or elapsed >= max_dining_stop_elapsed_s:
                    dining_stopped_at = now_wall
                    dining_stopped_elapsed_s = round(elapsed, 3)
                    request(API, "/v3/config/paths/patch/dining_area",
                            {"source": "publisher", "record": False})
                    stop_camera_bridge(devices["dining_area"])
                    print(f"Phase 3: Stopped Dining Area at +{elapsed:.2f}s; keeping Kitchen Back open to measure solo recovery.", flush=True)
            if index_task is None or index_task.done():
                if index_task is not None:
                    index_task.result()
                index_task = pool.submit(saved_coverage, directory, cache)
            telemetry(directory, observations, clock, active_cameras=active_cameras)
            if time.monotonic() - last_udp >= 2.0:
                last_udp = time.monotonic()
                counters = udp_counters()
                if counters:
                    udp_samples.append({"at": clock(), **counters})
            time.sleep(.25)
    finally:
        stop_at = time.time()
        if window_end is None:
            window_end = stop_at
        if recorder is not None and recorder.poll() is None:
            try:
                toggle_recording(False, active_cameras=active_cameras)
                time.sleep(0.5)
            except (URLError, TimeoutError):
                pass
        stop_service(recorder)
        stop_service(restream, tree=True)
        (directory / "start.lock").unlink(missing_ok=True)
        pool.shutdown(wait=True, cancel_futures=True)
        last = snapshot(directory)
        time.sleep(1)
        still = snapshot(directory)
        save_json(directory / "reader-telemetry.json", observations)
        stopped = last == still and all(proc is None or proc.poll() is not None for proc in (recorder, restream))
        print(f"Recorder and restream stopped; writes stable={stopped}; {len(still)} segments, "
              f"{sum(v[0] for v in still.values())} bytes; stop={cutoff or 'service exit'}", flush=True)
    if media_started is None:
        return False
    disk = saved_coverage(directory, {}, decode=False)
    p1_start = kitchen_first_frame_at or media_started
    p1_end = dining_started_at or window_end
    p2_start = dining_started_at or p1_end
    p2_end = dining_stopped_at or window_end
    p3_start = dining_stopped_at or p2_end
    p3_end = window_end

    phase1_kitchen = analyze_phase(directory, observations, disk, "kitchen_back", p1_start, p1_end, stream_active_at_start=True)
    phase2_kitchen = analyze_phase(directory, observations, disk, "kitchen_back", p2_start, p2_end, stream_active_at_start=True)
    phase2_dining = analyze_phase(
        directory, observations, disk, "dining_area",
        dining_first_frame_at or p2_start, p2_end,
        stream_active_at_start=dining_first_frame_at is not None
    )
    phase3_kitchen = analyze_phase(directory, observations, disk, "kitchen_back", p3_start, p3_end, stream_active_at_start=True)

    # Check when Kitchen emitted its first complete frame after Dining stopped, and measure post-recovery steady state.
    first_frame_after_dining_stop_s = None
    post_recovery_kitchen = None
    if dining_stopped_at is not None and (directory / "kitchen_back.bridge.log").exists():
        att_start = None
        post_frame_utcs = []
        for line in (directory / "kitchen_back.bridge.log").read_text(encoding="utf-8", errors="replace").splitlines():
            m = re.match(r"^(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}\.\d{6})\s+(.*)$", line)
            if not m:
                continue
            ts = datetime.strptime(m.group(1), "%Y/%m/%d %H:%M:%S.%f").replace(tzinfo=timezone.utc).timestamp()
            fields = dict(re.findall(r"(\w+)=([^\s]+)", m.group(2)))
            if fields.get("event") == "attempt":
                att_start = ts
            elif fields.get("event") == "progress" and att_start is not None:
                lf_ms = int(fields.get("last_frame_ms", -1))
                if lf_ms >= 0:
                    f_utc = att_start + lf_ms / 1000.0
                    if f_utc >= dining_stopped_at:
                        post_frame_utcs.append(f_utc)
        if post_frame_utcs:
            first_post_utc = min(post_frame_utcs)
            first_frame_after_dining_stop_s = round(first_post_utc - dining_stopped_at, 3)
            steady_start = min(p3_end, max(first_post_utc, dining_stopped_at + 2.0))
            if p3_end - steady_start >= 3.0:
                post_recovery_kitchen = analyze_phase(
                    directory, observations, disk, "kitchen_back",
                    steady_start, p3_end, stream_active_at_start=True
                )

    kitchen_after = count_frames(directory / "kitchen_back.bridge.log")
    recovered_without_reconnecting = bool(
        dining_stopped_at is not None
        and kitchen_after["attempts"] == 1
        and kitchen_after["bridge_retries"] == 0
        and phase3_kitchen["bridge_frames_delta"] > 0
        and (post_recovery_kitchen is not None and post_recovery_kitchen["max_bridge_frame_gap_s"] <= MAX_GAP)
    )
    save_json(directory / "udp-counters.json", udp_samples)

    def udp_delta(a, b):
        rows = [r for r in udp_samples if a is not None and b is not None and a <= r["at"] <= b]
        if len(rows) < 2:
            return None
        return {k: rows[-1][k] - rows[0][k] for k in ("received", "no_ports", "receive_errors") if k in rows[0]}

    extra = {
        "run_class": ("concurrent streaming test" if dining_first_frame_at is not None
                      else "startup test (Dining produced no video)"),
        "udp_host_counter_deltas": {
            "phase1_kitchen_only": udp_delta(p1_start, p1_end),
            "phase2_kitchen_plus_dining": udp_delta(p2_start, p2_end),
            "phase3_kitchen_only": udp_delta(p3_start, p3_end),
        },
        "kitchen_first_frame_elapsed_s": kitchen_first_frame_elapsed_s,
        "dining_started_after_kitchen_first_frame": dining_started_at is not None,
        "dining_started_elapsed_s": dining_started_elapsed_s,
        "dining_first_frame_elapsed_s": dining_first_frame_elapsed_s,
        "dining_stopped_elapsed_s": dining_stopped_elapsed_s,
        "kitchen_before_dining_start": kitchen_before_dining,
        "phases": {
            "phase1_kitchen_only": phase1_kitchen,
            "phase2_kitchen_plus_dining": {
                "kitchen_back": phase2_kitchen,
                "dining_area": phase2_dining,
            },
            "phase3_kitchen_only_recovery": {
                "full_phase": phase3_kitchen,
                "first_frame_after_dining_stop_s": first_frame_after_dining_stop_s,
                "steady_state_after_recovery": post_recovery_kitchen,
            },
        },
        "kitchen_same_session_throughout": kitchen_after["attempts"] == 1 and kitchen_after["bridge_retries"] == 0,
        "kitchen_recovered_without_reconnecting": recovered_without_reconnecting,
        "writes_stable": stopped,
        "cutoff": cutoff,
    }
    cam_windows = {
        "kitchen_back": (media_started, window_end),
        "dining_area": (dining_started_at or media_started, dining_stopped_at or window_end),
    }
    return (summary(directory, observations, media_started, window_end,
                    active_cameras=active_cameras, extra_report=extra, camera_windows=cam_windows)
            and stopped and cutoff == "wall limit")


def log_events(path):
    """Bridge health events as (utc_seconds, event, fields)."""
    rows = []
    if path.exists():
        for line in path.read_text(encoding="utf-8", errors="replace").splitlines():
            m = re.match(r"^(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}\.\d{6})\s+(.*)$", line)
            if m:
                ts = datetime.strptime(m.group(1), "%Y/%m/%d %H:%M:%S.%f").replace(tzinfo=timezone.utc).timestamp()
                fields = dict(re.findall(r"(\w+)=([^\s]+)", m.group(2)))
                rows.append((ts, fields.get("event"), fields))
    return rows


def first_event_time(path, event, after=0.0):
    return next((ts for ts, name, _ in log_events(path) if name == event and ts >= after), None)


def rx_trace(directory, alias):
    """Header-only socket trace: video packet arrival times and late arrivals (time, delay_s)."""
    import bisect
    path = directory / f"{alias}.bridge.log.rx.csv"
    times, late = [], []
    if not path.exists():
        return times, late
    blocks, start = [], None
    for line in path.read_text(encoding="utf-8").splitlines():
        if line.startswith("#"):
            m = re.search(r"start_utc=(\S+)", line)
            stamp = re.sub(r"(\.\d{6})\d+", r"\1", m.group(1).replace("Z", "+00:00"))
            start = datetime.fromisoformat(stamp).timestamp()
            blocks.append([])
        elif start is not None and line.strip():
            t, stream, seq, _, _ = (int(v) for v in line.split(","))
            blocks[-1].append((start + t / 1e6, stream, seq))
    for block in blocks:
        counts = {}
        for _, stream, _ in block:
            counts[stream] = counts.get(stream, 0) + 1
        if not counts:
            continue
        video = max(counts, key=counts.get)
        mx, ext, prev = None, 0, None
        seen, adv_ext, adv_t = set(), [], []
        for t, stream, seq in block:
            if stream != video:
                continue
            times.append(t)
            if prev is None:
                ext = seq
            else:
                ext += (seq - prev + 32768) % 65536 - 32768
            prev = seq
            if mx is None or ext > mx:
                mx = ext
                seen.add(ext)
                adv_ext.append(ext)
                adv_t.append(t)
            elif ext not in seen:
                seen.add(ext)
                i = bisect.bisect_right(adv_ext, ext)
                late.append((t, t - (adv_t[i] if i < len(adv_t) else t)))
    return sorted(times), late


def late_stats(late, start, end):
    delays = [d for t, d in late if start <= t < end]
    return {"late_arrivals": len(delays), "late_over_0_5s": sum(d > 0.5 for d in delays),
            "late_over_1s": sum(d > 1.0 for d in delays), "max_late_s": round(max(delays), 3) if delays else 0.0}


def run_powered_pair(directory, devices, env, dining_receiver="bridge"):
    """Dining first, then Cash woken and held connected without video, then released (powered pair)."""
    first, second = "dining_area", "cash_register"
    active_cameras = (first, second)
    inventory = json.loads((directory / "inventory.json").read_text(encoding="utf-8"))
    names = {CAMERAS[alias]: alias for alias in active_cameras}
    power = {names[row["deviceName"]]: row.get("isCharging") for row in inventory if row["deviceName"] in names}
    powered = {alias: str(value).lower() in ("1", "2", "true") for alias, value in power.items()}
    if len(powered) != 2 or not all(powered.values()):
        print(f"Power check failed (charging flags): {power}. No camera session was started.", flush=True)
        raise RuntimeError("Dining Area and Cash Register must both report external power.")
    if shutil.disk_usage(directory).free < 2 * DISK_LIMIT:
        raise RuntimeError("Not enough disk space for the bounded powered-pair capture.")
    check_ports([HTTP, RTSP, PLAYBACK, API])
    hold_s, max_release_elapsed_s, baseline_s = 8.0, 55.0, 12.0
    release_file = directory / "cash_register.release"
    release_file.unlink(missing_ok=True)
    pair = {alias: devices[alias] for alias in active_cameras}
    go2rtc, mediamtx = prepare(directory, pair, stagger=True, hold={second: release_file.name}, dining_receiver=dining_receiver)
    env = dict(env, VICO_RX_TRACE="1")
    env.setdefault("VICO_REASSEMBLY_WINDOW_MS", "1200")
    env.setdefault("VICO_FRAME_STALL_MS", "1800")
    restream = recorder = None
    observations = {alias: [] for alias in active_cameras}
    cache, index_task, window_end, media_started, cutoff = {}, None, None, None, None
    pool = ThreadPoolExecutor(max_workers=1)
    dining_log, cash_log = directory / f"{first}.bridge.log", directory / f"{second}.bridge.log"
    t_dining_first = t_cash_start = t_ready = t_release = t_cash_first = None
    udp_samples, last_udp, stopped = [], 0.0, False
    try:
        restream = start_service(directory, "go2rtc", [go2rtc, "-config", "go2rtc.json"], env)
        wait_http(HTTP, "/api/streams", restream)
        print(f"Powered Dining/Cash test: http://127.0.0.1:{HTTP}/ - {WALL_LIMIT}s cutoff - "
              f"{DISK_LIMIT // 1024**2} MiB recording budget", flush=True)
        clock = time.time
        media_started = clock()
        started = time.monotonic()
        recorder = start_service(directory, "mediamtx", [mediamtx, "mediamtx.json"], env)
        wait_http(API, "/v3/paths/list", recorder)
        request(API, f"/v3/config/paths/patch/{first}",
                {"source": f"rtsp://127.0.0.1:{RTSP}/{first}", "rtspTransport": "tcp", "record": True})
        print("Stage 1: Dining recorder input started at +0.0s; waiting for its first 2304x1296 frame...", flush=True)
        while recorder.poll() is None and restream.poll() is None:
            elapsed = time.monotonic() - started
            if sum(row[0] for row in snapshot(directory).values()) >= DISK_LIMIT - 3 * 4 * 1024**2:
                window_end, cutoff = clock(), "disk budget"
                break
            if elapsed >= WALL_LIMIT:
                window_end, cutoff = clock(), "wall limit"
                break
            now = clock()
            if t_dining_first is None and first_event_time(dining_log, "first_frame") is not None:
                t_dining_first = now
                print(f"Stage 1: Dining first frame at +{elapsed:.2f}s; {baseline_s:.0f}s Dining-only baseline...", flush=True)
            if t_dining_first is not None and t_cash_start is None and now - t_dining_first >= baseline_s:
                t_cash_start = now
                request(API, f"/v3/config/paths/patch/{second}",
                        {"source": f"rtsp://127.0.0.1:{RTSP}/{second}", "rtspTransport": "tcp", "record": True})
                print(f"Stage 2: Cash wake/WebRTC setup started at +{elapsed:.2f}s (no video requested).", flush=True)
            if t_cash_start is not None and t_ready is None and first_event_time(cash_log, "hold_ready") is not None:
                t_ready = now
                print(f"Stage 3: Cash connected (data channel open, holding) at +{elapsed:.2f}s.", flush=True)
            if t_cash_start is not None and t_release is None and (
                    (t_ready is not None and now - t_ready >= hold_s) or elapsed >= max_release_elapsed_s):
                release_file.write_text("release", encoding="utf-8")
                t_release = now
                print(f"Stage 4: released Cash startLive at +{elapsed:.2f}s"
                      f"{'' if t_ready else ' (never connected before the budget)'}.", flush=True)
            if t_release is not None and t_cash_first is None and first_event_time(cash_log, "first_frame") is not None:
                t_cash_first = now
                print(f"Stage 4: Cash first frame at +{elapsed:.2f}s.", flush=True)
            if index_task is None or index_task.done():
                if index_task is not None:
                    index_task.result()
                index_task = pool.submit(saved_coverage, directory, cache)
            telemetry(directory, observations, clock, active_cameras=active_cameras)
            if time.monotonic() - last_udp >= 2.0:
                last_udp = time.monotonic()
                counters = udp_counters()
                if counters:
                    udp_samples.append({"at": clock(), **counters})
            time.sleep(.25)
    finally:
        stop_at = time.time()
        window_end = window_end or stop_at
        if recorder is not None and recorder.poll() is None:
            try:
                toggle_recording(False, active_cameras=active_cameras)
                time.sleep(0.5)
            except (URLError, TimeoutError):
                pass
        stop_service(recorder)
        stop_service(restream, tree=True)
        (directory / "start.lock").unlink(missing_ok=True)
        pool.shutdown(wait=True, cancel_futures=True)
        last = snapshot(directory)
        time.sleep(1)
        still = snapshot(directory)
        save_json(directory / "reader-telemetry.json", observations)
        stopped = last == still and all(proc is None or proc.poll() is not None for proc in (recorder, restream))
        print(f"Recorder and restream stopped; writes stable={stopped}; {len(still)} segments, "
              f"{sum(v[0] for v in still.values())} bytes; stop={cutoff or 'service exit'}", flush=True)
    if media_started is None:
        return False
    save_json(directory / "udp-counters.json", udp_samples)
    disk = saved_coverage(directory, {}, decode=False)
    # Bridge-log times are the authoritative transition stamps.
    ev = {name: first_event_time(cash_log, name, t_cash_start or 0) for name in ("ticket", "hold_ready", "hold_released", "first_frame")}
    d_first = first_event_time(dining_log, "first_frame") or t_dining_first
    t0 = t_cash_start or window_end
    ready = ev["hold_ready"] or t_ready
    released = ev["hold_released"] or t_release or window_end
    cash_first = ev["first_frame"] or t_cash_first
    end = window_end
    phases = {"A_dining_only": (d_first, t0), "B_cash_startup": (t0, ready or released),
              "C_cash_hold_no_video": (ready, released) if ready else None,
              "D_cash_video": (released, end)}
    rx_dining_times, rx_dining_late = rx_trace(directory, first)
    rx_cash_times, _ = rx_trace(directory, second)
    dining = {}
    for name, window in phases.items():
        if not window or window[0] is None or window[1] <= window[0]:
            dining[name] = None
            continue
        row = analyze_phase(directory, observations, disk, first, window[0], window[1], stream_active_at_start=True)
        row.update(late_stats(rx_dining_late, *window))
        row["dining_udp_host_delta"] = None
        dining[name] = row
    cash_video_before_release = sum(1 for t in rx_cash_times if t < released)
    held_rows = [f for ts, name, f in log_events(cash_log) if name == "progress" and ts < released]
    cash_progress_before_release = max([int(f.get("packets", 0)) for f in held_rows] or [0])
    # The camera streams its low-resolution preview on connect, so RTP can flow during the hold.
    # What the hold controls is the main-stream request: no startLive and no emitted frames before release.
    hold_verified = (ready is not None and all(f.get("start") == "false" for f in held_rows)
                     and max([int(f.get("frames", 0)) for f in held_rows] or [0]) == 0)
    cash_d = (analyze_phase(directory, observations, disk, second, cash_first, end, stream_active_at_start=False)
              if cash_first else None)

    def udp_delta(a, b):
        rows = [r for r in udp_samples if a is not None and b is not None and a <= r["at"] <= b]
        return ({k: rows[-1][k] - rows[0][k] for k in ("received", "no_ports", "receive_errors") if k in rows[0]}
                if len(rows) >= 2 else None)

    def rel(t):
        return round(t - media_started, 3) if t is not None else None

    extra = {
        "run_class": f"powered Dining/Cash pair test ({dining_receiver} for Dining; Kitchen not used; both cameras on external power)",
        "dining_receiver": dining_receiver,
        "external_power_flags_at_preflight": powered,
        "transitions_s_from_recorder_start": {
            "dining_first_frame": rel(d_first), "cash_recorder_input_start": rel(t_cash_start),
            "cash_ticket": rel(ev["ticket"]), "cash_connected_hold_start": rel(ready),
            "cash_startlive_released": rel(released), "cash_first_frame": rel(cash_first), "end": rel(end)},
        "hold_verified_no_startlive_and_no_frames_before_release": hold_verified,
        "cash_preview_rtp_packets_at_socket_before_release": cash_video_before_release,
        "cash_preview_rtp_packets_at_bridge_before_release": cash_progress_before_release,
        "dining_phases": dining,
        "cash_video_phase": cash_d,
        "udp_host_counter_deltas": {name: udp_delta(*w) if w else None for name, w in phases.items()},
        "writes_stable": stopped, "cutoff": cutoff,
    }
    cam_windows = {first: (media_started, end), second: (cash_first or released, end)}
    return (summary(directory, observations, media_started, window_end, active_cameras=active_cameras,
                    extra_report=extra, camera_windows=cam_windows) and stopped and cutoff == "wall limit"
            and hold_verified)



def run_receiver_comparison(directory, devices, env):
    """Matched comparison of Dining Area on current bridge vs vicostream, with Cash Register in both arms."""
    active_cameras = ("dining_area", "cash_register")
    inventory = json.loads((directory / "inventory.json").read_text(encoding="utf-8"))
    names = {CAMERAS[alias]: alias for alias in active_cameras}
    power = {names[row["deviceName"]]: row.get("isCharging") for row in inventory if row["deviceName"] in names}
    powered = {alias: str(value).lower() in ("1", "2", "true") for alias, value in power.items()}
    if len(powered) != 2 or not all(powered.values()):
        print(f"Power check failed (charging flags): {power}. No camera session was started.", flush=True)
        raise RuntimeError("Dining Area and Cash Register must both report external power.")

    auth_json = directory / "auth/auth.json"
    auth_data = json.loads(auth_json.read_text(encoding="utf-8")) if auth_json.is_file() else {}
    auth_token = auth_data.get("token")

    # Arm 1: Current bridge for Dining
    arm1_dir = directory / "arm1-bridge"
    arm1_dir.mkdir(parents=True, exist_ok=True)
    (arm1_dir / "auth").mkdir(parents=True, exist_ok=True)
    if auth_json.is_file():
        shutil.copyfile(auth_json, arm1_dir / "auth/auth.json")
    shutil.copyfile(directory / "inventory.json", arm1_dir / "inventory.json")

    arm1_env = dict(env, VICOHOME_CACHE_DIR=str(arm1_dir / "auth"))
    if auth_token:
        arm1_env["VICO_TOKEN"] = auth_token

    print("\n=======================================================", flush=True)
    print("STARTING ARM 1: Current Go Bridge for Dining + Cash", flush=True)
    print("=======================================================", flush=True)
    arm1_pass = run_powered_pair(arm1_dir, devices, arm1_env, dining_receiver="bridge")

    print("\nArm 1 completed. Waiting 10s for clean camera teardown before Arm 2...", flush=True)
    time.sleep(10)

    # Arm 2: vicostream for Dining
    arm2_dir = directory / "arm2-vicostream"
    arm2_dir.mkdir(parents=True, exist_ok=True)
    (arm2_dir / "auth").mkdir(parents=True, exist_ok=True)
    if auth_json.is_file():
        shutil.copyfile(auth_json, arm2_dir / "auth/auth.json")
    shutil.copyfile(directory / "inventory.json", arm2_dir / "inventory.json")

    arm2_env = dict(env, VICOHOME_CACHE_DIR=str(arm2_dir / "auth"))
    if auth_token:
        arm2_env["VICO_TOKEN"] = auth_token

    print("\n=======================================================", flush=True)
    print("STARTING ARM 2: Alternate vicostream for Dining + Bridge for Cash", flush=True)
    print("=======================================================", flush=True)
    arm2_pass = run_powered_pair(arm2_dir, devices, arm2_env, dining_receiver="vicostream")

    # Load summaries for comparison
    arm1_summary = json.loads((arm1_dir / "summary.json").read_text(encoding="utf-8"))
    arm2_summary = json.loads((arm2_dir / "summary.json").read_text(encoding="utf-8"))

    d1 = arm1_summary["cameras"]["dining_area"]
    d2 = arm2_summary["cameras"]["dining_area"]
    c1 = arm1_summary["cameras"]["cash_register"]
    c2 = arm2_summary["cameras"]["cash_register"]

    # Decision evaluation according to the prompt instructions:
    # "If both receivers show gaps at packet arrival, focus on the camera-to-PC transport path.
    #  If packet arrival is comparable but only our bridge stalls, focus on the receiver.
    #  If neither arm reproduces a stall, record that result without assigning a cause."
    d1_packet_gap = d1.get("rtp_gap_s", 0) > MAX_GAP
    d2_packet_gap = d2.get("rtp_gap_s", 0) > MAX_GAP
    d1_bridge_stall = (d1.get("rtp_after_last_frame_s", 0) > MAX_GAP or
                       (d1.get("bridge_gap_s", 0) > MAX_GAP and d1.get("rtp_gap_s", 0) <= MAX_GAP))
    d2_bridge_stall = (d2.get("rtp_after_last_frame_s", 0) > MAX_GAP or
                       (d2.get("bridge_gap_s", 0) > MAX_GAP and d2.get("rtp_gap_s", 0) <= MAX_GAP))

    if d1_packet_gap and d2_packet_gap:
        focus = "camera-to-PC transport path (both receivers show gaps at packet arrival)"
    elif d1_bridge_stall and not d2_bridge_stall:
        focus = "receiver (packet arrival comparable but only our bridge stalls)"
    elif not d1_packet_gap and not d2_packet_gap and not d1_bridge_stall and not d2_bridge_stall and d1.get("pass") and d2.get("pass"):
        focus = "neither arm reproduced a stall (no cause assigned)"
    else:
        # Check saved gaps vs packet arrival
        if d1.get("rtp_gap_s", 0) > MAX_GAP and d2.get("rtp_gap_s", 0) > MAX_GAP:
            focus = "camera-to-PC transport path (both receivers show gaps at packet arrival)"
        elif d1.get("max_saved_gap_s", 0) > MAX_GAP and d2.get("max_saved_gap_s", 0) <= MAX_GAP:
            focus = "receiver (bridge stalled while vicostream maintained continuity)"
        elif d1.get("max_saved_gap_s", 0) <= MAX_GAP and d2.get("max_saved_gap_s", 0) <= MAX_GAP:
            focus = "neither arm reproduced a stall (no cause assigned)"
        else:
            focus = "camera-to-PC transport path (packet arrival loss across both arms)"

    comparison = {
        "pass": arm1_pass and arm2_pass,
        "focus_decision": focus,
        "dining_area": {
            "arm1_bridge": {
                "receiver": "vico-cli (Go bridge)",
                "ice_candidates": d1.get("ice_candidates"),
                "rtp_packets": d1.get("rtp_packets"),
                "rtp_seq_gaps": d1.get("rtp_seq_gaps"),
                "rtp_seq_missing": d1.get("rtp_seq_missing"),
                "rtp_gap_s": d1.get("rtp_gap_s"),
                "bridge_frames": d1.get("bridge_frames"),
                "bridge_gap_s": d1.get("bridge_gap_s"),
                "rtp_after_last_frame_s": d1.get("rtp_after_last_frame_s"),
                "reader_span_s": d1.get("reader_span_s"),
                "max_reader_gap_s": d1.get("max_reader_gap_s"),
                "saved_duration_s": d1.get("saved_duration_s"),
                "max_saved_gap_s": d1.get("max_saved_gap_s"),
                "resolution": d1.get("resolution"),
                "decode_pass": d1.get("decode_pass"),
                "decode_errors": d1.get("decode_errors"),
                "pass": d1.get("pass"),
            },
            "arm2_vicostream": {
                "receiver": "vicostream (independent receiver)",
                "ice_candidates": d2.get("ice_candidates"),
                "rtp_packets": d2.get("rtp_packets"),
                "rtp_seq_gaps": d2.get("rtp_seq_gaps"),
                "rtp_seq_missing": d2.get("rtp_seq_missing"),
                "rtp_gap_s": d2.get("rtp_gap_s"),
                "bridge_frames": d2.get("bridge_frames"),
                "bridge_gap_s": d2.get("bridge_gap_s"),
                "rtp_after_last_frame_s": d2.get("rtp_after_last_frame_s"),
                "reader_span_s": d2.get("reader_span_s"),
                "max_reader_gap_s": d2.get("max_reader_gap_s"),
                "saved_duration_s": d2.get("saved_duration_s"),
                "max_saved_gap_s": d2.get("max_saved_gap_s"),
                "resolution": d2.get("resolution"),
                "decode_pass": d2.get("decode_pass"),
                "decode_errors": d2.get("decode_errors"),
                "pass": d2.get("pass"),
            },
        },
        "cash_register": {
            "arm1": {"pass": c1.get("pass"), "max_saved_gap_s": c1.get("max_saved_gap_s"), "rtp_gap_s": c1.get("rtp_gap_s")},
            "arm2": {"pass": c2.get("pass"), "max_saved_gap_s": c2.get("max_saved_gap_s"), "rtp_gap_s": c2.get("rtp_gap_s")},
        },
        "overlap_s": {
            "arm1": arm1_summary.get("common_overlap_s"),
            "arm2": arm2_summary.get("common_overlap_s"),
        },
    }
    save_json(directory / "comparison.json", comparison)
    print("\n=======================================================", flush=True)
    print("MATCHED COMPARISON SUMMARY:", flush=True)
    print(json.dumps(comparison, indent=2), flush=True)
    print("=======================================================", flush=True)
    return comparison["pass"]


def run_three_camera(directory, devices, env):
    """Bounded three-camera acceptance test: Dining -> Cash -> Kitchen (mixed power)."""
    if shutil.disk_usage(directory).free < 2 * DISK_LIMIT:
        raise RuntimeError("Not enough disk space for the three-camera capture.")
    check_ports([HTTP, RTSP, PLAYBACK, API])

    # Check power inventory at preflight
    inventory = json.loads((directory / "inventory.json").read_text(encoding="utf-8"))
    inv_by_name = {row["deviceName"]: row for row in inventory}
    kitchen_start_battery = inv_by_name.get("Kitchen Back", {}).get("batteryLevel")
    kitchen_start_charging = inv_by_name.get("Kitchen Back", {}).get("isCharging")
    dining_charging = inv_by_name.get("Dining Area", {}).get("isCharging")
    cash_charging = inv_by_name.get("Cash Register", {}).get("isCharging")

    power_status = {
        "kitchen_back": {"battery": kitchen_start_battery, "charging": kitchen_start_charging},
        "dining_area": {"battery": inv_by_name.get("Dining Area", {}).get("batteryLevel"), "charging": dining_charging},
        "cash_register": {"battery": inv_by_name.get("Cash Register", {}).get("batteryLevel"), "charging": cash_charging},
    }
    print(f"Preflight power status: {json.dumps(power_status)}", flush=True)

    # Use stagger=True with startup_lock=True so paths are unrecorded until patched and attempts serialize cleanly
    go2rtc, mediamtx = prepare(directory, devices, stagger=True, startup_lock=True)
    env = dict(env, VICO_RX_TRACE="1")
    env.setdefault("VICO_REASSEMBLY_WINDOW_MS", "1200")
    env.setdefault("VICO_FRAME_STALL_MS", "1800")

    restream = recorder = None
    observations = {alias: [] for alias in CAMERAS}
    cache = {}
    index_task = None
    pool = ThreadPoolExecutor(max_workers=1)
    window_end = None
    media_started = None
    cutoff = None
    stopped = False

    # Startup order: Dining Area -> Cash Register -> Kitchen Back
    # Start each camera after the previous feed is advancing.
    startup_order = ["dining_area", "cash_register", "kitchen_back"]
    started_cam = {alias: False for alias in startup_order}
    first_frames = {alias: None for alias in startup_order}
    cam_start_times = {alias: None for alias in startup_order}

    try:
        restream = start_service(directory, "go2rtc", [go2rtc, "-config", "go2rtc.json"], env)
        wait_http(HTTP, "/api/streams", restream)
        print(f"Three-camera mixed-power test: http://127.0.0.1:{HTTP}/ · {WALL_LIMIT}s cutoff · "
              f"{DISK_LIMIT // 1024**2} MiB recording budget", flush=True)
        clock = time.time
        media_started = clock()
        started = time.monotonic()
        recorder = start_service(directory, "mediamtx", [mediamtx, "mediamtx.json"], env)
        wait_http(API, "/v3/paths/list", recorder)

        # Start Camera 1 (Dining Area)
        first = startup_order[0]
        request(API, f"/v3/config/paths/patch/{first}",
                {"source": f"rtsp://127.0.0.1:{RTSP}/{first}", "rtspTransport": "tcp", "record": True})
        started_cam[first] = True
        cam_start_times[first] = clock()
        print(f"Stage 1: Started recorder input for {CAMERAS[first]} at +0.0s.", flush=True)

        while recorder.poll() is None and restream.poll() is None:
            elapsed = time.monotonic() - started
            size = sum(row[0] for row in snapshot(directory).values())
            if size >= DISK_LIMIT - 3 * 4 * 1024**2:
                window_end = clock()
                cutoff = "disk budget"
                break
            if elapsed >= WALL_LIMIT:
                window_end = clock()
                cutoff = "wall limit"
                break

            now = clock()

            # Check if Camera 1 is advancing, then start Camera 2
            if started_cam[startup_order[0]] and not started_cam[startup_order[1]]:
                cam1 = startup_order[0]
                if first_frames[cam1] is None:
                    first_frames[cam1] = first_event_time(directory / f"{cam1}.bridge.log", "first_frame")
                    if first_frames[cam1] is not None:
                        print(f"Stage 1: {CAMERAS[cam1]} first frame at +{elapsed:.2f}s; advancing feed...", flush=True)
                # When camera 1 has reached first frame and advanced for 2.0s, start camera 2
                if (first_frames[cam1] is not None and (now - first_frames[cam1]) >= 2.0) or elapsed >= 20.0:
                    cam2 = startup_order[1]
                    request(API, f"/v3/config/paths/patch/{cam2}",
                            {"source": f"rtsp://127.0.0.1:{RTSP}/{cam2}", "rtspTransport": "tcp", "record": True})
                    started_cam[cam2] = True
                    cam_start_times[cam2] = now
                    print(f"Stage 2: Started recorder input for {CAMERAS[cam2]} at +{elapsed:.2f}s ({CAMERAS[cam1]} advancing).", flush=True)

            # Check if Camera 2 is advancing, then start Camera 3
            if started_cam[startup_order[1]] and not started_cam[startup_order[2]]:
                cam2 = startup_order[1]
                if first_frames[cam2] is None:
                    first_frames[cam2] = first_event_time(directory / f"{cam2}.bridge.log", "first_frame")
                    if first_frames[cam2] is not None:
                        print(f"Stage 2: {CAMERAS[cam2]} first frame at +{elapsed:.2f}s; advancing feed...", flush=True)
                # When camera 2 has reached first frame and advanced for 2.0s, start camera 3
                if (first_frames[cam2] is not None and (now - first_frames[cam2]) >= 2.0) or elapsed >= 40.0:
                    cam3 = startup_order[2]
                    request(API, f"/v3/config/paths/patch/{cam3}",
                            {"source": f"rtsp://127.0.0.1:{RTSP}/{cam3}", "rtspTransport": "tcp", "record": True})
                    started_cam[cam3] = True
                    cam_start_times[cam3] = now
                    print(f"Stage 3: Started recorder input for {CAMERAS[cam3]} at +{elapsed:.2f}s ({CAMERAS[cam2]} advancing).", flush=True)

            # Track camera 3's first frame
            if started_cam[startup_order[2]] and first_frames[startup_order[2]] is None:
                cam3 = startup_order[2]
                first_frames[cam3] = first_event_time(directory / f"{cam3}.bridge.log", "first_frame")
                if first_frames[cam3] is not None:
                    print(f"Stage 3: {CAMERAS[cam3]} first frame at +{elapsed:.2f}s; all three feeds advancing concurrently!", flush=True)

            if index_task is None or index_task.done():
                if index_task is not None:
                    index_task.result()
                index_task = pool.submit(saved_coverage, directory, cache)
            telemetry(directory, observations, clock, active_cameras=CAMERAS)
            time.sleep(0.25)
    finally:
        stop_at = time.time()
        window_end = window_end or stop_at
        if recorder is not None and recorder.poll() is None:
            try:
                toggle_recording(False, active_cameras=CAMERAS)
                time.sleep(0.5)
            except (URLError, TimeoutError):
                pass
        stop_service(recorder)
        stop_service(restream, tree=True)
        (directory / "start.lock").unlink(missing_ok=True)
        pool.shutdown(wait=True, cancel_futures=True)
        last = snapshot(directory)
        time.sleep(1)
        still = snapshot(directory)
        save_json(directory / "reader-telemetry.json", observations)
        stopped = last == still and all(proc is None or proc.poll() is not None for proc in (recorder, restream))
        print(f"Recorder and restream stopped; writes stable={stopped}; {len(still)} segments, "
              f"{sum(v[0] for v in still.values())} bytes; stop={cutoff or 'service exit'}", flush=True)

    if media_started is None:
        return False

    # Query ending battery status from account
    end_by_name = {}
    kitchen_end_battery = kitchen_end_charging = None
    dining_end_battery = dining_end_charging = None
    cash_end_battery = cash_end_charging = None
    try:
        env_query = dict(env, VICOHOME_CACHE_DIR=str(directory / "auth"))
        res = command([BRIDGE, "devices", "list", "--format", "json"], directory / "inventory_end.log", 20, env_query)
        if res.returncode == 0:
            end_devices = json.loads(res.stdout)
            end_by_name = {row["deviceName"]: row for row in end_devices}
            kitchen_end_battery = end_by_name.get("Kitchen Back", {}).get("batteryLevel")
            kitchen_end_charging = end_by_name.get("Kitchen Back", {}).get("isCharging")
            dining_end_battery = end_by_name.get("Dining Area", {}).get("batteryLevel")
            dining_end_charging = end_by_name.get("Dining Area", {}).get("isCharging")
            cash_end_battery = end_by_name.get("Cash Register", {}).get("batteryLevel")
            cash_end_charging = end_by_name.get("Cash Register", {}).get("isCharging")
            print(f"Ending status: Kitchen Back battery={kitchen_end_battery}% charging={kitchen_end_charging}; "
                  f"Dining Area battery={dining_end_battery}% charging={dining_end_charging}; "
                  f"Cash Register battery={cash_end_battery}% charging={cash_end_charging}", flush=True)
    except Exception as e:
        print(f"Warning: could not query ending battery: {e}", flush=True)

    battery_report = {
        "kitchen_back": {
            "starting_battery_percent": kitchen_start_battery,
            "starting_charging_flag": kitchen_start_charging,
            "ending_battery_percent": kitchen_end_battery,
            "ending_charging_flag": kitchen_end_charging,
        },
        "dining_area": {
            "starting_battery_percent": inv_by_name.get("Dining Area", {}).get("batteryLevel"),
            "starting_charging_flag": dining_charging,
            "ending_battery_percent": dining_end_battery,
            "ending_charging_flag": dining_end_charging,
        },
        "cash_register": {
            "starting_battery_percent": inv_by_name.get("Cash Register", {}).get("batteryLevel"),
            "starting_charging_flag": cash_charging,
            "ending_battery_percent": cash_end_battery,
            "ending_charging_flag": cash_end_charging,
        },
    }

    kitchen_effective_charging = kitchen_end_charging if kitchen_end_charging is not None else kitchen_start_charging
    is_kitchen_powered = str(kitchen_effective_charging) in ("1", "2") or str(kitchen_start_charging) in ("1", "2")
    is_dining_powered = str(dining_charging) in ("1", "2") or str(dining_end_charging) in ("1", "2")
    is_cash_powered = str(cash_charging) in ("1", "2") or str(cash_end_charging) in ("1", "2")

    if is_kitchen_powered and is_dining_powered and is_cash_powered:
        run_class_desc = "bounded three-camera acceptance test (all cameras on external power)"
        power_setup_desc = (f"all external power (Dining Area flag {dining_end_charging or dining_charging}, "
                            f"Cash Register flag {cash_end_charging or cash_charging}, "
                            f"Kitchen Back flag {kitchen_effective_charging})")
    else:
        run_class_desc = (f"bounded three-camera acceptance test (mixed-power: Kitchen flag {kitchen_effective_charging}, "
                          f"Dining flag {dining_end_charging or dining_charging}, "
                          f"Cash flag {cash_end_charging or cash_charging})")
        power_setup_desc = (f"mixed-power (Kitchen Back battery {kitchen_start_battery}->{kitchen_end_battery}% "
                            f"flag {kitchen_effective_charging}; "
                            f"Dining Area flag {dining_end_charging or dining_charging}; "
                            f"Cash Register flag {cash_end_charging or cash_charging})")

    def rel(t):
        return round(t - media_started, 3) if t is not None else None

    extra = {
        "run_class": run_class_desc,
        "power_setup": power_setup_desc,
        "battery_report": battery_report,
        "staggered_startup_order": startup_order,
        "first_frames_s_from_recorder_start": {alias: rel(first_frames[alias]) for alias in startup_order},
        "writes_stable": stopped,
        "cutoff": cutoff,
    }

    cam_windows = {alias: (first_frames[alias] or cam_start_times[alias] or media_started, window_end) for alias in startup_order}
    passed = summary(directory, observations, media_started, window_end, active_cameras=CAMERAS,
                     extra_report=extra, camera_windows=cam_windows)

    # Verify playback by time on recorded files:
    playback_check = {}
    cov_path = directory / "www/coverage.json"
    cov_data = json.loads(cov_path.read_text(encoding="utf-8")) if cov_path.exists() else {}
    for alias in CAMERAS:
        ranges = cov_data.get(alias, [])
        seg_files = files(directory, alias)
        playback_check[alias] = {
            "recorded_segments": len(seg_files),
            "playback_intervals": len(ranges),
            "total_coverage_ms": sum(r[1] - r[0] for r in ranges),
            "playable_by_time": len(ranges) > 0 and len(seg_files) > 0,
        }
    extra["playback_by_time"] = playback_check
    save_json(directory / "summary.json", json.loads((directory / "summary.json").read_text(encoding="utf-8")) | {"playback_by_time": playback_check})
    print(f"Playback by time verification: {json.dumps(playback_check, indent=2)}", flush=True)

    return passed and stopped and cutoff == "wall limit" and all(p["playable_by_time"] for p in playback_check.values())


def run_dining_solo(directory, devices, env):
    alias = "dining_area"
    rows = json.loads((directory / "inventory.json").read_text(encoding="utf-8"))
    dining = next(row for row in rows if row["deviceName"] == "Dining Area")
    if str(dining.get("isCharging")).lower() not in ("1", "2", "true"):
        raise RuntimeError("Dining did not report charging. No camera session started.")
    check_ports([HTTP, RTSP, PLAYBACK, API])
    go2rtc, mediamtx = prepare(directory, {alias: devices[alias]}, stagger=True)
    env["VICO_RX_TRACE"] = "1"
    restream = start_service(directory, "go2rtc", [go2rtc, "-config", "go2rtc.json"], env)
    wait_http(HTTP, "/api/streams", restream)
    recorder = start_service(directory, "mediamtx", [mediamtx, "mediamtx.json"], env)
    wait_http(API, "/v3/paths/list", recorder)
    print("Dining only · 60-second cutoff · 128 MiB budget", flush=True)
    observations = {alias: []}
    pool = ThreadPoolExecutor(max_workers=1)
    index = None
    cache = {}
    cutoff = None
    started, monotonic_start = time.time(), time.monotonic()
    request(API, f"/v3/config/paths/patch/{alias}", {
        "source": f"rtsp://127.0.0.1:{RTSP}/{alias}", "rtspTransport": "tcp", "record": True,
    })
    last_index = 0.0
    try:
        while recorder.poll() is None and restream.poll() is None:
            now = time.monotonic()
            if now - monotonic_start >= 60:
                cutoff = "wall limit"
                break
            if sum(row[0] for row in snapshot(directory).values()) >= DISK_LIMIT - 4 * 1024**2:
                cutoff = "disk budget"
                break
            if now - last_index >= 6 and (index is None or index.done()):
                if index is not None:
                    index.result()
                index = pool.submit(saved_coverage, directory, cache)
                last_index = now
            telemetry(directory, observations, time.time, active_cameras=(alias,))
            time.sleep(.25)
    finally:
        end = time.time()
        if recorder.poll() is None:
            try:
                toggle_recording(False, active_cameras=(alias,))
                time.sleep(.5)
            except (URLError, TimeoutError):
                pass
        stop_service(recorder)
        stop_service(restream, tree=True)
        (directory / "start.lock").unlink(missing_ok=True)
        pool.shutdown(wait=True, cancel_futures=True)
    files_list = snapshot(directory)
    time.sleep(1)
    stable = files_list == snapshot(directory)
    extras = {"writes_stable": stable, "cutoff": cutoff, "file_count": len(files_list),
              "saved_bytes": sum(row[0] for row in files_list.values())}
    save_json(directory / "reader-telemetry.json", observations)
    return summary(directory, observations, started, end, active_cameras=(alias,), extra_report=extras) and stable and cutoff == "wall limit"


def parse_utc_timestamp(text):
    if not text or text == "-":
        return None
    if text.endswith("Z"):
        match = re.fullmatch(r"(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d+))?Z", text)
        if match:
            frac = (match.group(2) or "").ljust(6, "0")[:6]
            return datetime.strptime(f"{match.group(1)}.{frac}", "%Y-%m-%dT%H:%M:%S.%f").replace(tzinfo=timezone.utc)
    try:
        return datetime.strptime(text, "%Y/%m/%d %H:%M:%S.%f").replace(tzinfo=timezone.utc)
    except ValueError:
        return None


class LiveCameraTracker:
    """Incrementally parses a continuous bridge health log and tracks live upstream state."""

    def __init__(self, alias, name, log_path, stream_profile="2k"):
        self.alias = alias
        self.name = name
        self.log_path = log_path
        self.stream_profile = stream_profile
        self.offset = 0
        self.activated_at = None
        self.attempt = 0
        self.ever_live = False
        self.attempt_live = False
        self.reconnects = 0
        self.wake_code = None
        self.answer_ok = False
        self.channel_ok = False
        self.start_ack = False
        self.ice = ""
        self.verified_resolution = ""
        self.stream_verified = False
        self.post_kbps = None
        self.inst_kbps = None
        self.recent_kbps_min = None
        self.recent_kbps_max = None
        self.prev_attempt_frames = 0
        self.attempt_frames = 0
        self.last_frame_utc = None
        self.first_frame_mono = None
        self.max_frame_gap_s = 0.0
        self.longest_intra_gap_s = 0.0
        self.last_progress_mono = None
        self.last_frame_mono = None
        self.in_gap = False
        self.gap_reason = ""
        self.last_error_reason = ""

    @property
    def total_frames(self):
        return self.prev_attempt_frames + self.attempt_frames

    def ingest(self):
        if not self.log_path.is_file():
            return
        with self.log_path.open("r", encoding="utf-8", errors="replace") as handle:
            handle.seek(self.offset)
            chunk = handle.read()
            self.offset = handle.tell()
        if not chunk:
            return
        now_mono = time.monotonic()
        for line in chunk.splitlines():
            parts = line.split()
            if len(parts) < 3:
                continue
            log_dt = parse_utc_timestamp(f"{parts[0]} {parts[1]}")
            fields = dict(item.split("=", 1) for item in parts[2:] if "=" in item)
            event = fields.get("event")
            if event == "attempt":
                if self.ever_live:
                    self.reconnects += 1
                self.prev_attempt_frames += self.attempt_frames
                self.attempt_frames = 0
                self.attempt = int(fields.get("attempt", self.attempt + 1))
                self.attempt_live = False
                self.answer_ok = False
                self.channel_ok = False
                self.start_ack = False
                self.in_gap = False
                self.last_progress_mono = now_mono
            elif event == "ticket":
                if "wake_code" in fields:
                    self.wake_code = int(fields["wake_code"])
            elif event == "answer_received":
                self.answer_ok = True
            elif event == "channel_open":
                self.channel_ok = True
            elif event == "start_live_ack":
                self.start_ack = True
            elif event == "ice_state":
                if fields.get("ice") and fields["ice"] != "/":
                    self.ice = fields["ice"]
            elif event == "stream_verified":
                self.stream_verified = True
                if fields.get("dim") and fields["dim"] != "-":
                    self.verified_resolution = fields["dim"]
            elif event == "first_frame":
                self.ever_live = True
                if self.first_frame_mono is None:
                    self.first_frame_mono = now_mono
                self.attempt_live = True
                self.in_gap = False
                self.last_error_reason = ""
                if fields.get("dim") and fields["dim"] != "-":
                    self.verified_resolution = fields["dim"]
                if fields.get("ice") and fields["ice"] != "/":
                    self.ice = fields["ice"]
                if log_dt is not None:
                    self._record_frame_time(log_dt, now_mono)
            elif event == "video_gap":
                self.in_gap = True
                self.gap_reason = fields.get("reason", "gap")
            elif event == "video_recovered":
                self.in_gap = False
                self.gap_reason = ""
                if "gap_ms" in fields:
                    self.longest_intra_gap_s = max(self.longest_intra_gap_s, int(fields["gap_ms"]) / 1000.0)
                    self.max_frame_gap_s = max(self.max_frame_gap_s, self.longest_intra_gap_s)
            elif event in ("output_stall", "peer_lost", "ice_failed", "ice_disconnected", "startup_timeout"):
                self.last_error_reason = event
                self.attempt_live = False
            elif event == "progress":
                self.last_progress_mono = now_mono
                self.answer_ok = fields.get("answer") == "true"
                self.channel_ok = fields.get("channel") == "true"
                self.start_ack = fields.get("ack") == "true"
                if fields.get("dim") and fields["dim"] != "-":
                    self.verified_resolution = fields["dim"]
                for key, attr in (("post_kbps", "post_kbps"), ("inst_kbps", "inst_kbps")):
                    if key in fields:
                        try:
                            val = float(fields[key])
                        except ValueError:
                            val = 0.0
                        if val > 0:
                            setattr(self, attr, round(val, 1))
                            if key == "inst_kbps" and self.attempt_live:
                                self.recent_kbps_min = val if self.recent_kbps_min is None else min(self.recent_kbps_min, val)
                                self.recent_kbps_max = val if self.recent_kbps_max is None else max(self.recent_kbps_max, val)
                frames = int(fields.get("frames", "0"))
                if frames > self.attempt_frames:
                    self.attempt_frames = frames
                if "max_frame_gap_ms" in fields:
                    intra = int(fields["max_frame_gap_ms"]) / 1000.0
                    self.longest_intra_gap_s = max(self.longest_intra_gap_s, intra)
                    self.max_frame_gap_s = max(self.max_frame_gap_s, intra)
                frame_dt = parse_utc_timestamp(fields.get("last_frame_utc", ""))
                if frame_dt is not None:
                    self._record_frame_time(frame_dt, now_mono)
                    if self.attempt_live is False and frames > 0:
                        self.ever_live = True
                        if self.first_frame_mono is None:
                            self.first_frame_mono = now_mono
                        self.attempt_live = True

    def _record_frame_time(self, frame_dt, now_mono):
        if self.last_frame_utc is not None and frame_dt > self.last_frame_utc:
            wall_gap = (frame_dt - self.last_frame_utc).total_seconds()
            self.max_frame_gap_s = max(self.max_frame_gap_s, wall_gap)
        if self.last_frame_utc is None or frame_dt > self.last_frame_utc:
            self.last_frame_utc = frame_dt
            self.last_frame_mono = now_mono

    def current_open_gap_s(self, now_utc):
        if self.last_frame_utc is None:
            return 0.0
        return max(0.0, (now_utc - self.last_frame_utc).total_seconds())

    def status_dict(self, now_utc, now_mono, mtx_item=None, go_item=None):
        consumers = len((go_item or {}).get("consumers") or [])
        mtx_ready = bool((mtx_item or {}).get("ready"))
        age_s = self.current_open_gap_s(now_utc) if self.last_frame_utc is not None else None
        last_iso = self.last_frame_utc.strftime("%Y-%m-%dT%H:%M:%SZ") if self.last_frame_utc else None
        gap_max = round(max(self.max_frame_gap_s, age_s or 0.0) if self.ever_live else self.max_frame_gap_s, 3)

        res_str = self.verified_resolution or ("640x360" if self.stream_profile == "360p" else "2304x1296")
        if self.activated_at is None:
            state, label, stage, detail = "starting", "Queued", "queued", "Waiting for previous camera to start..."
        elif not self.attempt_live:
            stage = ("waiting_keyframe" if self.start_ack else
                     "ice_checking" if self.answer_ok else
                     "signaling" if self.attempt > 0 else "waking")
            label_map = {
                "waiting_keyframe": "Waiting keyframe",
                "ice_checking": "Connecting ICE",
                "signaling": "Signaling",
                "waking": "Waking",
            }
            if self.ever_live or self.attempt > 1:
                state = "recovering"
                label = f"Recovering ({self.attempt})" if self.attempt > 1 else "Recovering"
                detail = f"Stage: {label_map[stage]} · Reconnects {self.reconnects}"
            else:
                state = "starting"
                label = label_map[stage]
                detail = f"Attempt {max(1, self.attempt)}: {label}..."
        elif self.in_gap or (age_s is not None and age_s > 3.5):
            state, label, stage = "stalled", "Stalled", "stalled"
            detail = f"Waiting for keyframe ({round(age_s or 0.0, 1)}s gap)"
        else:
            state, label, stage = "live", "Live", "streaming"
            kbps_note = f" · {round(self.post_kbps)} kbps" if self.post_kbps else ""
            detail = f"{res_str.replace('x', '×')}{kbps_note} · Frames {self.total_frames}"

        return {
            "id": self.alias,
            "name": self.name,
            "profile": self.stream_profile,
            "streamVerified": self.stream_verified,
            "state": state,
            "label": label,
            "stage": stage,
            "detail": detail,
            "attempt": self.attempt,
            "reconnects": self.reconnects,
            "frames": self.total_frames,
            "lastFrameUtc": last_iso,
            "ageMs": round((age_s or 0.0) * 1000) if age_s is not None else None,
            "gapS": round(age_s or 0.0, 2) if age_s is not None else None,
            "longestGapS": gap_max,
            "resolution": res_str if self.ever_live else None,
            "bitrateKbps": self.post_kbps,
            "recentBitrateKbps": self.inst_kbps,
            "ice": self.ice or None,
            "mediamtxReady": mtx_ready,
            "consumers": consumers,
        }


def run_live_preview(directory, devices, env, minutes=0.0, stream_profile="2k"):
    """Start feeds sequentially and keep independent upstream sessions live beyond the short recording."""
    if shutil.disk_usage(directory).free < 2 * DISK_LIMIT:
        raise RuntimeError("Not enough disk space for the live preview's short recording.")
    check_ports([HTTP, RTSP, PLAYBACK, API])
    go2rtc, mediamtx = prepare(
        directory, devices, stagger=True, continuous_timeout=35,
        startup_lock=True, read_timeout_s=120, stream_profile=stream_profile,
    )
    env = dict(env)
    env.setdefault("VICO_RX_TRACE", "1")
    env.setdefault("VICO_REASSEMBLY_WINDOW_MS", "1200")
    env.setdefault("VICO_FRAME_STALL_MS", "1800")
    sup_log = directory / "live-supervisor.log"
    restream = recorder = None
    active = []
    recording_cameras = set()
    saved_durations = {alias: 0.0 for alias in CAMERAS}
    cache = {}
    archive_open = True
    final_index_needed = False
    trackers = {
        alias: LiveCameraTracker(alias, name, directory / f"{alias}.bridge.log", stream_profile=stream_profile)
        for alias, name in CAMERAS.items()
    }
    print(f"Live stream profile: {'640x360 low-bandwidth trial' if stream_profile == '360p' else '2304x1296 2K'}", flush=True)
    start_battery = {}
    inv_path = directory / "inventory.json"
    if inv_path.is_file():
        inv_rows = json.loads(inv_path.read_text(encoding="utf-8"))
        by_name = {row["deviceName"]: row for row in inv_rows}
        for alias, name in CAMERAS.items():
            if name in by_name:
                start_battery[alias] = {
                    "batteryLevel": by_name[name].get("batteryLevel"),
                    "isCharging": by_name[name].get("isCharging"),
                }

    def note_supervisor(msg):
        ts = datetime.now(timezone.utc).strftime("%Y/%m/%d %H:%M:%S")
        with sup_log.open("a", encoding="utf-8") as fh:
            fh.write(f"{ts} {msg}\n")

    def poll_services():
        go_streams, mtx_paths = {}, {}
        try:
            raw = request(HTTP, "/api/streams")
            if raw:
                go_streams = json.loads(raw.decode("utf-8"))
        except (URLError, TimeoutError, ValueError, OSError):
            pass
        try:
            raw = request(API, "/v3/paths/list")
            if raw:
                mtx_data = json.loads(raw.decode("utf-8"))
                for item in mtx_data.get("items") or []:
                    mtx_paths[item.get("name")] = item
        except (URLError, TimeoutError, ValueError, OSError):
            pass
        return go_streams, mtx_paths

    try:
        restream = start_service(directory, "go2rtc", [go2rtc, "-config", "go2rtc.json"], env)
        wait_http(HTTP, "/", restream)
        recorder = start_service(directory, "mediamtx", [mediamtx, "mediamtx.json"], env)
        wait_http(API, "/v3/paths/list", recorder)
        started = time.monotonic()
        started_utc = datetime.now(timezone.utc)
        pending = iter(CAMERAS)
        next_camera = next(pending, None)
        last_start = started
        next_index = started
        next_status = started
        next_watchdog = started + 5.0
        last_recovery_action = 0.0
        unhealthy_since = {}
        short_ended_s = None
        last_frame_settle_mono = None
        with ThreadPoolExecutor(max_workers=1) as pool:
            index = None
            while restream.poll() is None and recorder.poll() is None:
                now = time.monotonic()
                now_utc = datetime.now(timezone.utc)
                for tracker in trackers.values():
                    tracker.ingest()
                if minutes > 0 and now >= started + minutes * 60:
                    break
                disk_bytes = sum(row[0] for row in snapshot(directory).values())
                if archive_open and (now >= started + 240.0 or disk_bytes >= DISK_LIMIT - 3 * 4 * 1024**2 or (next_camera is None and not recording_cameras and active)):
                    for alias in sorted(recording_cameras):
                        # Keep RTSP source pulling continuously with recording disabled so upstream survives browser closes.
                        request(API, f"/v3/config/paths/patch/{alias}", {"record": False})
                    recording_cameras.clear()
                    archive_open = False
                    final_index_needed = True
                    short_ended_s = round(now - started, 2)
                    next_index = now + 1.5
                    note_supervisor(f"short_recording_ended elapsed_s={short_ended_s} disk_bytes={disk_bytes}")
                    print("Short recording ended. Persistent upstream live viewing and playback remain active.", flush=True)
                if next_camera is not None:
                    prev_alias = active[-1] if active else None
                    prev_ready = False
                    if prev_alias is None:
                        prev_ready = True
                    elif trackers[prev_alias].ever_live:
                        if last_frame_settle_mono is None:
                            last_frame_settle_mono = now
                        prev_ready = (now - last_frame_settle_mono) >= 1.5
                    elif now - last_start >= 38.0:
                        prev_ready = True
                    if prev_ready:
                        rec_flag = archive_open
                        request(API, f"/v3/config/paths/patch/{next_camera}", {
                            "source": f"rtsp://127.0.0.1:{RTSP}/{next_camera}", "rtspTransport": "tcp",
                            "record": rec_flag,
                        })
                        if rec_flag:
                            recording_cameras.add(next_camera)
                        trackers[next_camera].activated_at = now
                        active.append(next_camera)
                        last_start = now
                        last_frame_settle_mono = None
                        note_supervisor(f"started_camera alias={next_camera}")
                        print(f"Started {CAMERAS[next_camera]}.", flush=True)
                        next_camera = next(pending, None)
                        if next_camera is None:
                            print(f"Open the store dashboard: http://127.0.0.1:{HTTP}/ (Ctrl+C to stop).", flush=True)
                go_streams = mtx_paths = None
                if now >= next_status or now >= next_watchdog:
                    go_streams, mtx_paths = poll_services()
                if now >= next_watchdog and active:
                    next_watchdog = now + 2.0
                    for alias in active:
                        tr = trackers[alias]
                        mtx_item = (mtx_paths or {}).get(alias) or {}
                        has_rtsp_source = bool((mtx_item.get("source") or {}).get("type"))
                        ready = bool(mtx_item.get("ready"))
                        if not has_rtsp_source:
                            if now - last_recovery_action >= 5.0:
                                request(API, f"/v3/config/paths/patch/{alias}", {
                                    "source": f"rtsp://127.0.0.1:{RTSP}/{alias}", "rtspTransport": "tcp",
                                    "record": alias in recording_cameras,
                                })
                                last_recovery_action = now
                                note_supervisor(f"restored_mediamtx_source alias={alias}")
                            continue
                        prog_stale = tr.last_progress_mono is None or (now - tr.last_progress_mono) > 15.0
                        frame_stale = tr.last_frame_mono is None or (now - tr.last_frame_mono) > 25.0
                        if (not ready or frame_stale) and prog_stale and now - tr.activated_at > 40.0:
                            unhealthy_since.setdefault(alias, now)
                            if now - unhealthy_since[alias] >= 10.0 and now - last_recovery_action >= 12.0:
                                request(API, f"/v3/config/paths/patch/{alias}", {"source": "publisher", "record": False})
                                time.sleep(0.5)
                                request(API, f"/v3/config/paths/patch/{alias}", {
                                    "source": f"rtsp://127.0.0.1:{RTSP}/{alias}", "rtspTransport": "tcp",
                                    "record": alias in recording_cameras,
                                })
                                last_recovery_action = now
                                unhealthy_since.pop(alias, None)
                                note_supervisor(f"recycled_stalled_mediamtx_path alias={alias}")
                        else:
                            unhealthy_since.pop(alias, None)
                if now >= next_status:
                    next_status = now + 1.0
                    status_payload = {
                        "mode": "live-low-bandwidth" if stream_profile == "360p" else "live",
                        "streamProfile": stream_profile,
                        "startedUtc": started_utc.strftime("%Y-%m-%dT%H:%M:%SZ"),
                        "updatedUtc": now_utc.strftime("%Y-%m-%dT%H:%M:%SZ"),
                        "recordingActive": archive_open,
                        "diskBytes": disk_bytes,
                        "cameras": {
                            alias: trackers[alias].status_dict(
                                now_utc, now, (mtx_paths or {}).get(alias), (go_streams or {}).get(alias)
                            )
                            for alias in CAMERAS
                        },
                    }
                    save_json(directory / "www/status.json", status_payload)
                if index is not None and index.done():
                    cov = index.result()
                    index = None
                    for alias, row in cov.items():
                        dur = sum(end - begin for begin, end in row.get("ranges") or ())
                        saved_durations[alias] = round(dur, 2)
                    shared_span = shared_recording_span(cov)
                    if archive_open and shared_span >= 25.0:
                        for alias in sorted(recording_cameras):
                            request(API, f"/v3/config/paths/patch/{alias}", {"record": False})
                        recording_cameras.clear()
                        note_supervisor(f"short_recording_common_complete shared_s={shared_span:.3f}")
                if now >= next_index and index is None:
                    index = pool.submit(saved_coverage, directory, cache)
                    if archive_open:
                        next_index = now + 3.0
                    else:
                        final_index_needed = False
                        next_index = float("inf")
                time.sleep(.25)
        stopped_unexpectedly = restream.poll() is not None or recorder.poll() is not None
        return not stopped_unexpectedly and all(tr.ever_live for tr in trackers.values())
    except KeyboardInterrupt:
        return True
    finally:
        for tracker in trackers.values():
            tracker.ingest()
        stopped_unexpectedly = (restream is not None and restream.poll() is not None) or (recorder is not None and recorder.poll() is not None)
        if recorder is not None and recorder.poll() is None and archive_open:
            try:
                toggle_recording(False, active_cameras=active)
            except (URLError, TimeoutError):
                pass
        stop_service(recorder)
        stop_service(restream, tree=True)
        (directory / "start.lock").unlink(missing_ok=True)
        final_cov = saved_coverage(directory, cache)
        for alias, row in final_cov.items():
            saved_durations[alias] = round(sum(end - begin for begin, end in row.get("ranges") or ()), 2)
        ended_utc = datetime.now(timezone.utc)
        first_snap = snapshot(directory)
        time.sleep(1.0)
        second_snap = snapshot(directory)
        writes_stable = first_snap == second_snap
        total_disk_bytes = sum(row[0] for row in second_snap.values())
        if "started" in locals():
            summary_data = {
                "stream_profile": stream_profile,
                "started_utc": started_utc.strftime("%Y-%m-%dT%H:%M:%SZ"),
                "ended_utc": ended_utc.strftime("%Y-%m-%dT%H:%M:%SZ"),
                "duration_s": round(time.monotonic() - started, 2),
                "short_recording_ended_at_s": short_ended_s,
                "common_overlap_s": round(shared_recording_span(final_cov), 3),
                "disk_bytes": total_disk_bytes,
                "disk_within_budget": total_disk_bytes <= DISK_LIMIT,
                "writes_stable": writes_stable,
                "services_healthy_at_exit": not stopped_unexpectedly,
                "start_battery": start_battery,
                "cameras": {
                    alias: {
                        "ever_live": tr.ever_live,
                        "stream_verified": tr.stream_verified,
                        "verified_resolution": tr.verified_resolution or None,
                        "post_start_kbps": tr.post_kbps,
                        "recent_kbps_min": round(tr.recent_kbps_min, 1) if tr.recent_kbps_min is not None else None,
                        "recent_kbps_max": round(tr.recent_kbps_max, 1) if tr.recent_kbps_max is not None else None,
                        "startup_s": round(tr.first_frame_mono - tr.activated_at, 3) if (tr.first_frame_mono is not None and tr.activated_at is not None) else None,
                        "total_frames": tr.total_frames,
                        "attempts": tr.attempt,
                        "reconnects": tr.reconnects,
                        "max_completed_gap_s": round(tr.max_frame_gap_s, 3),
                        "cutoff_open_gap_s": round(tr.current_open_gap_s(ended_utc) if tr.ever_live else 0.0, 3),
                        "longest_gap_s": round(max(tr.max_frame_gap_s, tr.current_open_gap_s(ended_utc) if tr.ever_live else 0.0), 3),
                        "within_2s_gap_gate": tr.ever_live and max(tr.max_frame_gap_s, tr.current_open_gap_s(ended_utc)) <= 2.0,
                        "longest_intra_gap_s": round(tr.longest_intra_gap_s, 3),
                        "saved_duration_s": saved_durations.get(alias, 0.0),
                        "last_frame_utc": tr.last_frame_utc.strftime("%Y-%m-%dT%H:%M:%SZ") if tr.last_frame_utc else None,
                        "ice": tr.ice or None,
                        "wake_code": tr.wake_code,
                        "start_ack": tr.start_ack,
                        "answer_ok": tr.answer_ok,
                        "channel_ok": tr.channel_ok,
                    }
                    for alias, tr in trackers.items()
                },
            }
            save_json(directory / "live-summary.json", summary_data)
            print(f"Live session ended: duration={summary_data['duration_s']}s disk={total_disk_bytes}B writes_stable={writes_stable}", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("preflight", "kitchen-ab", "two-camera", "powered-pair", "dining-solo", "receiver-comparison", "three-camera", "live", "round", "review"))
    parser.add_argument("number", nargs="?", type=int, choices=(1, 2))
    parser.add_argument("--minutes", type=float, default=0.0, help="bounded live duration in minutes (0 runs until Ctrl+C)")
    parser.add_argument("--low-bandwidth", action="store_true", help="opt-in 640x360 low-bandwidth Overview trial mode for live action")
    args = parser.parse_args()
    if (args.action in ("round", "review")) != (args.number is not None):
        parser.error("round/review require a round number (1 or 2)")
    if args.low_bandwidth and args.action != "live":
        parser.error("--low-bandwidth is only supported with live")
    env = os.environ.copy()
    if args.action == "review":
        return 0 if review(RUNTIME / f"round-{args.number}") else 1
    if args.action == "preflight":
        tag = "preflight-" + datetime.now().strftime("%Y%m%d-%H%M%S")
    elif args.action == "kitchen-ab":
        tag = "kitchen-ab-" + datetime.now().strftime("%Y%m%d-%H%M%S")
    elif args.action == "two-camera":
        tag = "two-camera-" + datetime.now().strftime("%Y%m%d-%H%M%S")
    elif args.action == "powered-pair":
        tag = "powered-pair-" + datetime.now().strftime("%Y%m%d-%H%M%S")
    elif args.action == "dining-solo":
        tag = "dining-solo-" + datetime.now().strftime("%Y%m%d-%H%M%S")
    elif args.action == "receiver-comparison":
        tag = "receiver-comparison-" + datetime.now().strftime("%Y%m%d-%H%M%S")
    elif args.action == "three-camera":
        tag = "three-camera-" + datetime.now().strftime("%Y%m%d-%H%M%S")
    elif args.action == "live":
        tag = "live-" + datetime.now().strftime("%Y%m%d-%H%M%S")
    else:
        tag = f"round-{args.number}"
    directory = RUNTIME / tag
    directory.mkdir(parents=True, exist_ok=False)
    print(f"Private evidence: local/store-dashboard/{tag}", flush=True)
    try:
        devices = authenticated_inventory(directory, env)
        if args.action == "preflight":
            passed = preflight(directory, devices, env)
        elif args.action == "kitchen-ab":
            passed = kitchen_ab(directory, devices, env)
        elif args.action == "two-camera":
            passed = run_two_camera(directory, devices, env)
        elif args.action == "powered-pair":
            passed = run_powered_pair(directory, devices, env)
        elif args.action == "dining-solo":
            passed = run_dining_solo(directory, devices, env)
        elif args.action == "receiver-comparison":
            passed = run_receiver_comparison(directory, devices, env)
        elif args.action == "three-camera":
            passed = run_three_camera(directory, devices, env)
        elif args.action == "live":
            passed = run_live_preview(
                directory, devices, env, minutes=args.minutes,
                stream_profile=("360p" if args.low_bandwidth else "2k"),
            )
        else:
            passed = run_round(directory, devices, env, stagger=args.number == 2)
        return 0 if passed else 1
    except (RuntimeError, OSError, subprocess.TimeoutExpired, ValueError) as error:
        # Exceptions can contain private command lines or response details.
        (directory / "error.log").write_text(repr(error), encoding="utf-8")
        print("Store command stopped. Details are in the private evidence directory.", flush=True)
        return 1
    finally:
        (directory / "auth/auth.json").unlink(missing_ok=True)
        for arm in ("arm1-bridge", "arm2-vicostream"):
            (directory / arm / "auth/auth.json").unlink(missing_ok=True)


if __name__ == "__main__":
    raise SystemExit(main())
