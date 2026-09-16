#!/usr/bin/env python3
"""Restart only the named disposable test container and check producer warmup/reset."""
import json
import os
import subprocess
import sys
import time

container = sys.argv[1]
assert container.startswith("rsdw-tick-"), "Only disposable tick-test containers are allowed"
token = os.environ["TICK_TEST_TOKEN"]

def request(endpoint):
    command = ["docker", "exec", "-e", "LD_PRELOAD=", container, "curl", "-fsS", "--max-time", "2",
               "-H", f"Authorization: Bearer {token}", f"http://127.0.0.1:8080/api/{endpoint}"]
    result = subprocess.run(command, capture_output=True, text=True, timeout=5)
    return json.loads(result.stdout) if result.returncode == 0 else None

before = request("health")
assert before and before["uptimeSeconds"] > 20
subprocess.run(["docker", "restart", "-t", "10", container], check=True, capture_output=True, timeout=20)
started = time.monotonic()
observations = []
while time.monotonic() - started < 30:
    payload = request("metrics")
    if payload:
        observations.append({"elapsedSeconds":time.monotonic()-started, "payload":payload})
        if payload["tick"]["status"] == "ready":
            break
    time.sleep(0.1)
assert observations and observations[-1]["payload"]["tick"]["status"] == "ready", observations
unready = [item["payload"]["tick"] for item in observations if item["payload"]["tick"]["status"] != "ready"]
assert unready, "Warmup was not observed"
assert all(item["rateHz"] is None and all(value is None for value in item["executionMs"].values()) for item in unready)
after = request("health")
assert after and after["uptimeSeconds"] < before["uptimeSeconds"] and after["uptimeSeconds"] < 30
print(json.dumps({"before":before,"after":after,"observations":observations}, indent=2))
