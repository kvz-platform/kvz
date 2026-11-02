//go:build darwin

package microvmrt

import (
	"context"
	"net"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"github.com/GrowlyX/kvz/pkg/image"
	rt "github.com/GrowlyX/kvz/pkg/runtime"
)

// ensureImages resolves every container image under its pull policy and the
// pod's imagePullSecrets. Failures surface as ErrImagePull/ImagePullBackOff
// and are retried with backoff until success or pod deletion (kubelet
// semantics) instead of failing the pod.
func (b *Backend) ensureImages(ctx context.Context, p *corev1.Pod, ps *pod) error {
	var payloads []map[string][]byte
	for _, ref := range p.Spec.ImagePullSecrets {
		if b.vols.Secrets == nil {
			break
		}
		if sec, err := b.vols.Secrets.Secrets(p.Namespace).Get(ref.Name); err == nil {
			payloads = append(payloads, sec.Data)
		}
	}
	kc := image.Keychain(payloads)
	ps.images = map[string]*image.Image{}
	all := append(append([]corev1.Container{}, p.Spec.InitContainers...), p.Spec.Containers...)
	backoff := time.Second
	for {
		var failed []string
		var lastErr error
		for _, c := range all {
			if ps.images[c.Image] != nil {
				continue
			}
			policy := string(c.ImagePullPolicy)
			if policy == "" {
				policy = image.PullIfNotPresent
			}
			t0 := time.Now()
			fresh := policy == image.PullAlways || !b.images.Has(c.Image)
			if fresh {
				b.t.Event(p.Namespace, p.Name, corev1.EventTypeNormal, "Pulling", "Pulling image %q", c.Image)
			}
			im, err := b.images.Ensure(c.Image, policy, kc)
			if err != nil {
				failed, lastErr = append(failed, c.Name), err
				b.t.Event(p.Namespace, p.Name, corev1.EventTypeWarning, "Failed", "Failed to pull image %q: %v", c.Image, err)
				continue
			}
			if fresh {
				b.t.Event(p.Namespace, p.Name, corev1.EventTypeNormal, "Pulled", "Successfully pulled image %q in %s", c.Image, time.Since(t0).Round(time.Millisecond))
			} else {
				b.t.Event(p.Namespace, p.Name, corev1.EventTypeNormal, "Pulled", "Container image %q already present on machine", c.Image)
			}
			ps.images[c.Image] = im
		}
		if len(failed) == 0 {
			return nil
		}
		reason := "ErrImagePull"
		if backoff > time.Second {
			reason = "ImagePullBackOff"
		}
		klog.InfoS("image pull failed", "pod", rt.Key(p.Namespace, p.Name), "err", lastErr)
		b.t.Update(p.Namespace, p.Name, func(s *corev1.PodStatus) {
			for _, n := range failed {
				rt.SetWaiting(s, n, reason, lastErr.Error())
			}
		})
		if reason == "ImagePullBackOff" {
			b.t.Event(p.Namespace, p.Name, corev1.EventTypeNormal, "BackOff", "Back-off pulling image")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 5*time.Minute {
			backoff *= 2
		}
	}
}

// forwardHostPorts exposes hostPort mappings on the Mac (TCP and UDP, on the
// requested hostIP), proxied into the pod over the pod network. hostNetwork
// pods are handled dynamically: every socket they listen on is exposed (see
// hostnet.go).
func (b *Backend) forwardHostPorts(p *corev1.Pod, ps *pod) {
	if p.Spec.HostNetwork {
		return
	}
	all := append(append([]corev1.Container{}, p.Spec.InitContainers...), p.Spec.Containers...)
	for _, c := range all {
		for _, cp := range c.Ports {
			if cp.HostPort == 0 {
				continue
			}
			proto := "tcp"
			if cp.Protocol == corev1.ProtocolUDP {
				proto = "udp"
			} else if cp.Protocol == corev1.ProtocolSCTP {
				continue
			}
			bind := net.JoinHostPort(cp.HostIP, strconv.Itoa(int(cp.HostPort)))
			target := net.JoinHostPort(ps.ip.String(), strconv.Itoa(int(cp.ContainerPort)))
			cl, err := b.exposeOnHost(proto, bind, target)
			if err != nil {
				klog.ErrorS(err, "hostPort", "pod", rt.Key(p.Namespace, p.Name), "port", cp.HostPort, "proto", proto)
				continue
			}
			ps.listeners = append(ps.listeners, cl)
		}
	}
}
