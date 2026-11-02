#!/usr/bin/env python3
"""Pod-to-pod TCP throughput (iperf3) on any cluster.

  bench/net.py --kubeconfig KC --label studio/kvz [--runtime-class linux]
Emits JSON lines: {"suite":"net", "streams":N, "gbps":...}.
"""
import argparse, json, subprocess, time, sys

ap = argparse.ArgumentParser()
ap.add_argument("--kubeconfig", required=True)
ap.add_argument("--label", default="")
ap.add_argument("--runtime-class", default="")
ap.add_argument("--seconds", type=int, default=5)
a = ap.parse_args()
K = ["kubectl", "--kubeconfig", a.kubeconfig, "-n", "default"]


def pod(name, cmd):
    spec = {"apiVersion": "v1", "kind": "Pod", "metadata": {"name": name, "labels": {"app": "kvznet"}},
            "spec": {"terminationGracePeriodSeconds": 1,
                     "containers": [{"name": "c", "image": "docker.io/library/alpine:3.18", "command": ["sh", "-c", cmd],
                                     "resources": {"limits": {"cpu": "2", "memory": "512Mi"}}}]}}
    if a.runtime_class:
        spec["spec"]["runtimeClassName"] = a.runtime_class
    for i in range(60):
        r = subprocess.run(K + ["apply", "-f", "-"], input=json.dumps(spec), text=True, capture_output=True)
        if r.returncode == 0:
            return
        time.sleep(1)
    raise SystemExit("apply failed: " + r.stderr)


subprocess.run(K + ["delete", "pod", "-l", "app=kvznet", "--wait=true"], capture_output=True)
setup = "apk add -q iperf3 >/dev/null 2>&1 || (sleep 2; apk add -q iperf3)"
pod("iperf-server", setup + " && iperf3 -s")
pod("iperf-client", setup + " && sleep 3600")
subprocess.run(K + ["wait", "--for=condition=Ready", "pod/iperf-server", "pod/iperf-client", "--timeout=180s"], check=True, capture_output=True)
ip = subprocess.run(K + ["get", "pod", "iperf-server", "-o", "jsonpath={.status.podIP}"], capture_output=True, text=True).stdout
# wait until iperf3 is installed on both sides
for _ in range(120):
    ok = subprocess.run(K + ["exec", "iperf-client", "--", "sh", "-c", "command -v iperf3"], capture_output=True).returncode == 0
    if ok and subprocess.run(K + ["exec", "iperf-client", "--", "iperf3", "-c", ip, "-t", "1"], capture_output=True).returncode == 0:
        break
    time.sleep(1)
time.sleep(1)
for streams in (1, 4):
    r = subprocess.run(K + ["exec", "iperf-client", "--", "iperf3", "-c", ip, "-t", str(a.seconds), "-P", str(streams), "-J"], capture_output=True, text=True)
    try:
        d = json.loads(r.stdout)
        bps = d["end"]["sum_received"]["bits_per_second"]
        out = {"suite": "net", "label": a.label, "streams": streams, "gbps": round(bps / 1e9, 2)}
    except Exception as e:
        err = ""
        try:
            err = json.loads(r.stdout).get("error", "")
        except Exception:
            err = r.stdout[-300:]
        out = {"suite": "net", "label": a.label, "streams": streams, "error": str(e), "iperf": err, "stderr": r.stderr[-300:]}
    print(json.dumps(out), flush=True)
r = subprocess.run(K + ["exec", "iperf-client", "--", "sh", "-c", "ping -c 20 -i 0.05 " + ip + " | tail -1"], capture_output=True, text=True)
print(json.dumps({"suite": "net", "label": a.label, "ping": r.stdout.strip()}), flush=True)
subprocess.run(K + ["delete", "pod", "-l", "app=kvznet", "--wait=false"], capture_output=True)
