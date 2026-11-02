#!/usr/bin/env python3
"""Raw Apple `container run -d` start throughput vs client concurrency.
Isolates the runtime from kvz. Prints one JSON line per concurrency level."""
import json, os, subprocess, sys, threading, time, platform

N = int(sys.argv[1]) if len(sys.argv) > 1 else 16
LEVELS = [int(x) for x in (sys.argv[2] if len(sys.argv) > 2 else "1,2,4,8,16").split(",")]
GUI = os.path.expanduser("~/kvz-tools/gui.sh")
WRAP = [GUI] if os.path.exists(GUI) else []
IMG = "docker.io/library/nginx:alpine"


def run(cmd):
    return subprocess.run(WRAP + cmd, capture_output=True, text=True)


for c in LEVELS:
    names = [f"scal-{c}-{i}" for i in range(N)]
    lat, lock, idx = [], threading.Lock(), [0]
    t0 = time.time()

    def worker():
        while True:
            with lock:
                if idx[0] >= N:
                    return
                i = idx[0]
                idx[0] += 1
            s = time.time()
            run(["container", "run", "-d", "--rm", "--progress", "none", "--name", names[i], IMG])
            with lock:
                lat.append(time.time() - s)
    ts = [threading.Thread(target=worker) for _ in range(c)]
    [t.start() for t in ts]
    [t.join() for t in ts]
    wall = time.time() - t0
    run(["container", "rm", "-f"] + names)
    lat.sort()
    print(json.dumps({"suite": "container-scaling", "host": platform.node().split(".")[0].lower(), "n": N, "concurrency": c,
                      "wall_s": round(wall, 2), "starts_per_s": round(N / wall, 2),
                      "p50_s": round(lat[len(lat) // 2], 3), "max_s": round(lat[-1], 3)}), flush=True)
    time.sleep(3)
