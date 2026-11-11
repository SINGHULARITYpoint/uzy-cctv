#!/usr/bin/env python3
"""Capture and verify bounded private samples from the VPS loopback RTSP service."""

import argparse
import json
import os
from pathlib import Path
import subprocess
import threading
import time


CAMERAS = ("dining_area", "cash_register", "kitchen_back")
MAX_GAP = 2.0


def capture(directory, cameras, minimum):
    directory.mkdir(mode=0o700)
    started = time.monotonic()
    started_unix_s = time.time()
    readers = {}

    def watch_progress(process, observations):
        last_pts = -1
        for line in process.stdout:
            key, _, value = line.strip().partition("=")
            if key == "out_time_us" and value != "N/A" and int(value) > last_pts:
                last_pts = int(value)
                observations.append((round(time.monotonic() - started, 3), last_pts))

    try:
        for camera in cameras:
            with (directory / f"{camera}.reader.log").open("w") as log:
                process = subprocess.Popen([
                    "ffmpeg", "-hide_banner", "-v", "warning", "-nostats",
                    "-stats_period", "0.25", "-progress", "pipe:1", "-rtsp_transport", "tcp",
                    "-timeout", "48000000", "-analyzeduration", "1000000", "-probesize", "524288",
                    "-i", f"rtsp://127.0.0.1:8554/{camera}", "-map", "0:v:0", "-c:v", "copy",
                    str(directory / f"{camera}.mp4"),
                ], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=log, text=True)
            observations = []
            thread = threading.Thread(target=watch_progress, args=(process, observations))
            readers[camera] = process, observations, thread
            thread.start()

        deadline = started + 72
        while time.monotonic() < deadline:
            active = [(process, times) for process, times, _ in readers.values() if process.poll() is None]
            if not active:
                break
            if all(times for _, times in active):
                latest_start = max(times[0][0] for _, times in active)
                if time.monotonic() - started >= latest_start + minimum + 3:
                    break
            time.sleep(0.1)
    finally:
        for process, _, _ in readers.values():
            if process.poll() is None:
                try:
                    process.stdin.write("q\n")
                    process.stdin.flush()
                except BrokenPipeError:
                    pass
        for process, _, thread in readers.values():
            try:
                process.wait(timeout=3)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
            thread.join()
            process.stdin.close()
            process.stdout.close()

    results = {}
    overlap_starts = []
    overlap_ends = []
    for camera, (process, observations, _) in readers.items():
        path = directory / f"{camera}.mp4"
        with (directory / f"{camera}.progress.json").open("w") as file:
            json.dump(observations, file)
        result = inspect_sample(path)
        times = [elapsed for elapsed, _ in observations]
        reader_gap = max((b - a for a, b in zip(times, times[1:])), default=0)
        result.update(reader_exit=process.returncode, startup_s=times[0] if times else None,
                      reader_span_s=round(times[-1] - times[0], 3) if times else 0,
                      max_reader_gap_s=round(reader_gap, 3))
        if times:
            overlap_starts.append(times[0])
            overlap_ends.append(times[-1])
        result["pass"] = (result["resolution"] == [2304, 1296] and result["decode_exit"] == 0
                          and result["decode_errors"] == 0 and result["duration_s"] >= minimum
                          and result["max_pts_gap_s"] <= MAX_GAP and reader_gap <= MAX_GAP
                          and result["reader_span_s"] >= minimum and process.returncode == 0)
        results[camera] = result

    overlap = max(0, min(overlap_ends) - max(overlap_starts)) if len(overlap_starts) == len(cameras) else 0
    summary = {"started_unix_s": started_unix_s, "cameras": results, "overlap_s": round(overlap, 3),
               "pass": overlap >= minimum and all(result["pass"] for result in results.values())}
    (directory / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
    print(directory.name, json.dumps(summary), flush=True)
    return summary["pass"]


def inspect_sample(path):
    probe = subprocess.run([
        "ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries",
        "stream=width,height,duration,nb_frames:packet=pts_time", "-of", "json", str(path),
    ], capture_output=True, text=True, timeout=20)
    data = json.loads(probe.stdout)
    streams = data.get("streams", [])
    pts = sorted(float(packet["pts_time"]) for packet in data.get("packets", []) if "pts_time" in packet)
    stream = streams[0] if streams else {}
    decode_log = path.with_suffix(".decode.log")
    with decode_log.open("w") as log:
        decode = subprocess.run([
            "ffmpeg", "-nostdin", "-v", "error", "-xerror", "-err_detect", "explode", "-i", str(path),
            "-map", "0:v:0", "-vf", "settb=1/90000,setpts=N", "-enc_time_base", "1:90000",
            "-fps_mode", "passthrough", "-f", "rawvideo", "-y", "/dev/null",
        ], stderr=log, timeout=30)
    return {"resolution": [stream["width"], stream["height"]] if stream else None,
            "duration_s": float(stream.get("duration", 0)), "frames": int(stream.get("nb_frames", 0)),
            "max_pts_gap_s": round(max((b - a for a, b in zip(pts, pts[1:])), default=0), 3),
            "decode_exit": decode.returncode, "decode_errors": len(decode_log.read_text().splitlines())}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("directory", type=Path)
    parser.add_argument("--solo-first", action="store_true")
    args = parser.parse_args()
    os.umask(0o077)
    if args.solo_first:
        for camera in CAMERAS:
            capture(args.directory / f"solo-{camera}", (camera,), 8)
            time.sleep(3)
    return 0 if capture(args.directory / "overlap", CAMERAS, 20) else 1


if __name__ == "__main__":
    raise SystemExit(main())
