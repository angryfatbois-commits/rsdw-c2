#!/usr/bin/env python3
import copy
import json
import math
import sys

def validate(capture):
    assert capture["nativeCounterDelta"] > 0, "Native frame counter did not advance"
    observations = capture["observations"]
    assert len(observations) >= 3, "Need overlapping native and API windows"
    for index in range(2, len(observations)):
        first, last = observations[index - 2], observations[index]
        elapsed = last["elapsedSeconds"] - first["elapsedSeconds"]
        frames = last["nativeFrameCounter"] - first["nativeFrameCounter"]
        tick = last["payload"]["tick"]
        assert elapsed > 0 and frames > 0, "Native frame counter did not advance"
        assert abs(elapsed - tick["windowSeconds"]) < 1, "Observation windows do not align"
        assert math.isclose(frames / elapsed, tick["rateHz"], rel_tol=0.10, abs_tol=1), "Native and API tick rates disagree"
    assert capture["burstNativeFrames"] > 0, "Native ticks stopped during HTTP burst"
    burst_rate = capture["burstNativeFrames"] / capture["burstElapsedSeconds"]
    assert math.isclose(burst_rate, observations[-1]["payload"]["tick"]["rateHz"], rel_tol=0.10, abs_tol=2), "HTTP burst changed tick cadence"

if __name__ == "__main__":
    with open(sys.argv[1]) if len(sys.argv) > 1 else sys.stdin as source:
        capture = json.load(source)
    validate(capture)
    for mode in ("stuck", "wrong-rate"):
        broken = copy.deepcopy(capture)
        for item in broken["observations"]:
            if mode == "stuck":
                item["nativeFrameCounter"] = 1
            else:
                item["payload"]["tick"]["rateHz"] *= 2
        try:
            validate(broken)
        except AssertionError:
            continue
        raise AssertionError("Evidence gate accepted " + mode)
    print("PASS aligned native/API windows, burst cadence, and negative evidence checks", file=sys.stderr)
