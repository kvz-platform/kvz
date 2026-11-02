//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// kubectl debug adds an ephemeral container to a running pod; it runs in the
// pod VM (shares its network and processes) and accepts exec.
func TestEphemeralContainer(t *testing.T) {
	apply(t, `
apiVersion: v1
kind: Pod
metadata: {name: e2e-debug-target}
spec:
  containers:
    - name: web
      image: nginx:alpine
`)
	must(t, "wait", "--for=condition=Ready", "pod/e2e-debug-target", "--timeout=120s")
	must(t, "debug", "e2e-debug-target", "--image=busybox:1.36", "--container=dbg", "-i", "-t=true", "--attach=false", "--", "sh")
	deadline := time.Now().Add(90 * time.Second)
	for {
		st, _ := kubectl("get", "pod", "e2e-debug-target", "-o", "jsonpath={.status.ephemeralContainerStatuses[0].state}")
		if strings.Contains(st, "running") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ephemeral container not running: %s", st)
		}
		time.Sleep(500 * time.Millisecond)
	}
	out := must(t, "exec", "e2e-debug-target", "-c", "dbg", "--", "wget", "-qO-", "http://localhost")
	if !strings.Contains(out, "Welcome to nginx") {
		t.Fatalf("debug container cannot reach the pod's nginx on localhost:\n%s", out)
	}
}
