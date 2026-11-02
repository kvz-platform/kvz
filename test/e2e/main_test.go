//go:build e2e

// Package e2e starts a real kvz server and checks it end to end with kubectl.
//
//	KVZ_BIN=$PWD/bin/kvz go test -tags e2e -count=1 -v ./test/e2e
//
// It needs an Apple Silicon Mac with the pod kernel installed, and no other
// kvz running on the machine (the pod network uses fixed subnets). The server
// gets its own data directory and ports, and is stopped when the tests end.
//
// Optional: KVZ_E2E_MACOS_IMAGE=<tart image> also runs the macOS VM test.
package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	apiPort     = 16443
	etcdPort    = 12379
	kubeletPort = 20250
)

var (
	kvzBin     string
	kubeconfig string
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	kvzBin = os.Getenv("KVZ_BIN")
	if kvzBin == "" {
		fmt.Fprintln(os.Stderr, "e2e: set KVZ_BIN to a built kvz binary")
		return 1
	}
	dir, err := os.MkdirTemp("", "kvz-e2e-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(dir)
	kubeconfig = filepath.Join(dir, "kubeconfig.yaml")

	logf, _ := os.Create(filepath.Join(dir, "server.log"))
	srv := exec.Command(kvzBin, "server",
		"--data-dir", dir,
		"--node-name", "kvz-e2e",
		"--https-listen-port", fmt.Sprint(apiPort),
		"--etcd-port", fmt.Sprint(etcdPort),
		"--kubelet-port", fmt.Sprint(kubeletPort),
		"--disable", "traefik",
	)
	srv.Stdout, srv.Stderr = logf, logf
	if err := srv.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "e2e: start kvz:", err)
		return 1
	}
	stop := func() {
		_ = srv.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = srv.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			_ = srv.Process.Kill()
		}
	}
	defer stop()

	if err := waitReady(3 * time.Minute); err != nil {
		fmt.Fprintln(os.Stderr, "e2e: cluster never became ready:", err)
		dumpLog(filepath.Join(dir, "server.log"))
		return 1
	}
	code := m.Run()
	if code != 0 {
		dumpLog(filepath.Join(dir, "server.log"))
	}
	return code
}

// waitReady waits for the API server, then for the node to be Ready.
func waitReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(kubeconfig); err == nil {
			if _, err := kubectl("get", "--raw", "/readyz"); err == nil {
				if _, err := kubectl("wait", "--for=condition=Ready", "node/kvz-e2e", "--timeout=5s"); err == nil {
					return nil
				}
			}
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("timed out after %s", timeout)
}

// kubectl runs the kubectl built into kvz against the test cluster.
func kubectl(args ...string) (string, error) {
	return kubectlIn("", args...)
}

func kubectlIn(stdin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, kvzBin, append([]string{"kubectl", "--request-timeout=60s"}, args...)...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}

// must runs kubectl and fails the test on error.
func must(t *testing.T, args ...string) string {
	t.Helper()
	out, err := kubectl(args...)
	if err != nil {
		t.Fatalf("kubectl %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// apply applies a manifest and deletes it when the test ends.
func apply(t *testing.T, manifest string) {
	t.Helper()
	if out, err := kubectlIn(manifest, "apply", "-f", "-"); err != nil {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	t.Cleanup(func() { _, _ = kubectlIn(manifest, "delete", "--wait=false", "--ignore-not-found", "-f", "-") })
}

func dumpLog(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	lines := strings.Split(string(b), "\n")
	if len(lines) > 200 {
		lines = lines[len(lines)-200:]
	}
	fmt.Fprintln(os.Stderr, "---- last kvz server log lines ----")
	fmt.Fprintln(os.Stderr, strings.Join(lines, "\n"))
}
