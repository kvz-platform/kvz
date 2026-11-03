package upgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/GrowlyX/kvz/pkg/version"
)

// retryAfter is how long the agent waits before retrying a failed upgrade
// (a download error, say) of the same version.
const retryAfter = 2 * time.Minute

// bootTime is when this process started; the node counts as Ready on this
// binary only with a Ready heartbeat newer than this.
var bootTime = time.Now()

var (
	restartMu sync.Mutex
	restartTo string
)

// RestartRequested returns the binary to restart into once the server or
// agent command has returned, or "".
func RestartRequested() string {
	restartMu.Lock()
	defer restartMu.Unlock()
	return restartTo
}

// requestRestart stops this process the way SIGTERM does and has main call
// Restart afterwards. If shutdown hangs, it restarts anyway.
func requestRestart(bin string) {
	restartMu.Lock()
	restartTo = bin
	restartMu.Unlock()
	_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
	go func() {
		time.Sleep(45 * time.Second)
		klog.InfoS("upgrade: shutdown is taking too long, restarting now", "exe", bin)
		Restart(bin)
	}()
}

// waitPIDEnv tells a replacement process which process to wait for.
const waitPIDEnv = "KVZ_UPGRADE_WAIT_PID"

// Restart ends this process so bin runs in its place, with the same
// arguments. Under launchd (kvz install) that is just exiting: KeepAlive
// starts the installed path again, which now holds the new binary. Anywhere
// else kvz starts bin itself, as a replacement that waits for this process to
// exit before it binds any ports.
//
// It never execs in place: execve(2) of a process holding
// Virtualization.framework state hung indefinitely in testing.
func Restart(bin string) {
	restartOnce.Do(func() { restart(bin) })
	select {} // the other caller is restarting
}

var restartOnce sync.Once

func restart(bin string) {
	klog.Flush()
	if UnderLaunchd() {
		os.Exit(0)
	}
	cmd := exec.Command(bin, os.Args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), fmt.Sprintf("%s=%d", waitPIDEnv, os.Getpid()))
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "kvz: start %s: %v\n", bin, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "kvz: upgraded; restarted as pid %d (use `kvz install` to run kvz under launchd)\n", cmd.Process.Pid)
	os.Exit(0)
}

// UnderLaunchd reports whether this process is a kvz launchd service.
func UnderLaunchd() bool { return strings.HasPrefix(os.Getenv("XPC_SERVICE_NAME"), "dev.kvz.") }

// WaitForPredecessor blocks a replacement process started by Restart until
// the process it replaces has exited (and released its ports and etcd lock).
func WaitForPredecessor() {
	v := os.Getenv(waitPIDEnv)
	if v == "" {
		return
	}
	os.Unsetenv(waitPIDEnv)
	pid, err := strconv.Atoi(v)
	if err != nil || pid <= 1 {
		return
	}
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if syscall.Kill(pid, 0) == syscall.ESRCH {
			return
		}
	}
	fmt.Fprintf(os.Stderr, "kvz: pid %d is still running; starting anyway\n", pid)
}

// Agent upgrades the node it runs on when the controller asks.
type Agent struct {
	cs      kubernetes.Interface
	node    string
	dataDir string
	http    *http.Client
	failed  map[string]time.Time // version -> last failed attempt
}

// RunAgent runs the node side of upgrades until ctx ends.
func RunAgent(ctx context.Context, cs kubernetes.Interface, nodeName, dataDir string) {
	a := &Agent{cs: cs, node: nodeName, dataDir: dataDir, http: &http.Client{Timeout: 30 * time.Minute}, failed: map[string]time.Time{}}
	a.run(ctx)
}

func (a *Agent) run(ctx context.Context) {
	confirmed := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
		n, err := a.cs.CoreV1().Nodes().Get(ctx, a.node, metav1.GetOptions{})
		if err != nil {
			continue
		}
		if !confirmed {
			if !readySince(n, bootTime) {
				continue
			}
			// Ready on this binary: the install (if any) is good.
			ann := map[string]any{AnnRelease: version.Release}
			if s := loadState(a.dataDir); s != nil {
				switch {
				case s.Failed != "":
					ann[AnnStatus] = "Failed: " + s.Reason
				case s.To == version.Release:
					klog.InfoS("upgrade: running the new release", "from", s.From, "to", s.To)
					ann[AnnStatus] = "Upgraded from " + s.From
					_ = os.Remove(statePath(a.dataDir))
				}
			}
			if err := a.annotate(ctx, ann); err != nil {
				continue
			}
			confirmed = true
			continue
		}
		want := n.Annotations[AnnApply]
		if want == "" || want == version.Release {
			continue
		}
		if s := loadState(a.dataDir); s != nil && s.Failed == want {
			continue // rolled back from it; the plan needs another version
		}
		if t, ok := a.failed[want]; ok && time.Since(t) < retryAfter {
			continue
		}
		if err := a.upgrade(ctx, want, n.Annotations[AnnURL]); err != nil {
			if ctx.Err() != nil {
				return
			}
			klog.ErrorS(err, "upgrade failed", "version", want)
			a.failed[want] = time.Now()
			_ = a.annotate(ctx, map[string]any{AnnStatus: "Failed: " + oneLine(err.Error())})
			continue
		}
		return // restarting
	}
}

func (a *Agent) upgrade(ctx context.Context, want, url string) error {
	if url == "" {
		return fmt.Errorf("no release URL")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	klog.InfoS("upgrade: downloading", "version", want, "url", url)
	_ = a.annotate(ctx, map[string]any{AnnStatus: "Downloading " + want})
	work := filepath.Join(stateDir(a.dataDir), want)
	_ = os.RemoveAll(work)
	if err := os.MkdirAll(work, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(work)
	tarball := filepath.Join(work, "release.tar.gz")
	got, err := download(ctx, a.http, url, tarball)
	if err != nil {
		return err
	}
	sum, err := fetchChecksum(ctx, a.http, url+".sha256")
	if err != nil {
		return fmt.Errorf("checksum: %w", err)
	}
	if got != sum {
		return fmt.Errorf("checksum mismatch: tarball is %s, %s.sha256 says %s", got, url, sum)
	}
	_ = a.annotate(ctx, map[string]any{AnnStatus: "Verifying " + want})
	staged := filepath.Join(work, "staged")
	members, err := extract(tarball, staged)
	if err != nil {
		return fmt.Errorf("unpack: %w", err)
	}
	if err := verifyBinary(ctx, filepath.Join(staged, "bin", "kvz"), exe, want); err != nil {
		return err
	}
	targets := installTarget(exe, members)
	s := &state{From: version.Release, To: want, Exe: exe, Time: time.Now()}
	rels := make([]string, 0, len(targets))
	for rel := range targets {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		s.Files = append(s.Files, targets[rel])
	}
	// Write the state first: if we crash halfway, BootCheck on the new binary
	// still knows what to roll back.
	if err := saveState(a.dataDir, s); err != nil {
		return err
	}
	for i, rel := range rels {
		if err := swap(filepath.Join(staged, filepath.FromSlash(rel)), targets[rel]); err != nil {
			for _, done := range rels[:i] {
				_ = os.Rename(targets[done]+".prev", targets[done])
			}
			_ = os.Remove(statePath(a.dataDir))
			return fmt.Errorf("install %s: %w", targets[rel], err)
		}
	}
	klog.InfoS("upgrade: installed, restarting", "version", want, "files", s.Files)
	_ = a.annotate(ctx, map[string]any{AnnStatus: "Restarting into " + want})
	requestRestart(exe)
	return nil
}

func (a *Agent) annotate(ctx context.Context, ann map[string]any) error {
	b, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": ann}})
	_, err := a.cs.CoreV1().Nodes().Patch(ctx, a.node, types.MergePatchType, b, metav1.PatchOptions{})
	return err
}

// readySince reports whether the node is Ready with a heartbeat from this
// process (status timestamps have one-second resolution).
func readySince(n *corev1.Node, t time.Time) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue && !c.LastHeartbeatTime.Time.Before(t.Truncate(time.Second))
		}
	}
	return false
}
