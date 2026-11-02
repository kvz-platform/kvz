#!/usr/bin/env python3
"""Benchmark orchestrator: runs the same suite against one platform on this host.

  bench/run.py --platform kvz|lima-k3s|apple-cluster [--out results/<host>] [--quick]

Platforms
  kvz            this repo: native control plane + Apple container / Vz / process pods
  lima-k3s       k3s inside a Lima (Virtualization.framework) Linux VM, containerd pods
  apple-cluster  Apple `container cluster` (kind node image inside an Apple container VM)

Every record is one JSON line tagged with host + platform. Memory is measured as
macOS phys_footprint (footprint(1)) summed over the platform's processes, which
for VM-based platforms includes guest memory actually touched.
"""
import argparse, json, os, platform, re, shutil, signal, subprocess, sys, tempfile, threading, time

HOME = os.path.expanduser("~")
REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
GUI = os.path.join(HOME, "kvz-tools", "gui.sh")
WRAP = [GUI] if os.path.exists(GUI) else []  # minis: run inside console launchd context
BIN = lambda n: next((p for p in [os.path.join(REPO, "bin", n), os.path.join(HOME, "kvz-tools", n)] if os.path.exists(p)), n)
CONTAINER = shutil.which("container") or "/usr/local/bin/container"
LIMACTL = shutil.which("limactl") or "/opt/homebrew/bin/limactl"
KUBECTL = shutil.which("kubectl") or "/opt/homebrew/bin/kubectl"
VZ = "com.apple.Virtualization.VirtualMachine"
CONTAINER_DAEMONS = r"container-apiserver|container-core-images|container-network-vmnet|container-runtime-linux"

OUT = None
HOST = platform.node().split(".")[0].lower()
PLAT = None


def log(*a):
    print(f"[{time.strftime('%H:%M:%S')}] [{HOST}/{PLAT}]", *a, file=sys.stderr, flush=True)


def emit(rec):
    rec.update(host=HOST, platform=PLAT, ts=time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()))
    line = json.dumps(rec)
    print(line, flush=True)
    with open(OUT, "a") as f:
        f.write(line + "\n")


def sh(cmd, timeout=600, check=False, wrap=False, env=None):
    if isinstance(cmd, str):
        cmd = ["/bin/sh", "-c", cmd]
    if wrap:
        cmd = WRAP + cmd
    r = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout, env=env)
    if check and r.returncode != 0:
        raise RuntimeError(f"{cmd}: {r.returncode}: {r.stderr[-2000:]}")
    return r


# ---------------------------------------------------------------- measurement

def ps_table():
    out = sh(["ps", "-axo", "pid=,time=,rss=,command="]).stdout
    rows = []
    for l in out.splitlines():
        parts = l.split(None, 3)
        if len(parts) == 4:
            rows.append((int(parts[0]), parts[1], int(parts[2]), parts[3]))
    return rows


def cpu_seconds(t):
    # ps time: [[dd-]hh:]mm:ss.cc
    days = 0
    if "-" in t:
        d, t = t.split("-", 1)
        days = int(d)
    parts = [float(x) for x in t.split(":")]
    s = 0.0
    for p in parts:
        s = s * 60 + p
    return s + days * 86400


class Procs:
    """Tracks the platform's process set: name regex + VZ VMs started after baseline."""

    def __init__(self, regex):
        self.regex = re.compile(regex) if regex else None
        self.baseline_vz = {pid for pid, _, _, cmd in ps_table() if VZ in cmd}

    def pids(self):
        out = []
        for pid, t, rss, cmd in ps_table():
            if VZ in cmd and pid not in self.baseline_vz:
                out.append(pid)
            elif self.regex and self.regex.search(cmd) and "run.py" not in cmd and "kvzbench" not in cmd:
                out.append(pid)
        return out

    def footprint(self):
        pids = self.pids()
        if not pids:
            return {"footprint_mb": 0, "rss_mb": 0, "nproc": 0}
        with tempfile.NamedTemporaryFile(suffix=".json", delete=False) as f:
            path = f.name
        sh(["footprint", "-f", "bytes", "-j", path] + [str(p) for p in pids], timeout=120)
        fp = 0
        try:
            d = json.load(open(path))
            fp = sum(p.get("footprint", 0) for p in d.get("processes", []))
        except Exception:
            pass
        os.unlink(path)
        rss = sum(r for pid, _, r, _ in ps_table() if pid in set(pids))
        return {"footprint_mb": round(fp / 2**20, 1), "rss_mb": round(rss / 1024, 1), "nproc": len(pids)}

    def cpu(self, seconds=30):
        def snap():
            ps = set(self.pids())
            return {pid: cpu_seconds(t) for pid, t, _, _ in ps_table() if pid in ps}
        a, t0 = snap(), time.time()
        time.sleep(seconds)
        b, t1 = snap(), time.time()
        used = sum(b[p] - a.get(p, 0) for p in b)
        return round(100 * used / (t1 - t0), 2)


def kubectl(kc, *args, timeout=60):
    return sh([KUBECTL, "--kubeconfig", kc, "--request-timeout=5s"] + list(args), timeout=timeout)


def wait_api(kc, deadline):
    while time.time() < deadline:
        if kubectl(kc, "get", "--raw", "/readyz").returncode == 0:
            return True
        time.sleep(0.1)
    return False


def wait_nodes_ready(kc, deadline, n=1):
    while time.time() < deadline:
        r = kubectl(kc, "get", "nodes", "-o", "json")
        if r.returncode == 0:
            try:
                items = json.loads(r.stdout)["items"]
                ready = [i for i in items if any(c["type"] == "Ready" and c["status"] == "True" for c in i["status"]["conditions"])]
                if len(ready) >= n:
                    return True
            except Exception:
                pass
        time.sleep(0.2)
    return False


def wait_default_sa(kc, deadline):
    while time.time() < deadline:
        if kubectl(kc, "get", "sa", "default", "-n", "default").returncode == 0:
            return True
        time.sleep(0.2)
    return False


# ---------------------------------------------------------------- platforms

class KVZ:
    name = "kvz"
    data = os.path.join(HOME, ".kvz-bench")
    regex = r"kvz server|" + CONTAINER_DAEMONS

    def __init__(self):
        self.kc = os.path.join(self.data, "kubeconfig.yaml")
        self.proc = None

    def reset(self):
        self.stop()
        shutil.rmtree(self.data, ignore_errors=True)

    def start(self):
        logf = open(os.path.join(tempfile.gettempdir(), "kvz-bench-server.log"), "a")
        cmd = WRAP + [BIN("kvz"), "server", "--data-dir", self.data]
        self.proc = subprocess.Popen(cmd, stdout=logf, stderr=logf, start_new_session=True)

    def stop(self):
        sh(["pkill", "-INT", "-f", "kvz server --data-dir " + self.data])
        for _ in range(100):
            if sh(["pgrep", "-f", "kvz server --data-dir " + self.data]).returncode != 0:
                break
            time.sleep(0.1)
        sh(["pkill", "-9", "-f", "kvz server --data-dir " + self.data])
        # remove any leftover pod VMs
        ids = sh([CONTAINER, "ls", "-aq"], wrap=True).stdout.split()
        kv = [i for i in ids if i.startswith("kvz-")]
        if kv:
            sh([CONTAINER, "rm", "-f"] + kv, wrap=True, timeout=300)


class LimaK3s:
    name = "lima-k3s"
    regex = r"limactl hostagent|limactl.*k3s"

    def __init__(self):
        self.kc = os.path.join(HOME, ".lima", "k3s", "copied-from-guest", "kubeconfig.yaml")

    def reset(self):
        self.stop()

    def start(self):
        self.proc = subprocess.Popen([LIMACTL, "start", "--tty=false", "k3s"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

    def stop(self):
        sh([LIMACTL, "stop", "k3s"], timeout=300)


class AppleCluster:
    name = "apple-cluster"
    regex = CONTAINER_DAEMONS

    def __init__(self, cpus, mem):
        self.kc = os.path.join(tempfile.gettempdir(), "apple-cluster-bench.kubeconfig")
        self.cpus, self.mem = cpus, mem

    def reset(self):
        self.stop()

    def start(self):
        def run():
            sh([CONTAINER, "cluster", "create", "--name", "bench", "--cpus", str(self.cpus), "--memory", self.mem], wrap=True, timeout=900)
            sh([CONTAINER, "cluster", "write-config", "--name", "bench", "--kubeconfig", self.kc], wrap=True)
        try:
            os.unlink(self.kc)
        except FileNotFoundError:
            pass
        self.t = threading.Thread(target=run, daemon=True)
        self.t.start()

    def stop(self):
        sh([CONTAINER, "cluster", "delete", "--name", "bench"], wrap=True, timeout=300)


# ---------------------------------------------------------------- suites

def bench(kc, *args, timeout=1800):
    r = sh([BIN("kvzbench")] + list(args) + ["--kubeconfig", kc, "--label", f"{HOST}/{PLAT}"], timeout=timeout)
    recs = []
    for l in r.stdout.splitlines():
        try:
            recs.append(json.loads(l))
        except Exception:
            pass
    if r.returncode != 0:
        log("kvzbench failed:", r.stderr[-800:])
    return recs


def timed_start(p, kind):
    t0 = time.time()
    p.start()
    deadline = t0 + 900
    while not os.path.exists(p.kc) and time.time() < deadline:
        time.sleep(0.1)
    ok_api = wait_api(p.kc, deadline)
    t_api = time.time() - t0
    ok_node = wait_nodes_ready(p.kc, deadline)
    t_node = time.time() - t0
    wait_default_sa(p.kc, deadline)
    t_sa = time.time() - t0
    emit({"suite": "startup", "kind": kind, "api_ready_s": round(t_api, 3), "node_ready_s": round(t_node, 3),
          "workload_ready_s": round(t_sa, 3), "ok": ok_api and ok_node})
    log(f"{kind} start: api {t_api:.2f}s node {t_node:.2f}s sa {t_sa:.2f}s")


def pod_burst(p, procs, n, c, extra, label, hold=20):
    ready_file = os.path.join(tempfile.gettempdir(), f"kvzbench-ready-{os.getpid()}")
    try:
        os.unlink(ready_file)
    except FileNotFoundError:
        pass
    result = {}

    def runb():
        result["recs"] = bench(p.kc, "pods", "--n", str(n), "--c", str(c), "--hold", f"{hold}s", "--ready-file", ready_file, "--timeout", "15m", *extra)
    th = threading.Thread(target=runb)
    th.start()
    fp = None
    while th.is_alive():
        if os.path.exists(ready_file):
            time.sleep(3)
            fp = procs.footprint()
            break
        time.sleep(0.2)
    th.join()
    for r in result.get("recs", []):
        r.update(suite="pods", variant=label)
        if r.get("phase") == "startup" and fp:
            r["footprint_loaded"] = fp
        emit(r)


def pod_seq(p, reps, extra, label):
    lat = []
    for i in range(reps):
        for r in bench(p.kc, "pods", "--n", "1", "--c", "1", "--timeout", "10m", *extra):
            if r.get("phase") == "startup" and r["ready"]["n"] == 1:
                lat.append(r["ready"]["p50_ms"])
            if r.get("phase") == "teardown":
                lat_td = r.get("all_gone_wall_s")
    lat.sort()
    if lat:
        emit({"suite": "pods-seq", "variant": label, "reps": len(lat), "ready_ms": lat,
              "p50_ms": lat[len(lat) // 2], "min_ms": lat[0], "max_ms": lat[-1]})
        log(f"{label} sequential ready p50 {lat[len(lat)//2]:.0f}ms (min {lat[0]:.0f}, max {lat[-1]:.0f})")


def main():
    global OUT, PLAT
    ap = argparse.ArgumentParser()
    ap.add_argument("--platform", required=True)
    ap.add_argument("--out", default=os.path.join(REPO, "results", HOST))
    ap.add_argument("--quick", action="store_true")
    ap.add_argument("--vm-cpus", type=int, default=8)
    ap.add_argument("--vm-mem", default="12G")
    ap.add_argument("--macos-mem", default="4Gi")
    ap.add_argument("--macos-image", default="tahoe-base")
    ap.add_argument("--skip", default="", help="comma list of suites to skip")
    a = ap.parse_args()
    PLAT = a.platform
    os.makedirs(a.out, exist_ok=True)
    OUT = os.path.join(a.out, f"{PLAT}.jsonl")
    skip = set(a.skip.split(",")) if a.skip else set()
    p = {"kvz": KVZ, "lima-k3s": LimaK3s, "apple-cluster": lambda: AppleCluster(a.vm_cpus, a.vm_mem)}[PLAT]()

    hw = sh("sysctl -n machdep.cpu.brand_string hw.ncpu hw.memsize; sw_vers -productVersion").stdout.split("\n")
    emit({"suite": "host", "cpu": hw[0], "ncpu": int(hw[1]), "mem_gb": round(int(hw[2]) / 2**30), "macos": hw[3]})

    p.reset()
    procs = Procs(p.regex)
    timed_start(p, "first")
    if PLAT == "kvz" or PLAT == "lima-k3s":
        p.stop()
        time.sleep(3)
        timed_start(p, "restart")

    if "idle" not in skip:
        time.sleep(20)
        cpu = procs.cpu(30)
        fp = procs.footprint()
        emit({"suite": "idle", "cpu_pct": cpu, **fp})
        log(f"idle cpu {cpu}% footprint {fp}")

    if "api" not in skip:
        for rep in range(1 if a.quick else 2):
            for r in bench(p.kc, "api", "--n", "2000", "--c", "32"):
                r.update(suite="api", rep=rep)
                emit(r)

    # linux pods (warm the image first)
    nginx = ["--image", "docker.io/library/nginx:alpine"]
    if PLAT == "kvz":
        nginx += ["--runtime-class", "linux"]
    bench(p.kc, "pods", "--n", "1", "--c", "1", "--timeout", "10m", *nginx)
    if "linux" not in skip:
        pod_seq(p, 5 if a.quick else 10, nginx, "linux-nginx")
        pod_burst(p, procs, 20, 20, nginx, "linux-nginx-20")
        if not a.quick:
            pod_burst(p, procs, 50, 50, nginx, "linux-nginx-50", hold=15)

    if PLAT == "kvz" and "darwin" not in skip:
        dw = ["--runtime-class", "darwin", "--command", "sleep 3600"]
        pod_seq(p, 10, dw, "darwin-sleep")
        pod_burst(p, procs, 200, 50, dw, "darwin-sleep-200", hold=10)
        if not a.quick:
            pod_burst(p, procs, 500, 100, dw, "darwin-sleep-500", hold=10)

    if PLAT == "kvz" and "macos" not in skip:
        mac = ["--runtime-class", "macos", "--image", a.macos_image, "--cpu", "2", "--memory", a.macos_mem]
        pod_seq(p, 3, mac, "macos-vm")
        pod_burst(p, procs, 2, 2, mac, "macos-vm-2", hold=30)

    p.stop()
    log("done")


if __name__ == "__main__":
    main()
