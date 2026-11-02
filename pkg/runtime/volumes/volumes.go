// Package volumes materializes pod volumes (emptyDir, hostPath, configMap,
// secret) as host directories that backends bind-mount.
package volumes

import (
	"fmt"
	"os"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
)

type Materializer struct {
	ConfigMaps corev1listers.ConfigMapLister
	Secrets    corev1listers.SecretLister
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
		case v.ConfigMap != nil && m.ConfigMaps != nil:
			cm, err := m.ConfigMaps.ConfigMaps(pod.Namespace).Get(v.ConfigMap.Name)
			if err != nil {
				if v.ConfigMap.Optional != nil && *v.ConfigMap.Optional {
					_ = os.MkdirAll(dst, 0o755)
					break
				}
				return nil, fmt.Errorf("configmap %s: %w", v.ConfigMap.Name, err)
			}
			files := map[string][]byte{}
			for k, s := range cm.Data {
				files[k] = []byte(s)
			}
			for k, b := range cm.BinaryData {
				files[k] = b
			}
			if err := write(dst, files, v.ConfigMap.Items); err != nil {
				return nil, err
			}
		case v.Secret != nil && m.Secrets != nil:
			s, err := m.Secrets.Secrets(pod.Namespace).Get(v.Secret.SecretName)
			if err != nil {
				if v.Secret.Optional != nil && *v.Secret.Optional {
					_ = os.MkdirAll(dst, 0o700)
					break
				}
				return nil, fmt.Errorf("secret %s: %w", v.Secret.SecretName, err)
			}
			if err := write(dst, s.Data, v.Secret.Items); err != nil {
				return nil, err
			}
		default:
			continue // projected SA tokens etc. are not supported yet
		}
		paths[v.Name] = dst
	}
	out := map[string][]string{}
	for _, c := range pod.Spec.Containers {
		for _, vm := range c.VolumeMounts {
			p, ok := paths[vm.Name]
			if !ok {
				continue
			}
			if vm.SubPath != "" {
				p = filepath.Join(p, vm.SubPath)
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

func write(dir string, files map[string][]byte, items []corev1.KeyToPath) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if len(items) > 0 {
		sel := map[string][]byte{}
		for _, it := range items {
			if b, ok := files[it.Key]; ok {
				sel[it.Path] = b
			}
		}
		files = sel
	}
	for name, b := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			return err
		}
	}
	return nil
}
