//go:build e2e

package e2e

import (
	"os"
	"strings"
	"testing"
)

// A Linux pod boots its own VM, becomes Ready, and runs exec.
func TestLinuxPod(t *testing.T) {
	apply(t, `
apiVersion: v1
kind: Pod
metadata: {name: e2e-linux}
spec:
  containers:
    - name: web
      image: nginx:alpine
`)
	must(t, "wait", "--for=condition=Ready", "pod/e2e-linux", "--timeout=120s")
	out := must(t, "exec", "e2e-linux", "--", "cat", "/etc/os-release")
	if !strings.Contains(out, "Alpine") {
		t.Fatalf("exec output did not look like Alpine:\n%s", out)
	}
	if ip := must(t, "get", "pod", "e2e-linux", "-o", "jsonpath={.status.podIP}"); ip == "" {
		t.Fatal("pod has no IP")
	}
}

// A native pod runs a host process; its output reaches kubectl logs.
func TestDarwinPod(t *testing.T) {
	apply(t, `
apiVersion: v1
kind: Pod
metadata: {name: e2e-darwin}
spec:
  runtimeClassName: darwin
  restartPolicy: Never
  containers:
    - name: main
      image: host
      command: ["/usr/bin/sw_vers"]
`)
	must(t, "wait", "--for=jsonpath={.status.phase}=Succeeded", "pod/e2e-darwin", "--timeout=60s")
	if out := must(t, "logs", "e2e-darwin"); !strings.Contains(out, "ProductVersion") {
		t.Fatalf("logs did not contain sw_vers output:\n%s", out)
	}
}

// A Service is reachable by its cluster DNS name from another pod.
func TestServiceDNS(t *testing.T) {
	apply(t, `
apiVersion: apps/v1
kind: Deployment
metadata: {name: e2e-web}
spec:
  replicas: 2
  selector: {matchLabels: {app: e2e-web}}
  template:
    metadata: {labels: {app: e2e-web}}
    spec:
      containers: [{name: web, image: nginx:alpine, ports: [{containerPort: 80}]}]
---
apiVersion: v1
kind: Service
metadata: {name: e2e-web}
spec:
  selector: {app: e2e-web}
  ports: [{port: 80}]
`)
	must(t, "rollout", "status", "deployment/e2e-web", "--timeout=120s")
	apply(t, `
apiVersion: batch/v1
kind: Job
metadata: {name: e2e-curl}
spec:
  backoffLimit: 3
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: curl
          image: alpine:3.22
          command: ["wget", "-qO-", "http://e2e-web.default.svc.cluster.local"]
`)
	must(t, "wait", "--for=condition=Complete", "job/e2e-curl", "--timeout=120s")
	if out := must(t, "logs", "job/e2e-curl"); !strings.Contains(out, "nginx") {
		t.Fatalf("service response did not come from nginx:\n%s", out)
	}
}

// A macOS VM pod boots and answers exec. Slow (about 20 s) and needs an
// image, so it only runs when KVZ_E2E_MACOS_IMAGE is set.
func TestMacOSPod(t *testing.T) {
	image := os.Getenv("KVZ_E2E_MACOS_IMAGE")
	if image == "" {
		t.Skip("set KVZ_E2E_MACOS_IMAGE to run the macOS VM test")
	}
	apply(t, `
apiVersion: v1
kind: Pod
metadata: {name: e2e-macos}
spec:
  runtimeClassName: macos
  containers: [{name: vm, image: "`+image+`"}]
`)
	must(t, "wait", "--for=condition=Ready", "pod/e2e-macos", "--timeout=300s")
	if out := must(t, "exec", "e2e-macos", "--", "sw_vers"); !strings.Contains(out, "ProductName") {
		t.Fatalf("unexpected sw_vers output:\n%s", out)
	}
}
