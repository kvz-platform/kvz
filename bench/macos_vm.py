#!/usr/bin/env python3
"""macOS VM benchmarks:
  1. boot-to-SSH: kvz macos pod vs `tart run` (same image, same CPU/mem)
  2. guest vs host: CPU (LibreSSL speed, 1 thread + N threads), disk (dd write+fsync, read)

  bench/macos_vm.py --kubeconfig ~/.kvz/kubeconfig.yaml [--image tahoe-base] [--reps 3]
Requires a running kvz server with the macos runtime and tart installed.
"""
import argparse, json, os, platform, re, subprocess, time, socket

HOST = platform.node().split(".")[0].lower()
NODE = HOST
TART = os.path.expanduser("~/kvz-tools/tart.app/Contents/MacOS/tart")
if not os.path.exists(TART):
    TART = "tart"

PROBE = r'''
set -e
N=$(sysctl -n hw.ncpu)
st=$(openssl speed -elapsed -seconds 5 sha256 2>/dev/null | awk '/^sha256/{print $NF}')
mt=$(openssl speed -elapsed -seconds 5 -multi $N sha256 2>/dev/null | awk '/^sha256/{print $NF}')
f=${TMPDIR:-/tmp}/kvzdd.$$
t0=$(perl -MTime::HiRes=time -e 'print time')
dd if=/dev/zero of=$f bs=1m count=2048 2>/dev/null; sync
t1=$(perl -MTime::HiRes=time -e 'print time')
sudo -n purge 2>/dev/null || true
t2=$(perl -MTime::HiRes=time -e 'print time')
dd if=$f of=/dev/null bs=1m 2>/dev/null
t3=$(perl -MTime::HiRes=time -e 'print time')
rm -f $f
echo "{\"ncpu\":$N,\"sha256_16k_1t\":\"$st\",\"sha256_16k_mt\":\"$mt\",\"dd_write_s\":$(perl -e "print $t1-$t0"),\"dd_read_s\":$(perl -e "print $t3-$t2")}"
'''


def emit(r):
    r.update(host=HOST, ts=time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()))
    print(json.dumps(r), flush=True)


def ssh_ready(ip, timeout=300):
    t0 = time.time()
    while time.time() - t0 < timeout:
        try:
            s = socket.create_connection((ip, 22), timeout=1)
            banner = s.recv(32)
            s.close()
            if banner.startswith(b"SSH-"):
                return True
        except OSError:
            pass
        time.sleep(0.1)
    return False


def tart_boot(image, cpus, mem_mb):
    name = f"kvzbench-{int(time.time())}"
    t0 = time.time()
    subprocess.run([TART, "clone", image, name], check=True)
    t_clone = time.time() - t0
    subprocess.run([TART, "set", name, "--cpu", str(cpus), "--memory", str(mem_mb)], check=True)
    t1 = time.time()
    p = subprocess.Popen([TART, "run", "--no-graphics", name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    ip = subprocess.run([TART, "ip", "--wait", "180", name], capture_output=True, text=True).stdout.strip()
    t_ip = time.time() - t1
    ok = ssh_ready(ip)
    t_ssh = time.time() - t1
    subprocess.run([TART, "stop", name], capture_output=True)
    p.wait(timeout=60)
    subprocess.run([TART, "delete", name], capture_output=True)
    return {"clone_s": round(t_clone, 3), "ip_s": round(t_ip, 2), "ssh_s": round(t_ssh, 2), "ok": ok}


def kvz_boot(kc, image, cpus, mem):
    name = f"macbench-{int(time.time())}"
    manifest = {"apiVersion": "v1", "kind": "Pod", "metadata": {"name": name, "namespace": "default"},
                "spec": {"runtimeClassName": "macos", "terminationGracePeriodSeconds": 5, "nodeName": NODE,
                         "containers": [{"name": "vm", "image": image, "resources": {"limits": {"cpu": str(cpus), "memory": mem}}}]}}
    t0 = time.time()
    subprocess.run(["kubectl", "--kubeconfig", kc, "apply", "-f", "-"], input=json.dumps(manifest), text=True, check=True, capture_output=True)
    subprocess.run(["kubectl", "--kubeconfig", kc, "wait", "--for=condition=Ready", f"pod/{name}", "--timeout=300s"], check=True, capture_output=True)
    t = time.time() - t0
    return name, round(t, 2)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--kubeconfig", default=os.path.expanduser("~/.kvz/kubeconfig.yaml"))
    ap.add_argument("--image", default="tahoe-base")
    ap.add_argument("--cpus", type=int, default=4)
    ap.add_argument("--mem", default="8Gi")
    ap.add_argument("--reps", type=int, default=3)
    ap.add_argument("--skip-tart", action="store_true")
    ap.add_argument("--node", default=HOST)
    a = ap.parse_args()
    global NODE
    NODE = a.node
    mem_mb = int(re.sub(r"Gi$", "", a.mem)) * 1024
    kc = a.kubeconfig

    for i in range(a.reps):
        name, t = kvz_boot(kc, a.image, a.cpus, a.mem)
        emit({"suite": "macos-boot", "platform": "kvz", "rep": i, "apply_to_ready_s": t})
        if i == 0:
            out = subprocess.run(["kubectl", "--kubeconfig", kc, "exec", name, "--", "/bin/sh", "-c", PROBE], capture_output=True, text=True)
            try:
                emit({"suite": "macos-perf", "where": f"guest-{a.cpus}vcpu", **json.loads(out.stdout.strip().splitlines()[-1])})
            except Exception as e:
                emit({"suite": "macos-perf", "where": "guest", "error": str(e), "stderr": out.stderr[-500:]})
        subprocess.run(["kubectl", "--kubeconfig", kc, "delete", "pod", name, "--wait=true"], capture_output=True)
        time.sleep(3)
    if not a.skip_tart:
        for i in range(a.reps):
            r = tart_boot(os.path.expanduser("~/.tart/vms/" + a.image) if False else a.image, a.cpus, mem_mb)
            emit({"suite": "macos-boot", "platform": "tart", "rep": i, **r})
            time.sleep(3)
    out = subprocess.run(["/bin/sh", "-c", PROBE], capture_output=True, text=True)
    emit({"suite": "macos-perf", "where": "host", **json.loads(out.stdout.strip().splitlines()[-1])})


if __name__ == "__main__":
    main()
