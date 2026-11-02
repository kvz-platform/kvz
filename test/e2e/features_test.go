//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// eventually retries f until it returns "" (success) or the timeout passes.
func eventually(t *testing.T, timeout time.Duration, f func() string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	msg := ""
	for time.Now().Before(deadline) {
		if msg = f(); msg == "" {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal(msg)
}

func podLogs(t *testing.T, pod string) string {
	t.Helper()
	var out string
	eventually(t, 90*time.Second, func() string {
		phase, _ := kubectl("get", "pod", pod, "-o", "jsonpath={.status.phase}")
		if phase != "Succeeded" && phase != "Failed" {
			return pod + " did not finish (phase " + phase + ")"
		}
		out = must(t, "logs", pod)
		return ""
	})
	return out
}

// Security context: capabilities like containerd, users/groups, read-only root.
func TestSecurityContext(t *testing.T) {
	apply(t, `
apiVersion: v1
kind: Pod
metadata: {name: e2e-secctx}
spec:
  restartPolicy: Never
  securityContext: {runAsUser: 1234, runAsGroup: 2345, supplementalGroups: [3456]}
  containers:
    - name: c
      image: busybox:1.36
      securityContext: {readOnlyRootFilesystem: true, allowPrivilegeEscalation: false, capabilities: {add: [NET_ADMIN]}}
      command: [sh, -c, 'id; grep -E "CapBnd|CapEff|NoNewPrivs" /proc/self/status; touch /x 2>&1; true']
`)
	out := podLogs(t, "e2e-secctx")
	for _, want := range []string{"uid=1234", "gid=2345", "groups=3456", "CapEff:\t0000000000000000",
		"CapBnd:\t00000000a80435fb", "NoNewPrivs:\t1", "Read-only file system"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// Termination messages, sysctls, memory-backed emptyDir, $(VAR) expansion.
func TestPodRuntimeFeatures(t *testing.T) {
	apply(t, `
apiVersion: v1
kind: Pod
metadata: {name: e2e-runtime}
spec:
  restartPolicy: Never
  securityContext: {sysctls: [{name: kernel.shm_rmid_forced, value: "1"}]}
  volumes: [{name: mem, emptyDir: {medium: Memory, sizeLimit: 32Mi}}]
  containers:
    - name: c
      image: busybox:1.36
      env: [{name: GREETING, value: expanded}]
      command: [sh, -c, 'echo "arg=$(GREETING)"; sysctl -n kernel.shm_rmid_forced; mount | grep " /mem "; echo -n bye > /dev/termination-log; exit 3']
      volumeMounts: [{name: mem, mountPath: /mem}]
`)
	out := podLogs(t, "e2e-runtime")
	for _, want := range []string{"arg=expanded", "\n1\n", "tmpfs on /mem type tmpfs"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	msg := must(t, "get", "pod", "e2e-runtime", "-o", "jsonpath={.status.containerStatuses[0].state.terminated.message}")
	code := must(t, "get", "pod", "e2e-runtime", "-o", "jsonpath={.status.containerStatuses[0].state.terminated.exitCode}")
	if msg != "bye" || code != "3" {
		t.Errorf("termination: message %q exit %s, want \"bye\" 3", msg, code)
	}
}

// The apiserver reaches pods through the egress proxy (pods/proxy, which is
// also how webhooks and aggregated APIs in pods are called).
func TestAPIServerToPod(t *testing.T) {
	apply(t, `
apiVersion: v1
kind: Pod
metadata: {name: e2e-proxy}
spec:
  containers:
    - name: web
      image: nginx:alpine
`)
	must(t, "wait", "--for=condition=Ready", "pod/e2e-proxy", "--timeout=120s")
	// Ready means started; nginx may need a moment to listen
	eventually(t, 30*time.Second, func() string {
		out, err := kubectl("get", "--raw", "/api/v1/namespaces/default/pods/e2e-proxy:80/proxy/")
		if err != nil || !strings.Contains(out, "Welcome to nginx") {
			return "pods/proxy: " + out
		}
		return ""
	})
}

// Label/annotation changes stick (status updates once reverted them) and
// downwardAPI volumes follow them.
func TestPodMetadataUpdates(t *testing.T) {
	apply(t, `
apiVersion: v1
kind: Pod
metadata: {name: e2e-meta, labels: {stage: one}}
spec:
  containers:
    - name: c
      image: busybox:1.36
      command: [sh, -c, 'while true; do cat /podinfo/labels; echo; sleep 1; done']
      volumeMounts: [{name: podinfo, mountPath: /podinfo}]
  volumes:
    - name: podinfo
      downwardAPI: {items: [{path: labels, fieldRef: {fieldPath: metadata.labels}}]}
`)
	must(t, "wait", "--for=condition=Ready", "pod/e2e-meta", "--timeout=120s")
	must(t, "label", "pod", "e2e-meta", "stage=two", "--overwrite")
	eventually(t, 30*time.Second, func() string {
		if l := must(t, "get", "pod", "e2e-meta", "-o", "jsonpath={.metadata.labels.stage}"); l != "two" {
			return "label reverted to " + l
		}
		if out, _ := kubectl("logs", "e2e-meta", "--tail=4"); !strings.Contains(out, `stage="two"`) {
			return "downwardAPI volume not updated: " + out
		}
		return ""
	})
}
