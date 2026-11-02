// Package linux runs pod containers as Linux micro-VMs through Apple's
// `container` runtime (Containerization framework). Every container gets its
// own lightweight VM and its own routable IP on the host's vmnet network; the
// pod IP is the first container's IP.
package linux

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	rt "github.com/GrowlyX/kvz/pkg/runtime"
	"github.com/GrowlyX/kvz/pkg/runtime/volumes"
)

type Backend struct {
	bin    string
	dir    string
	hostIP string
	t      *rt.Tracker
	vols   *volumes.Materializer

	mu   sync.Mutex
	pods map[string]*podState

	// Apple's container daemon serializes VM starts (~2/s per host regardless
	// of client concurrency; see bench/container_scaling.py). Gating starts
	// keeps throughput identical but makes pods become ready FIFO instead of
	// all at the end of a burst.
	startGate chan struct{}

	pollMu  sync.Mutex
	waiters map[string][]chan string // container name -> waiters for IP
	kick    chan struct{}
}

type podState struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup
	names  map[string]string // k8s container -> runtime id
}

func New(dataDir, hostIP string, t *rt.Tracker, vols *volumes.Materializer) *Backend {
	bin, err := exec.LookPath("container")
	if err != nil {
		bin = "/usr/local/bin/container"
	}
	b := &Backend{bin: bin, dir: filepath.Join(dataDir, "pods"), hostIP: hostIP, t: t, vols: vols,
		pods: map[string]*podState{}, waiters: map[string][]chan string{}, kick: make(chan struct{}, 1)}
	n := 2
	if v, err := strconv.Atoi(os.Getenv("KVZ_LINUX_START_CONCURRENCY")); err == nil && v > 0 {
		n = v
	}
	b.startGate = make(chan struct{}, n)
	go b.ipPoller()
	return b
}

func (b *Backend) Handler() string { return "linux" }

func cid(pod *corev1.Pod, c string) string {
	u := strings.ReplaceAll(string(pod.UID), "-", "")
	if len(u) > 12 {
		u = u[:12]
	}
	return "kvz-" + u + "-" + c
}

func (b *Backend) podDir(pod *corev1.Pod) string { return filepath.Join(b.dir, string(pod.UID)) }

func (b *Backend) Start(ctx context.Context, pod *corev1.Pod) error {
	dir := b.podDir(pod)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	mounts, err := b.vols.Materialize(pod, dir)
	if err != nil {
		return err
	}
	pctx, cancel := context.WithCancel(context.Background())
	ps := &podState{cancel: cancel, names: map[string]string{}}
	for _, c := range pod.Spec.Containers {
		ps.names[c.Name] = cid(pod, c.Name)
	}
	b.mu.Lock()
	b.pods[rt.Key(pod.Namespace, pod.Name)] = ps
	b.mu.Unlock()
	for i, c := range pod.Spec.Containers {
		i, c := i, c
		ps.wg.Add(1)
		go func() {
			defer ps.wg.Done()
			b.supervise(pctx, pod, c, i == 0, dir, mounts[c.Name])
		}()
	}
	return nil
}

func (b *Backend) runArgs(pod *corev1.Pod, c corev1.Container, mounts []string) []string {
	args := []string{"run", "--rm", "--name", cid(pod, c.Name), "--progress", "none",
		"-l", "kvz.pod=" + pod.Namespace + "." + pod.Name, "-l", "kvz.uid=" + string(pod.UID)}
	if q, ok := c.Resources.Limits[corev1.ResourceCPU]; ok {
		n := (q.MilliValue() + 999) / 1000
		args = append(args, "-c", fmt.Sprint(max(n, 1)))
	} else if q, ok := c.Resources.Requests[corev1.ResourceCPU]; ok {
		args = append(args, "-c", fmt.Sprint(max((q.MilliValue()+999)/1000, 1)))
	}
	if q, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
		args = append(args, "-m", fmt.Sprintf("%dM", max(q.Value()>>20, 64)))
	} else if q, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
		args = append(args, "-m", fmt.Sprintf("%dM", max(q.Value()>>20, 64)))
	}
	for _, e := range rt.Env(c) {
		args = append(args, "-e", e)
	}
	for _, m := range mounts {
		args = append(args, "-v", m)
	}
	if c.WorkingDir != "" {
		args = append(args, "-w", c.WorkingDir)
	}
	if c.TTY {
		args = append(args, "-t")
	}
	if c.Stdin {
		args = append(args, "-i")
	}
	rest := []string{}
	if len(c.Command) > 0 {
		args = append(args, "--entrypoint", c.Command[0])
		rest = append(rest, c.Command[1:]...)
	}
	rest = append(rest, c.Args...)
	args = append(args, c.Image)
	return append(args, rest...)
}

func (b *Backend) supervise(ctx context.Context, pod *corev1.Pod, c corev1.Container, primary bool, dir string, mounts []string) {
	name := cid(pod, c.Name)
	backoff := time.Second
	for {
		logf, err := os.OpenFile(filepath.Join(dir, c.Name+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return
		}
		// Clear any leftover with the same name (crash recovery).
		_ = exec.Command(b.bin, "delete", "--force", name).Run()
		cmd := exec.Command(b.bin, b.runArgs(pod, c, mounts)...)
		cmd.Stdout, cmd.Stderr = logf, logf
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		select {
		case b.startGate <- struct{}{}:
		case <-ctx.Done():
			logf.Close()
			return
		}
		gated := true
		release := func() {
			if gated {
				gated = false
				<-b.startGate
			}
		}
		ipc := b.waitIP(name)
		if err := cmd.Start(); err != nil {
			release()
			logf.Close()
			b.t.Update(pod.Namespace, pod.Name, func(s *corev1.PodStatus) { rt.SetTerminated(s, c.Name, 127, "StartError") })
			return
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		var werr error
		exited := false
	wait:
		for {
			select {
			case ip := <-ipc:
				release()
				b.t.Update(pod.Namespace, pod.Name, func(s *corev1.PodStatus) {
					if primary && ip != "" {
						s.PodIP = ip
						s.PodIPs = []corev1.PodIP{{IP: ip}}
					}
					rt.SetRunning(s, c.Name, "container://"+name, true)
				})
				ipc = nil
			case werr = <-done:
				exited = true
				break wait
			case <-ctx.Done():
				break wait
			}
		}
		release()
		if !exited {
			b.cancelWait(name, ipc)
			grace := 10
			if pod.Spec.TerminationGracePeriodSeconds != nil {
				grace = int(*pod.Spec.TerminationGracePeriodSeconds)
			}
			_ = exec.Command(b.bin, "stop", "-t", fmt.Sprint(grace), name).Run()
			select {
			case <-done:
			case <-time.After(time.Duration(grace+5) * time.Second):
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				_ = exec.Command(b.bin, "delete", "--force", name).Run()
			}
			logf.Close()
			return
		}
		b.cancelWait(name, ipc)
		logf.Close()
		code := int32(0)
		if werr != nil {
			code = 1
			if ee, ok := werr.(*exec.ExitError); ok {
				code = int32(ee.ExitCode())
			}
		}
		reason := "Completed"
		if code != 0 {
			reason = "Error"
		}
		b.t.Update(pod.Namespace, pod.Name, func(s *corev1.PodStatus) { rt.SetTerminated(s, c.Name, code, reason) })
		rp := pod.Spec.RestartPolicy
		if rp == corev1.RestartPolicyNever || (rp == corev1.RestartPolicyOnFailure && code == 0) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 60*time.Second {
			backoff *= 2
		}
		b.t.Update(pod.Namespace, pod.Name, func(s *corev1.PodStatus) { rt.IncRestart(s, c.Name) })
	}
}

// --- IP discovery: one batched `container ls` serves all starting containers.

func (b *Backend) waitIP(name string) chan string {
	ch := make(chan string, 1)
	b.pollMu.Lock()
	b.waiters[name] = append(b.waiters[name], ch)
	b.pollMu.Unlock()
	select {
	case b.kick <- struct{}{}:
	default:
	}
	return ch
}

func (b *Backend) cancelWait(name string, ch chan string) {
	if ch == nil {
		return
	}
	b.pollMu.Lock()
	ws := b.waiters[name]
	for i, w := range ws {
		if w == ch {
			ws = append(ws[:i], ws[i+1:]...)
			break
		}
	}
	if len(ws) == 0 {
		delete(b.waiters, name)
	} else {
		b.waiters[name] = ws
	}
	b.pollMu.Unlock()
}

type lsEntry struct {
	ID     string `json:"id"`
	Status struct {
		State    string `json:"state"`
		Networks []struct {
			IPv4 string `json:"ipv4Address"`
		} `json:"networks"`
	} `json:"status"`
	Configuration struct {
		ID string `json:"id"`
	} `json:"configuration"`
}

func (b *Backend) ipPoller() {
	for {
		b.pollMu.Lock()
		n := len(b.waiters)
		b.pollMu.Unlock()
		if n == 0 {
			<-b.kick
			continue
		}
		out, err := exec.Command(b.bin, "ls", "--format", "json").Output()
		if err == nil {
			var ents []lsEntry
			if json.Unmarshal(out, &ents) == nil {
				b.pollMu.Lock()
				for _, e := range ents {
					id := e.ID
					if id == "" {
						id = e.Configuration.ID
					}
					ws, ok := b.waiters[id]
					if !ok || e.Status.State != "running" {
						continue
					}
					ip := ""
					if len(e.Status.Networks) > 0 {
						ip = strings.SplitN(e.Status.Networks[0].IPv4, "/", 2)[0]
					}
					for _, w := range ws {
						w <- ip
					}
					delete(b.waiters, id)
				}
				b.pollMu.Unlock()
			}
		} else {
			klog.V(2).InfoS("container ls failed", "err", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (b *Backend) Stop(ctx context.Context, pod *corev1.Pod) error {
	k := rt.Key(pod.Namespace, pod.Name)
	b.mu.Lock()
	ps := b.pods[k]
	delete(b.pods, k)
	b.mu.Unlock()
	if ps != nil {
		ps.cancel()
		ps.wg.Wait()
	}
	return os.RemoveAll(b.podDir(pod))
}

func (b *Backend) Logs(ctx context.Context, pod *corev1.Pod, container string, opts api.ContainerLogOpts) (io.ReadCloser, error) {
	f, err := os.Open(filepath.Join(b.podDir(pod), container+".log"))
	if err != nil {
		return nil, err
	}
	if !opts.Follow {
		return f, nil
	}
	return rt.Follow(ctx, f), nil
}

func (b *Backend) Exec(ctx context.Context, pod *corev1.Pod, container string, cmdv []string, attach api.AttachIO) error {
	args := []string{"exec"}
	if attach.Stdin() != nil {
		args = append(args, "-i")
	}
	if attach.TTY() {
		args = append(args, "-t")
	}
	args = append(append(args, cid(pod, container)), cmdv...)
	cmd := exec.CommandContext(ctx, b.bin, args...)
	if attach.Stdin() != nil {
		cmd.Stdin = attach.Stdin()
	}
	if attach.Stdout() != nil {
		cmd.Stdout = attach.Stdout()
	}
	if attach.Stderr() != nil {
		cmd.Stderr = attach.Stderr()
	} else if attach.Stdout() != nil {
		cmd.Stderr = attach.Stdout()
	}
	return cmd.Run()
}
