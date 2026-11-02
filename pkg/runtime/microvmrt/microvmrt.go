//go:build darwin

// Package microvmrt is the kvz "linux" runtime: one kvz microVM per pod,
// booted cold for every pod (no pools, no snapshots). All containers of a pod
// share the VM, so they share a network namespace (one pod IP) like real
// Kubernetes pods. The pod IP is allocated by the host and handed to the
// kernel on its cmdline, so it is published before the VM even boots.
package microvmrt

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"github.com/GrowlyX/kvz/pkg/image"
	"github.com/GrowlyX/kvz/pkg/microvm"
	"github.com/GrowlyX/kvz/pkg/podnet"
	rt "github.com/GrowlyX/kvz/pkg/runtime"
	"github.com/GrowlyX/kvz/pkg/runtime/volumes"
)

//go:embed kvz-init
var initBin []byte

type Backend struct {
	dir    string
	kernel string
	initrd string
	t      *rt.Tracker
	vols   *volumes.Materializer
	images *image.Store
	pnet   *podnet.Net
	dns    []string

	mu   sync.Mutex
	pods map[string]*pod
}

type pod struct {
	vm     *microvm.VM
	ip     net.IP
	dir    string
	cancel context.CancelFunc
	done   chan struct{}
	names  []string
}

func DefaultKernel() string {
	if k := os.Getenv("KVZ_KERNEL"); k != "" {
		return k
	}
	home, _ := os.UserHomeDir()
	if p := filepath.Join(home, ".kvz", "kernel", "Image"); exists(p) {
		return p
	}
	return filepath.Join(home, "Library", "Application Support", "com.apple.container", "kernels", "default.kernel-arm64")
}

func consoleLog(dir string) string {
	if os.Getenv("KVZ_VM_CONSOLE") != "" {
		return filepath.Join(dir, "console.log")
	}
	return ""
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func New(dataDir string, t *rt.Tracker, vols *volumes.Materializer) (*Backend, error) {
	home, _ := os.UserHomeDir()
	initrd, err := microvm.InitrdPath(filepath.Join(dataDir, "microvm"), initBin)
	if err != nil {
		return nil, err
	}
	cidr := os.Getenv("KVZ_POD_CIDR")
	if cidr == "" {
		cidr = "10.88.0.0/16"
	}
	pn, err := podnet.New(cidr, 0)
	if err != nil {
		return nil, err
	}
	return &Backend{dir: filepath.Join(dataDir, "mvm"), kernel: DefaultKernel(), initrd: initrd, t: t, vols: vols,
		images: image.NewStore(filepath.Join(home, ".kvz", "images")), pnet: pn, dns: []string{pn.GW.String()},
		pods: map[string]*pod{}}, nil
}

func (b *Backend) Handler() string { return "linux" }

// Images exposes the store (pre-pull API).
func (b *Backend) Images() *image.Store { return b.images }

type cspec struct {
	Name     string   `json:"name"`
	Argv     []string `json:"argv"`
	Env      []string `json:"env"`
	Cwd      string   `json:"cwd"`
	UID      int      `json:"uid"`
	GID      int      `json:"gid"`
	ImageTag string   `json:"image_tag"`
	Restart  string   `json:"restart"`
}

type mspec struct {
	Container string `json:"container"`
	Tag       string `json:"tag"`
	Sub       string `json:"sub"`
	Path      string `json:"path"`
	ReadOnly  bool   `json:"ro"`
}

type spec struct {
	IP         string   `json:"ip"`
	Mask       int      `json:"mask"`
	GW         string   `json:"gw"`
	Hostname   string   `json:"hostname"`
	DNS        []string `json:"dns"`
	Hosts      string   `json:"hosts"`
	Containers []cspec  `json:"containers"`
	Mounts     []mspec  `json:"mounts"`
}

func (b *Backend) Start(ctx context.Context, p *corev1.Pod) error {
	key := rt.Key(p.Namespace, p.Name)
	ip, err := b.pnet.Alloc()
	if err != nil {
		return err
	}
	dir := filepath.Join(b.dir, string(p.UID))
	_ = os.MkdirAll(filepath.Join(dir, "logs"), 0o755)
	pctx, cancel := context.WithCancel(context.Background())
	ps := &pod{ip: ip, dir: dir, cancel: cancel, done: make(chan struct{})}
	b.mu.Lock()
	b.pods[key] = ps
	b.mu.Unlock()
	// Publish the IP immediately: it is fixed before boot.
	b.t.Update(p.Namespace, p.Name, func(s *corev1.PodStatus) {
		s.PodIP = ip.String()
		s.PodIPs = []corev1.PodIP{{IP: ip.String()}}
	})
	go func() {
		defer close(ps.done)
		if err := b.run(pctx, p, ps); err != nil && pctx.Err() == nil {
			klog.ErrorS(err, "microvm pod failed", "pod", key)
			b.t.Update(p.Namespace, p.Name, func(s *corev1.PodStatus) {
				for _, c := range p.Spec.Containers {
					rt.SetTerminated(s, c.Name, 128, "StartError")
				}
				s.Message = err.Error()
			})
		}
	}()
	return nil
}

func (b *Backend) buildSpec(p *corev1.Pod, ps *pod) (*spec, []microvm.Share, error) {
	sp := &spec{Hostname: p.Name, DNS: b.dns, IP: ps.ip.String(), Mask: b.pnet.Bits(), GW: b.pnet.GW.String()}
	if p.Spec.Hostname != "" {
		sp.Hostname = p.Spec.Hostname
	}
	sp.Hosts = fmt.Sprintf("127.0.0.1\tlocalhost\n::1\tlocalhost\n%s\t%s\n", ps.ip, sp.Hostname)
	shares := []microvm.Share{{Tag: "pod", Path: ps.dir}}
	seen := map[string]bool{}
	for _, c := range p.Spec.Containers {
		im, err := b.images.Get(c.Image)
		if err != nil {
			return nil, nil, err
		}
		if !seen[im.Tag] {
			shares = append(shares, microvm.Share{Tag: im.Tag, Path: im.Rootfs, ReadOnly: true})
			seen[im.Tag] = true
		}
		argv := im.Entrypoint
		args := im.Cmd
		if len(c.Command) > 0 {
			argv, args = c.Command, nil
		}
		if len(c.Args) > 0 {
			args = c.Args
		}
		argv = append(append([]string{}, argv...), args...)
		if len(argv) == 0 {
			return nil, nil, fmt.Errorf("container %s: no command", c.Name)
		}
		env := mergeEnv(im.Env, rt.Env(c))
		cwd := im.WorkingDir
		if c.WorkingDir != "" {
			cwd = c.WorkingDir
		}
		uid, gid := userIDs(im, c)
		restart := string(p.Spec.RestartPolicy)
		if restart == "" {
			restart = "Always"
		}
		sp.Containers = append(sp.Containers, cspec{Name: c.Name, Argv: argv, Env: env, Cwd: cwd, UID: uid, GID: gid, ImageTag: im.Tag, Restart: restart})
		ps.names = append(ps.names, c.Name)
	}
	// Volumes: everything materialized under the pod dir is reachable through
	// the "kvz" share; hostPath gets its own share.
	mounts, err := b.vols.Materialize(p, ps.dir)
	if err != nil {
		return nil, nil, err
	}
	hp := 0
	for cname, specs := range mounts {
		for _, m := range specs {
			parts := strings.Split(m, ":")
			host, guest := parts[0], parts[1]
			ro := len(parts) > 2 && parts[2] == "ro"
			if rel, err := filepath.Rel(ps.dir, host); err == nil && !strings.HasPrefix(rel, "..") {
				sp.Mounts = append(sp.Mounts, mspec{Container: cname, Sub: rel, Path: guest, ReadOnly: ro})
			} else {
				tag := fmt.Sprintf("hp%d", hp)
				hp++
				shares = append(shares, microvm.Share{Tag: tag, Path: host, ReadOnly: ro})
				sp.Mounts = append(sp.Mounts, mspec{Container: cname, Tag: tag, Path: guest, ReadOnly: ro})
			}
		}
	}
	return sp, shares, nil
}

func mergeEnv(base, over []string) []string {
	idx := map[string]int{}
	out := append([]string{}, base...)
	for i, e := range out {
		idx[strings.SplitN(e, "=", 2)[0]] = i
	}
	for _, e := range over {
		k := strings.SplitN(e, "=", 2)[0]
		if i, ok := idx[k]; ok {
			out[i] = e
		} else {
			idx[k] = len(out)
			out = append(out, e)
		}
	}
	return out
}

func userIDs(im *image.Image, c corev1.Container) (int, int) {
	if c.SecurityContext != nil && c.SecurityContext.RunAsUser != nil {
		gid := 0
		if c.SecurityContext.RunAsGroup != nil {
			gid = int(*c.SecurityContext.RunAsGroup)
		}
		return int(*c.SecurityContext.RunAsUser), gid
	}
	u := im.User
	if u == "" {
		return 0, 0
	}
	parts := strings.SplitN(u, ":", 2)
	uid, err := strconv.Atoi(parts[0])
	gid := 0
	if err != nil {
		uid, gid = lookupUser(im.Rootfs, parts[0])
	}
	if len(parts) == 2 {
		if g, err := strconv.Atoi(parts[1]); err == nil {
			gid = g
		}
	}
	return uid, gid
}

func lookupUser(root, name string) (int, int) {
	b, err := os.ReadFile(filepath.Join(root, "etc", "passwd"))
	if err != nil {
		return 0, 0
	}
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Split(l, ":")
		if len(f) > 3 && f[0] == name {
			u, _ := strconv.Atoi(f[2])
			g, _ := strconv.Atoi(f[3])
			return u, g
		}
	}
	return 0, 0
}

func vcpus(p *corev1.Pod) uint {
	var milli int64
	for _, c := range p.Spec.Containers {
		if q, ok := c.Resources.Limits[corev1.ResourceCPU]; ok {
			milli += q.MilliValue()
		} else if q, ok := c.Resources.Requests[corev1.ResourceCPU]; ok {
			milli += q.MilliValue()
		}
	}
	n := uint((milli + 999) / 1000)
	if n < 1 {
		n = 1
	}
	return n
}

func memMB(p *corev1.Pod) uint64 {
	var bytes int64
	for _, c := range p.Spec.Containers {
		if q, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
			bytes += q.Value()
		}
	}
	mb := uint64(bytes >> 20)
	if mb == 0 {
		mb = 512 // only touched pages are backed by host memory
	}
	if mb < 128 {
		mb = 128
	}
	return mb
}

func (b *Backend) run(ctx context.Context, p *corev1.Pod, ps *pod) error {
	sp, shares, err := b.buildSpec(p, ps)
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(sp)
	if err := os.WriteFile(filepath.Join(ps.dir, "spec.json"), raw, 0o644); err != nil {
		return err
	}
	nic, err := b.pnet.Attach(ctx)
	if err != nil {
		return err
	}
	vm, err := microvm.Boot(microvm.Config{Kernel: b.kernel, Initrd: b.initrd, IP: ps.ip, GW: b.pnet.GW, Mask: b.pnet.Bits(),
		CPUs: vcpus(p), MemoryMB: memMB(p), Shares: shares, ConsoleLog: consoleLog(ps.dir), NetFile: nic})
	if err != nil {
		nic.Close()
		return err
	}
	b.mu.Lock()
	ps.vm = vm
	b.mu.Unlock()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-vm.Done():
			if ctx.Err() == nil {
				b.t.Update(p.Namespace, p.Name, func(s *corev1.PodStatus) {
					for _, n := range ps.names {
						rt.SetTerminated(s, n, 137, "VMStopped")
					}
				})
			}
			return nil
		case e, ok := <-vm.Events:
			if !ok {
				<-vm.Done()
				continue
			}
			switch e.Type {
			case "started":
				b.t.Update(p.Namespace, p.Name, func(s *corev1.PodStatus) {
					for _, cs := range s.ContainerStatuses {
						if cs.Name == e.Container && (cs.State.Terminated != nil || cs.State.Waiting != nil && cs.State.Waiting.Reason == "CrashLoopBackOff") {
							rt.IncRestart(s, e.Container)
						}
					}
					rt.SetRunning(s, e.Container, fmt.Sprintf("kvz://%s/%d", p.UID, e.Pid), true)
				})
			case "exit":
				code := int32(e.Code)
				reason := "Completed"
				if code != 0 {
					reason = "Error"
				}
				b.t.Update(p.Namespace, p.Name, func(s *corev1.PodStatus) {
					if rt.WillRestart(p.Spec.RestartPolicy, code) {
						rt.SetCrashLoop(s, e.Container, code, reason)
					} else {
						rt.SetTerminated(s, e.Container, code, reason)
					}
				})
			case "error":
				klog.InfoS("kvz-init error", "pod", rt.Key(p.Namespace, p.Name), "msg", e.Msg)
			}
		}
	}
}

func (b *Backend) Stop(ctx context.Context, p *corev1.Pod) error {
	key := rt.Key(p.Namespace, p.Name)
	b.mu.Lock()
	ps := b.pods[key]
	delete(b.pods, key)
	b.mu.Unlock()
	if ps == nil {
		return nil
	}
	b.mu.Lock()
	vm := ps.vm
	b.mu.Unlock()
	t0 := time.Now()
	var tExit, tStop time.Duration
	defer func() {
		klog.InfoS("microvm pod stopped", "pod", key, "exit", tExit, "vmStop", tStop, "total", time.Since(t0))
	}()
	if vm != nil {
		grace := 1 * time.Second
		if p.Spec.TerminationGracePeriodSeconds != nil {
			grace = time.Duration(*p.Spec.TerminationGracePeriodSeconds) * time.Second
		}
		if grace > 0 && vm.Send(map[string]any{"type": "kill", "signal": int(syscall.SIGTERM)}) == nil {
			// wait until all containers exited or grace expires
			deadline := time.After(grace)
			exited := map[string]bool{}
			_ = exited
		wait:
			for {
				st := b.t.Get(p.Namespace, p.Name)
				all := st != nil
				if st != nil {
					for _, cs := range st.Status.ContainerStatuses {
						if cs.State.Terminated == nil {
							all = false
						}
					}
				}
				if all {
					break
				}
				select {
				case <-deadline:
					break wait
				case <-vm.Done():
					break wait
				case <-time.After(20 * time.Millisecond):
				}
			}
		}
		tExit = time.Since(t0)
		_ = vm.Stop()
		tStop = time.Since(t0)
	}
	ps.cancel()
	<-ps.done
	b.pnet.Free(ps.ip)
	return os.RemoveAll(ps.dir)
}

func (b *Backend) Logs(ctx context.Context, p *corev1.Pod, container string, opts api.ContainerLogOpts) (io.ReadCloser, error) {
	f, err := os.Open(filepath.Join(b.dir, string(p.UID), "logs", container+".log"))
	if err != nil {
		return nil, err
	}
	if !opts.Follow {
		return f, nil
	}
	return rt.Follow(ctx, f), nil
}

func (b *Backend) Exec(ctx context.Context, p *corev1.Pod, container string, cmd []string, attach api.AttachIO) error {
	b.mu.Lock()
	ps := b.pods[rt.Key(p.Namespace, p.Name)]
	b.mu.Unlock()
	if ps == nil || ps.vm == nil {
		return fmt.Errorf("pod not running")
	}
	return execInVM(ctx, ps.vm, container, cmd, attach)
}

// PortForward dials into the pod over the pod network.
func (b *Backend) PortForward(ctx context.Context, p *corev1.Pod, port int32, stream io.ReadWriteCloser) error {
	b.mu.Lock()
	ps := b.pods[rt.Key(p.Namespace, p.Name)]
	b.mu.Unlock()
	if ps == nil {
		return fmt.Errorf("pod not running")
	}
	c, err := b.pnet.Dial(ctx, net.JoinHostPort(ps.ip.String(), strconv.Itoa(int(port))))
	if err != nil {
		return err
	}
	defer c.Close()
	done := make(chan struct{}, 2)
	go func() { io.Copy(c, stream); done <- struct{}{} }()
	go func() { io.Copy(stream, c); done <- struct{}{} }()
	<-done
	return nil
}
