//go:build darwin

// Package macos runs pods as native macOS virtual machines on
// Virtualization.framework. Container 0 is the VM: its image names a local VM
// bundle (a tart-format directory: disk.img, nvram.bin, config.json). The disk
// is cloned with APFS clonefile(2) so pod start costs O(1) disk I/O. If the
// container has a command it is run inside the guest over SSH once the guest
// is reachable; its output becomes the container log and its exit code ends
// the pod. Approach inspired by agoda-com/macOS-vz-kubelet (Apache-2.0).
package macos

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Code-Hex/vz/v3"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"github.com/GrowlyX/kvz/pkg/podnet"
	rt "github.com/GrowlyX/kvz/pkg/runtime"
)

// MaxVMs is the macOS kernel's limit on concurrently running macOS guests.
const MaxVMs = 2

type Backend struct {
	pn       *podnet.Net
	dir      string
	t        *rt.Tracker
	slots    chan struct{}
	user     string
	password string

	mu  sync.Mutex
	vms map[string]*instance
}

type instance struct {
	vm     *vz.VirtualMachine
	dir    string
	mac    string
	ip     string
	cancel context.CancelFunc
	done   chan struct{}
	slot   bool
	nic    *os.File // must outlive the VM (VZ does not dup the fd)
}

type bundleConfig struct {
	HardwareModel string `json:"hardwareModel"`
	CPUCount      uint   `json:"cpuCount"`
	MemorySize    uint64 `json:"memorySize"`
}

func New(dataDir string, t *rt.Tracker, pn *podnet.Net) *Backend {
	b := &Backend{pn: pn, dir: filepath.Join(dataDir, "vms"), t: t, slots: make(chan struct{}, MaxVMs),
		user: envOr("KVZ_MACOS_SSH_USER", "admin"), password: envOr("KVZ_MACOS_SSH_PASSWORD", "admin"),
		vms: map[string]*instance{}}
	return b
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func (b *Backend) Handler() string { return "macos" }

// resolve maps an image reference to a local bundle dir.
func resolve(image string) (string, error) {
	home, _ := os.UserHomeDir()
	image = strings.TrimPrefix(image, "tart://")
	if strings.HasPrefix(image, "/") {
		return image, nil
	}
	if !strings.Contains(image, "/") {
		return filepath.Join(home, ".tart", "vms", image), nil
	}
	repo, tag := image, "latest"
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		repo, tag = image[:i], image[i+1:]
	}
	p := filepath.Join(home, ".tart", "cache", "OCIs", repo, tag)
	if _, err := os.Stat(filepath.Join(p, "disk.img")); err != nil {
		klog.InfoS("pulling macOS image via tart", "image", image)
		if out, err := exec.Command("tart", "pull", image).CombinedOutput(); err != nil {
			return "", fmt.Errorf("tart pull %s: %v: %s", image, err, out)
		}
	}
	return p, nil
}

func (b *Backend) Start(ctx context.Context, pod *corev1.Pod) error {
	key := rt.Key(pod.Namespace, pod.Name)
	ictx, cancel := context.WithCancel(context.Background())
	inst := &instance{cancel: cancel, done: make(chan struct{}), dir: filepath.Join(b.dir, string(pod.UID))}
	b.mu.Lock()
	b.vms[key] = inst
	b.mu.Unlock()
	go func() {
		defer close(inst.done)
		if err := b.run(ictx, pod, inst); err != nil && ictx.Err() == nil {
			klog.ErrorS(err, "macOS VM failed", "pod", key)
			c := pod.Spec.Containers[0].Name
			b.t.Update(pod.Namespace, pod.Name, func(s *corev1.PodStatus) {
				rt.SetTerminated(s, c, 1, "VMError")
				s.Message = err.Error()
			})
		}
	}()
	return nil
}

func (b *Backend) run(ctx context.Context, pod *corev1.Pod, inst *instance) error {
	c := pod.Spec.Containers[0]
	select {
	case b.slots <- struct{}{}:
		inst.slot = true
	default:
		b.t.Update(pod.Namespace, pod.Name, func(s *corev1.PodStatus) {
			rt.SetWaiting(s, c.Name, "WaitingForVMSlot", fmt.Sprintf("macOS allows %d concurrent VMs per host", MaxVMs))
		})
		select {
		case b.slots <- struct{}{}:
			inst.slot = true
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	t0 := time.Now()
	src, err := resolve(c.Image)
	if err != nil {
		return err
	}
	var bc bundleConfig
	raw, err := os.ReadFile(filepath.Join(src, "config.json"))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &bc); err != nil {
		return err
	}
	if err := os.MkdirAll(inst.dir, 0o755); err != nil {
		return err
	}
	disk, nvram := filepath.Join(inst.dir, "disk.img"), filepath.Join(inst.dir, "nvram.bin")
	for _, f := range [][2]string{{filepath.Join(src, "disk.img"), disk}, {filepath.Join(src, "nvram.bin"), nvram}} {
		_ = os.Remove(f[1])
		if err := unix.Clonefile(f[0], f[1], unix.CLONE_NOFOLLOW); err != nil {
			return fmt.Errorf("clonefile %s: %w", f[0], err)
		}
	}
	tClone := time.Since(t0)

	cpus, mem := bc.CPUCount, bc.MemorySize
	if q, ok := c.Resources.Limits[corev1.ResourceCPU]; ok {
		cpus = uint(max((q.MilliValue()+999)/1000, 2))
	}
	if q, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
		mem = uint64(max(q.Value(), 4<<30))
	}
	if cpus == 0 {
		cpus = 4
	}
	if mem == 0 {
		mem = 8 << 30
	}
	var nic *os.File
	if b.pn != nil {
		nic, err = b.pn.Attach(ctx)
		if err != nil {
			return err
		}
		inst.nic = nic
	}
	vm, mac, err := buildVM(bc, disk, nvram, cpus, mem, nic)
	if err != nil {
		return err
	}
	inst.vm, inst.mac = vm, mac
	if err := vm.Start(); err != nil {
		return fmt.Errorf("vm start: %w", err)
	}
	tStart := time.Since(t0)
	ip, err := b.waitLease(ctx, mac, 3*time.Minute)
	if err != nil {
		return err
	}
	inst.ip = ip
	tIP := time.Since(t0)
	b.t.Update(pod.Namespace, pod.Name, func(s *corev1.PodStatus) {
		s.PodIP = ip
		s.PodIPs = []corev1.PodIP{{IP: ip}}
		rt.SetRunning(s, c.Name, "vz://"+string(pod.UID), false)
	})
	cl, tBanner, err := b.connectSSH(ctx, ip, 5*time.Minute, t0)
	if err != nil {
		return err
	}
	defer cl.Close()
	tSSH := time.Since(t0)
	klog.InfoS("macOS VM ready", "pod", rt.Key(pod.Namespace, pod.Name), "ip", ip,
		"clone", tClone, "vmStart", tStart, "ipLease", tIP, "sshBanner", tBanner, "sshReady", tSSH)
	_ = os.WriteFile(filepath.Join(inst.dir, "timings.json"), []byte(fmt.Sprintf(
		`{"clone_ms":%d,"vm_start_ms":%d,"ip_ms":%d,"ssh_banner_ms":%d,"ssh_ms":%d}`,
		tClone.Milliseconds(), tStart.Milliseconds(), tIP.Milliseconds(), tBanner.Milliseconds(), tSSH.Milliseconds())), 0o644)
	b.t.Update(pod.Namespace, pod.Name, func(s *corev1.PodStatus) {
		rt.SetRunning(s, c.Name, "vz://"+string(pod.UID), true)
	})

	argv := append(append([]string{}, c.Command...), c.Args...)
	if len(argv) == 0 {
		<-ctx.Done()
		return nil
	}
	logf, err := os.OpenFile(filepath.Join(inst.dir, c.Name+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logf.Close()
	sess, err := cl.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	sess.Stdout, sess.Stderr = logf, logf
	var sb strings.Builder
	for _, e := range rt.Env(c) {
		sb.WriteString("export " + shellQuote(e) + "; ")
	}
	sb.WriteString(joinQuoted(argv))
	errc := make(chan error, 1)
	go func() { errc <- sess.Run(sb.String()) }()
	select {
	case <-ctx.Done():
		return nil
	case err := <-errc:
		code := int32(0)
		var ee *ssh.ExitError
		if errors.As(err, &ee) {
			code = int32(ee.ExitStatus())
		} else if err != nil {
			code = 1
		}
		reason := "Completed"
		if code != 0 {
			reason = "Error"
		}
		b.t.Update(pod.Namespace, pod.Name, func(s *corev1.PodStatus) { rt.SetTerminated(s, c.Name, code, reason) })
		_ = vm.Stop()
		return nil
	}
}

func buildVM(bc bundleConfig, disk, nvram string, cpus uint, mem uint64, nicFile *os.File) (*vz.VirtualMachine, string, error) {
	hw, err := base64.StdEncoding.DecodeString(bc.HardwareModel)
	if err != nil {
		return nil, "", err
	}
	model, err := vz.NewMacHardwareModelWithData(hw)
	if err != nil {
		return nil, "", err
	}
	id, err := vz.NewMacMachineIdentifier() // fresh identity per pod
	if err != nil {
		return nil, "", err
	}
	aux, err := vz.NewMacAuxiliaryStorage(nvram)
	if err != nil {
		return nil, "", err
	}
	plat, err := vz.NewMacPlatformConfiguration(vz.WithMacAuxiliaryStorage(aux), vz.WithMacHardwareModel(model), vz.WithMacMachineIdentifier(id))
	if err != nil {
		return nil, "", err
	}
	boot, err := vz.NewMacOSBootLoader()
	if err != nil {
		return nil, "", err
	}
	cfg, err := vz.NewVirtualMachineConfiguration(boot, cpus, mem)
	if err != nil {
		return nil, "", err
	}
	cfg.SetPlatformVirtualMachineConfiguration(plat)
	gfx, err := vz.NewMacGraphicsDeviceConfiguration()
	if err != nil {
		return nil, "", err
	}
	disp, err := vz.NewMacGraphicsDisplayConfiguration(1024, 768, 72)
	if err != nil {
		return nil, "", err
	}
	gfx.SetDisplays(disp)
	cfg.SetGraphicsDevicesVirtualMachineConfiguration([]vz.GraphicsDeviceConfiguration{gfx})
	att, err := vz.NewDiskImageStorageDeviceAttachmentWithCacheAndSync(disk, false, vz.DiskImageCachingModeAutomatic, vz.DiskImageSynchronizationModeFsync)
	if err != nil {
		return nil, "", err
	}
	blk, err := vz.NewVirtioBlockDeviceConfiguration(att)
	if err != nil {
		return nil, "", err
	}
	cfg.SetStorageDevicesVirtualMachineConfiguration([]vz.StorageDeviceConfiguration{blk})
	var netAtt vz.NetworkDeviceAttachment
	if nicFile != nil {
		fh, err := vz.NewFileHandleNetworkDeviceAttachment(nicFile)
		if err != nil {
			return nil, "", err
		}
		netAtt = fh
	} else {
		nat, err := vz.NewNATNetworkDeviceAttachment()
		if err != nil {
			return nil, "", err
		}
		netAtt = nat
	}
	nic, err := vz.NewVirtioNetworkDeviceConfiguration(netAtt)
	if err != nil {
		return nil, "", err
	}
	macAddr, err := vz.NewRandomLocallyAdministeredMACAddress()
	if err != nil {
		return nil, "", err
	}
	nic.SetMACAddress(macAddr)
	cfg.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{nic})
	ent, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err == nil {
		cfg.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{ent})
	}
	kb, err := vz.NewUSBKeyboardConfiguration()
	if err == nil {
		cfg.SetKeyboardsVirtualMachineConfiguration([]vz.KeyboardConfiguration{kb})
	}
	ptr, err := vz.NewUSBScreenCoordinatePointingDeviceConfiguration()
	if err == nil {
		cfg.SetPointingDevicesVirtualMachineConfiguration([]vz.PointingDeviceConfiguration{ptr})
	}
	if ok, err := cfg.Validate(); !ok || err != nil {
		return nil, "", fmt.Errorf("invalid VM config: %v", err)
	}
	vm, err := vz.NewVirtualMachine(cfg)
	if err != nil {
		return nil, "", err
	}
	return vm, macAddr.String(), nil
}

// waitLease finds the guest's DHCP lease: from the kvz pod network's DHCP
// server, or (VZ NAT) from macOS bootpd's lease file.
func (b *Backend) waitLease(ctx context.Context, mac string, timeout time.Duration) (string, error) {
	want := normMAC(mac)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if b.pn != nil {
			if ip, ok := b.pn.LeaseFor(mac); ok {
				return ip, nil
			}
		} else if ip := leaseFor(want); ip != "" {
			return ip, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return "", fmt.Errorf("no DHCP lease for %s after %s", mac, timeout)
}

func normMAC(m string) string {
	parts := strings.Split(strings.ToLower(m), ":")
	for i, p := range parts {
		parts[i] = strings.TrimLeft(p, "0")
		if parts[i] == "" {
			parts[i] = "0"
		}
	}
	return strings.Join(parts, ":")
}

func leaseFor(mac string) string {
	f, err := os.Open("/var/db/dhcpd_leases")
	if err != nil {
		return ""
	}
	defer f.Close()
	var ip, hw string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		switch {
		case l == "{":
			ip, hw = "", ""
		case strings.HasPrefix(l, "ip_address="):
			ip = strings.TrimPrefix(l, "ip_address=")
		case strings.HasPrefix(l, "hw_address="):
			v := strings.TrimPrefix(l, "hw_address=")
			if i := strings.Index(v, ","); i >= 0 {
				v = v[i+1:]
			}
			hw = normMAC(v)
		case l == "}":
			if hw == mac && ip != "" {
				return ip
			}
		}
	}
	return ""
}

// dial reaches the guest: through the pod network when attached to it.
func (b *Backend) dial(ctx context.Context, addr string) (net.Conn, error) {
	dctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if b.pn != nil {
		return b.pn.Dial(dctx, addr)
	}
	var d net.Dialer
	return d.DialContext(dctx, "tcp", addr)
}

// connectSSH waits for the guest's sshd and logs in, without ever closing a
// connection unauthenticated: OpenSSH's PerSourcePenalties (macOS 26)
// penalizes such connections per source address, and every probe from the
// pod network shares one source (the gateway), so banner-probing stalled
// logins by up to 75 s. "connection refused" (sshd not up yet) costs nothing.
func (b *Backend) connectSSH(ctx context.Context, ip string, timeout time.Duration, t0 time.Time) (*ssh.Client, time.Duration, error) {
	cfg := &ssh.ClientConfig{
		User:            b.user,
		Auth:            []ssh.AuthMethod{ssh.Password(b.password)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}
	deadline := time.Now().Add(timeout)
	var tBanner time.Duration
	backoff := 500 * time.Millisecond
	var last error
	for time.Now().Before(deadline) {
		c, err := b.dial(ctx, net.JoinHostPort(ip, "22"))
		if err != nil {
			last = err
			select {
			case <-ctx.Done():
				return nil, 0, ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}
		if tBanner == 0 {
			tBanner = time.Since(t0)
		}
		_ = c.SetDeadline(time.Now().Add(30 * time.Second))
		conn, chans, reqs, err := ssh.NewClientConn(c, ip+":22", cfg)
		if err == nil {
			_ = c.SetDeadline(time.Time{})
			return ssh.NewClient(conn, chans, reqs), tBanner, nil
		}
		c.Close()
		last = err
		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 4*time.Second {
			backoff *= 2
		}
	}
	return nil, 0, fmt.Errorf("ssh to %s: %w", ip, last)
}

func (b *Backend) dialSSH(ctx context.Context, ip string, timeout time.Duration) (*ssh.Client, error) {
	cfg := &ssh.ClientConfig{
		User:            b.user,
		Auth:            []ssh.AuthMethod{ssh.Password(b.password)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // guest is a fresh clone on a host-only NAT
		Timeout:         2 * time.Second,
	}
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		c, err := b.dial(ctx, net.JoinHostPort(ip, "22"))
		if err == nil {
			_ = c.SetDeadline(time.Now().Add(10 * time.Second))
			conn, chans, reqs, err := ssh.NewClientConn(c, ip+":22", cfg)
			_ = c.SetDeadline(time.Time{})
			if err == nil {
				return ssh.NewClient(conn, chans, reqs), nil
			}
			c.Close()
			last = err
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return nil, fmt.Errorf("ssh to %s: %w", ip, last)
}

func (b *Backend) get(pod *corev1.Pod) *instance {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.vms[rt.Key(pod.Namespace, pod.Name)]
}

func (b *Backend) Stop(ctx context.Context, pod *corev1.Pod) error {
	key := rt.Key(pod.Namespace, pod.Name)
	b.mu.Lock()
	inst := b.vms[key]
	delete(b.vms, key)
	b.mu.Unlock()
	if inst == nil {
		return nil
	}
	inst.cancel()
	if inst.vm != nil && inst.vm.State() != vz.VirtualMachineStateStopped {
		// The pod's disk is a throwaway APFS clone: honor the pod's grace
		// period for a guest shutdown, never more, then hard-stop.
		grace := 30 * time.Second
		if pod.Spec.TerminationGracePeriodSeconds != nil {
			grace = time.Duration(*pod.Spec.TerminationGracePeriodSeconds) * time.Second
		}
		if grace > 0 && inst.vm.CanRequestStop() {
			_, _ = inst.vm.RequestStop()
			select {
			case <-waitStopped(inst.vm):
			case <-time.After(grace):
			}
		}
		if inst.vm.State() != vz.VirtualMachineStateStopped {
			_ = inst.vm.Stop()
		}
	}
	<-inst.done
	if inst.nic != nil {
		inst.nic.Close()
	}
	if inst.slot {
		<-b.slots
	}
	return os.RemoveAll(inst.dir)
}

func waitStopped(vm *vz.VirtualMachine) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		for vm.State() != vz.VirtualMachineStateStopped {
			time.Sleep(100 * time.Millisecond)
		}
		close(ch)
	}()
	return ch
}

func (b *Backend) Logs(ctx context.Context, pod *corev1.Pod, container string, opts api.ContainerLogOpts) (io.ReadCloser, error) {
	inst := b.get(pod)
	if inst == nil {
		return nil, fmt.Errorf("pod not found")
	}
	f, err := os.Open(filepath.Join(inst.dir, container+".log"))
	if err != nil {
		return io.NopCloser(strings.NewReader("")), nil
	}
	if !opts.Follow {
		return f, nil
	}
	return rt.Follow(ctx, f), nil
}

func (b *Backend) Exec(ctx context.Context, pod *corev1.Pod, container string, cmd []string, attach api.AttachIO) error {
	inst := b.get(pod)
	if inst == nil || inst.ip == "" {
		return fmt.Errorf("VM not ready")
	}
	cl, err := b.dialSSH(ctx, inst.ip, 10*time.Second)
	if err != nil {
		return err
	}
	defer cl.Close()
	s, err := cl.NewSession()
	if err != nil {
		return err
	}
	defer s.Close()
	if attach.TTY() {
		if err := s.RequestPty("xterm-256color", 40, 120, ssh.TerminalModes{}); err != nil {
			return err
		}
		go func() {
			for sz := range attach.Resize() {
				_ = s.WindowChange(int(sz.Height), int(sz.Width))
			}
		}()
	}
	if attach.Stdin() != nil {
		s.Stdin = attach.Stdin()
	}
	if attach.Stdout() != nil {
		s.Stdout = attach.Stdout()
	}
	if attach.Stderr() != nil {
		s.Stderr = attach.Stderr()
	}
	return s.Run(joinQuoted(cmd))
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func joinQuoted(a []string) string {
	q := make([]string, len(a))
	for i, s := range a {
		q[i] = shellQuote(s)
	}
	return strings.Join(q, " ")
}
