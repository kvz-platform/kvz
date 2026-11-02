package microvmrt

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"github.com/GrowlyX/kvz/pkg/image"
	rt "github.com/GrowlyX/kvz/pkg/runtime"
)

// Ephemeral containers (kubectl debug): added to a running pod through the
// pods/ephemeralcontainers subresource. The image is pulled on the host (the
// guest sees the whole image store), then kvz-init builds an overlay root and
// starts the process inside the running VM. They share the VM's process,
// network and IPC view with every other container, and never restart.

func (b *Backend) startEphemerals(p *corev1.Pod, ps *pod) {
	for _, ec := range p.Spec.EphemeralContainers {
		ps.ephMu.Lock()
		if ps.ephemeral == nil {
			ps.ephemeral = map[string]bool{}
		}
		started := ps.ephemeral[ec.Name]
		ps.ephemeral[ec.Name] = true
		ps.ephMu.Unlock()
		if started {
			continue
		}
		go func(ec corev1.EphemeralContainer) {
			if err := b.startEphemeral(p, ps, ec); err != nil {
				klog.ErrorS(err, "ephemeral container", "pod", rt.Key(p.Namespace, p.Name), "container", ec.Name)
				b.t.Event(p.Namespace, p.Name, corev1.EventTypeWarning, "Failed", "Error: %v", err)
				b.t.Update(p.Namespace, p.Name, func(s *corev1.PodStatus) {
					rt.SetWaiting(s, ec.Name, "CreateContainerError", err.Error())
				})
			}
		}(ec)
	}
}

func (b *Backend) startEphemeral(p *corev1.Pod, ps *pod, ec corev1.EphemeralContainer) error {
	b.t.Update(p.Namespace, p.Name, func(s *corev1.PodStatus) {
		s.EphemeralContainerStatuses = append(s.EphemeralContainerStatuses, corev1.ContainerStatus{
			Name: ec.Name, Image: ec.Image, ImageID: ec.Image,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}})
	})
	policy := string(ec.ImagePullPolicy)
	if policy == "" {
		policy = image.PullIfNotPresent
	}
	var payloads []map[string][]byte
	for _, ref := range p.Spec.ImagePullSecrets {
		if b.vols.Secrets != nil {
			if sec, err := b.vols.Secrets.Secrets(p.Namespace).Get(ref.Name); err == nil {
				payloads = append(payloads, sec.Data)
			}
		}
	}
	if !b.images.Has(ec.Image) {
		b.t.Event(p.Namespace, p.Name, corev1.EventTypeNormal, "Pulling", "Pulling image %q", ec.Image)
	}
	im, err := b.images.Ensure(ec.Image, policy, image.Keychain(payloads))
	if err != nil {
		return fmt.Errorf("pull %s: %w", ec.Image, err)
	}
	c := corev1.Container{Name: ec.Name, Image: ec.Image, Command: ec.Command, Args: ec.Args, WorkingDir: ec.WorkingDir,
		Env: ec.Env, VolumeMounts: ec.VolumeMounts, SecurityContext: ec.SecurityContext, Stdin: ec.Stdin, TTY: ec.TTY,
		TerminationMessagePath: ec.TerminationMessagePath, TerminationMessagePolicy: ec.TerminationMessagePolicy}
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
		return fmt.Errorf("no command")
	}
	cwd := im.WorkingDir
	if c.WorkingDir != "" {
		cwd = c.WorkingDir
	}
	uid, gid := userIDs(im, c, p.Spec.SecurityContext)
	cs := cspec{Name: c.Name, Argv: argv, Env: mergeEnv(im.Env, rt.Env(c)), Cwd: cwd, UID: uid, GID: gid,
		ImageTag: b.imageLower(im), Restart: "Never", TTY: c.TTY, Stdin: c.Stdin, TermPath: "/dev/termination-log"}
	if sc := c.SecurityContext; sc != nil {
		cs.Privileged = sc.Privileged != nil && *sc.Privileged
		if sc.Capabilities != nil {
			for _, a := range sc.Capabilities.Add {
				cs.CapAdd = append(cs.CapAdd, string(a))
			}
			for _, d := range sc.Capabilities.Drop {
				cs.CapDrop = append(cs.CapDrop, string(d))
			}
		}
	}
	// volumes the debug container mounts: the pod's materialized volume dirs
	var mounts []mspec
	for _, vm := range c.VolumeMounts {
		for _, v := range p.Spec.Volumes {
			if v.Name != vm.Name || v.HostPath != nil || v.PersistentVolumeClaim != nil {
				continue
			}
			sub := filepath.Join("volumes", v.Name, vm.SubPath)
			mounts = append(mounts, mspec{Container: c.Name, Sub: sub, Path: vm.MountPath, ReadOnly: vm.ReadOnly})
		}
	}
	b.mu.Lock()
	vm := ps.vm
	b.mu.Unlock()
	if vm == nil {
		return fmt.Errorf("pod VM is not running")
	}
	return vm.Send(map[string]any{"type": "add-container", "spec": cs, "mounts": mounts})
}

func isEphemeral(p *corev1.Pod, name string) bool {
	for _, ec := range p.Spec.EphemeralContainers {
		if ec.Name == name {
			return true
		}
	}
	return false
}

// AttachTTY attaches to the pty of an interactive (tty) container. It reports
// handled=false for other containers, which get log-following attach.
func (b *Backend) AttachTTY(ctx context.Context, p *corev1.Pod, container string, attach api.AttachIO) (bool, error) {
	cur := p
	b.mu.Lock()
	ps := b.pods[rt.Key(p.Namespace, p.Name)]
	b.mu.Unlock()
	if ps == nil || ps.vm == nil {
		return false, nil
	}
	if lp := ps.latest.Load(); lp != nil {
		cur = lp
	}
	tty := false
	for _, ec := range cur.Spec.EphemeralContainers {
		if ec.Name == container {
			tty = ec.TTY
		}
	}
	if !tty {
		return false, nil
	}
	return true, execInVM(ctx, ps.vm, container, []string{"__kvz_attach"}, attach)
}
