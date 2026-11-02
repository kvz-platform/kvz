import sys, os, time, subprocess
sys.path.insert(0, os.path.expanduser("~/kvz-tools/bench"))
import run
run.PLAT = sys.argv[1]
host = run.HOST
p = {"kvz": run.KVZ, "lima-k3s": run.LimaK3s, "apple-cluster": lambda: run.AppleCluster(8, "12G")}[sys.argv[1]]()
p.reset(); p.start()
deadline = time.time() + 600
while not os.path.exists(p.kc) and time.time() < deadline: time.sleep(0.5)
run.wait_api(p.kc, deadline); run.wait_nodes_ready(p.kc, deadline); run.wait_default_sa(p.kc, deadline)
out = subprocess.run(["python3", os.path.expanduser("~/kvz-tools/bench/net.py"), "--kubeconfig", p.kc, "--label", f"{host}/{sys.argv[1]}"], capture_output=True, text=True)
print(out.stdout, out.stderr[-500:])
open(os.path.expanduser(f"~/kvz-tools/results/{host}/net-{sys.argv[1]}.jsonl"), "w").write(out.stdout)
p.stop()
