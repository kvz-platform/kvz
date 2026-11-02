//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// Cluster DNS answers cluster.local authoritatively (A, SRV, NXDOMAIN) and
// musl resolvers no longer stall on AAAA lookups for search-list names.
func TestClusterDNS(t *testing.T) {
	apply(t, `
apiVersion: v1
kind: Service
metadata: {name: e2e-hl}
spec: {clusterIP: None, selector: {app: e2e-hl}, ports: [{name: http, port: 80}]}
---
apiVersion: v1
kind: Pod
metadata: {name: e2e-hl-0, labels: {app: e2e-hl}}
spec: {hostname: e2e-hl-0, subdomain: e2e-hl, containers: [{name: web, image: nginx:alpine}]}
---
apiVersion: v1
kind: Pod
metadata: {name: e2e-dns}
spec:
  restartPolicy: Never
  containers:
    - name: c
      image: alpine:3.22
      command: [sh, -c, 'until getent hosts e2e-hl-0.e2e-hl.default.svc.cluster.local; do sleep 0.5; done; nslookup -type=SRV _http._tcp.e2e-hl.default.svc.cluster.local; getent hosts nope.default.svc.cluster.local || echo NXDOMAIN-OK; s=$(date +%s); getent hosts dl-cdn.alpinelinux.org >/dev/null; echo external-lookup-seconds=$(( $(date +%s) - s ))']
`)
	must(t, "wait", "--for=condition=Ready", "pod/e2e-hl-0", "--timeout=120s")
	var out string
	dnsWait(t, 120*time.Second, func() string {
		if p, _ := kubectl("get", "pod", "e2e-dns", "-o", "jsonpath={.status.phase}"); p != "Succeeded" {
			return "e2e-dns phase " + p
		}
		out = must(t, "logs", "e2e-dns")
		return ""
	})
	for _, want := range []string{"e2e-hl-0.e2e-hl.default.svc.cluster.local", "0 100 80 e2e-hl-0.e2e-hl", "NXDOMAIN-OK"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "external-lookup-seconds=") && !strings.Contains(out, "external-lookup-seconds=0") && !strings.Contains(out, "external-lookup-seconds=1\n") {
		t.Errorf("external lookup stalled:\n%s", out)
	}
}

func dnsWait(t *testing.T, timeout time.Duration, f func() string) {
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
