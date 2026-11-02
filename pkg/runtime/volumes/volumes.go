// Package volumes materializes pod volumes (emptyDir, hostPath, configMap,
// secret) as host directories that backends bind-mount.
package volumes

import (
	"context"
	"fmt"
	"github.com/GrowlyX/kvz/pkg/downward"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
)

type Materializer struct {
	ConfigMaps corev1listers.ConfigMapLister
	Secrets    corev1listers.SecretLister
	Client     kubernetes.Interface // for projected service-account tokens
}

// Materialize writes volume contents under podDir/volumes and returns, per
// container, "host:guest[:ro]" mount specs.
func (m *Materializer) Materialize(pod *corev1.Pod, podDir string) (map[string][]string, error) {
	paths := map[string]string{}
	for _, v := range pod.Spec.Volumes {
		dst := filepath.Join(podDir, "volumes", v.Name)
		switch {
		case v.EmptyDir != nil:
			if err := os.MkdirAll(dst, 0o777); err != nil {
				return nil, err
			}
		case v.HostPath != nil:
			dst = v.HostPath.Path
			if t := v.HostPath.Type; t != nil && (*t == corev1.HostPathDirectoryOrCreate) {
				_ = os.MkdirAll(dst, 0o777)
			}
		case v.PersistentVolumeClaim != nil && m.Client != nil:
			path, err := m.pvcPath(pod.Namespace, v.PersistentVolumeClaim.ClaimName)
			if err != nil {
				return nil, err
			}
			dst = path
		case v.ConfigMap != nil || v.Secret != nil || v.DownwardAPI != nil || v.Projected != nil:
			c, err := m.render(pod, v, dst, false)
			if err != nil {
				return nil, err
			}
			if err := syncDir(dst, c); err != nil {
				return nil, err
			}
		default:
			continue
		}
		paths[v.Name] = dst
	}
	out := map[string][]string{}
	for _, c := range append(append([]corev1.Container{}, pod.Spec.InitContainers...), pod.Spec.Containers...) {
		for _, vm := range c.VolumeMounts {
			p, ok := paths[vm.Name]
			if !ok {
				continue
			}
			sub := vm.SubPath
			if vm.SubPathExpr != "" {
				sub = expandEnv(vm.SubPathExpr, c.Env)
			}
			if sub != "" {
				if filepath.IsAbs(sub) {
					return nil, fmt.Errorf("failed to prepare subPath for volumeMount %q of container %q: subpath %q must not be absolute", vm.Name, c.Name, sub)
				}
				for _, part := range strings.Split(filepath.ToSlash(sub), "/") {
					if part == ".." {
						return nil, fmt.Errorf("failed to prepare subPath for volumeMount %q of container %q: subpath %q must not contain '..'", vm.Name, c.Name, sub)
					}
				}
				p = filepath.Join(p, sub)
				if _, err := os.Lstat(p); os.IsNotExist(err) {
					_ = os.MkdirAll(p, 0o777) // the kubelet creates missing subpaths as directories
				}
			}
			spec := p + ":" + vm.MountPath
			if vm.ReadOnly {
				spec += ":ro"
			}
			out[c.Name] = append(out[c.Name], spec)
		}
	}
	return out, nil
}

// pvcPath resolves a bound PVC to the host directory backing its PV
// (local-path / hostPath / local volumes).
func (m *Materializer) pvcPath(ns, claim string) (string, error) {
	ctx := context.Background()
	var pvc *corev1.PersistentVolumeClaim
	var err error
	for i := 0; i < 50; i++ { // binding can lag scheduling slightly
		pvc, err = m.Client.CoreV1().PersistentVolumeClaims(ns).Get(ctx, claim, metav1.GetOptions{})
		if err == nil && pvc.Spec.VolumeName != "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		return "", fmt.Errorf("pvc %s: %w", claim, err)
	}
	if pvc.Spec.VolumeName == "" {
		return "", fmt.Errorf("pvc %s is not bound", claim)
	}
	pv, err := m.Client.CoreV1().PersistentVolumes().Get(ctx, pvc.Spec.VolumeName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("pv %s: %w", pvc.Spec.VolumeName, err)
	}
	var path string
	switch {
	case pv.Spec.HostPath != nil:
		path = pv.Spec.HostPath.Path
	case pv.Spec.Local != nil:
		path = pv.Spec.Local.Path
	default:
		return "", fmt.Errorf("pv %s: unsupported volume source (kvz supports hostPath/local)", pv.Name)
	}
	if err := os.MkdirAll(path, 0o777); err != nil {
		return "", err
	}
	return path, nil
}

// downwardFiles renders downwardAPI volume items.
func downwardFiles(pod *corev1.Pod, items []corev1.DownwardAPIVolumeFile) map[string][]byte {
	out := map[string][]byte{}
	kv := func(m map[string]string) []byte {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		for _, k := range keys {
			b.WriteString(k + "=" + strconv.Quote(m[k]) + "\n")
		}
		return []byte(b.String())
	}
	for _, it := range items {
		switch {
		case it.FieldRef != nil:
			switch it.FieldRef.FieldPath {
			case "metadata.name":
				out[it.Path] = []byte(pod.Name)
			case "metadata.namespace":
				out[it.Path] = []byte(pod.Namespace)
			case "metadata.uid":
				out[it.Path] = []byte(pod.UID)
			case "metadata.labels":
				out[it.Path] = kv(pod.Labels)
			case "metadata.annotations":
				out[it.Path] = kv(pod.Annotations)
			}
		case it.ResourceFieldRef != nil:
			if v, err := downward.Resource(it.ResourceFieldRef, pod, it.ResourceFieldRef.ContainerName); err == nil {
				out[it.Path] = []byte(v)
			}
		}
	}
	return out
}

// expandEnv expands $(VAR) references like the kubelet (unknown vars stay as is,
// $$ escapes).
func expandEnv(in string, env []corev1.EnvVar) string {
	vals := map[string]string{}
	for _, e := range env {
		vals[e.Name] = e.Value
	}
	var b strings.Builder
	for i := 0; i < len(in); i++ {
		if in[i] == '$' && i+1 < len(in) {
			if in[i+1] == '$' {
				b.WriteByte('$')
				i++
				continue
			}
			if in[i+1] == '(' {
				if j := strings.IndexByte(in[i+2:], ')'); j >= 0 {
					name := in[i+2 : i+2+j]
					if v, ok := vals[name]; ok {
						b.WriteString(v)
					} else {
						b.WriteString(in[i : i+3+j])
					}
					i += 2 + j
					continue
				}
			}
		}
		b.WriteByte(in[i])
	}
	return b.String()
}
