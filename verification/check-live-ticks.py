#!/usr/bin/env python3
import json
import math
import os
import struct
import subprocess
import sys
import time

container = sys.argv[1]
token = os.environ["TICK_TEST_TOKEN"]

def docker(*args):
    return subprocess.check_output(["docker", *args], timeout=10, text=True)

pid = int(docker("inspect", container, "--format", "{{.State.Pid}}"))
memory = os.open(f"/proc/{pid}/mem", os.O_RDONLY)

def frame():
    return struct.unpack("<Q", os.pread(memory, 8, 0xdd49ea8))[0]

def request(endpoint, authenticated=True):
    args = ["exec", "-e", "LD_PRELOAD=", container, "curl", "-sS", "--max-time", "3"]
    if authenticated:
        args += ["-H", f"Authorization: Bearer {token}"]
    return docker(*args, "-w", "\n%{http_code}", f"http://127.0.0.1:8080/api/{endpoint}").rsplit("\n", 1)

assert request("metrics", False)[1] == "401", "Metrics endpoint must require authentication"
observations = []
first_count, started = frame(), time.monotonic()
for i in range(13):
    body, status = request("metrics")
    assert status == "200", (status, body)
    payload = json.loads(body)
    tick = payload["tick"]
    assert payload["schemaVersion"] == 1 and tick["status"] == "ready", tick
    assert tick["source"] == "engine_tick_hook" and tick["scope"] == "UDomGameEngine::Tick", tick
    rate, window, count = tick["rateHz"], tick["windowSeconds"], tick["sampleCount"]
    assert all(math.isfinite(x) and x > 0 for x in [rate, window, count]), tick
    assert math.isclose(rate, count / window, rel_tol=1e-8), tick
    durations = [tick["executionMs"][key] for key in ["p50", "p95", "p99"]]
    assert all(math.isfinite(x) and x >= 0 for x in durations) and durations == sorted(durations), tick
    assert tick["lastSampleAgeSeconds"] <= 2, tick
    assert abs(time.time() - tick["observedAtUnixMs"] / 1000) < 3, tick
    observations.append({"elapsedSeconds":time.monotonic() - started, "nativeFrameCounter":frame(), "payload":payload})
    if i < 12:
        time.sleep(5)
elapsed, last_count = time.monotonic() - started, frame()
health, health_status = request("health")
players, players_status = request("players")
assert health_status == players_status == "200"
assert json.loads(health)["engineReady"] is True
assert json.loads(players)["count"] == 0
burst_started, burst_count = time.monotonic(), frame()
for _ in range(30):
    body, status = request("metrics")
    assert status == "200" and json.loads(body)["tick"]["status"] == "ready"
burst_elapsed = time.monotonic() - burst_started
burst_frames = frame() - burst_count
os.close(memory)
report = json.dumps({"container":container,"scope":"offline empty-world actual server", "unauthenticatedStatus":401,
    "nativeCounterRateHz":(last_count-first_count)/elapsed, "nativeCounterDelta":last_count-first_count,
    "elapsedSeconds":elapsed, "health":json.loads(health), "players":json.loads(players),
    "burstRequests":30,"burstElapsedSeconds":burst_elapsed,"burstNativeFrames":burst_frames,
    "observations":observations}, indent=2)
subprocess.run([sys.executable, os.path.join(os.path.dirname(__file__), "check-tick-evidence.py")], input=report, text=True, check=True)
print(report)
