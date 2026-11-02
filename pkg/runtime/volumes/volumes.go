// Package volumes materializes pod volumes (emptyDir, hostPath, configMap,
// secret) as host directories that backends bind-mount.
package volumes

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	authv1 "k8s.io/api/authentication/v1"
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
		case v.Projected != nil:
			files, err := m.projected(pod, v.Projected)
			if err != nil {
				return nil, err
			}
			if err := write(dst, files, nil); err != nil {
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

// projected materializes a projected volume: service-account tokens (via the
// TokenRequest API, bound to the pod), configMaps, secrets and the downward
// API's namespace/name fields. This is what kube-api-access-* volumes need.
func (m *Materializer) projected(pod *corev1.Pod, pv *corev1.ProjectedVolumeSource) (map[string][]byte, error) {
	files := map[string][]byte{}
	for _, src := range pv.Sources {
		switch {
		case src.ServiceAccountToken != nil && m.Client != nil:
			t := src.ServiceAccountToken
			exp := int64(24 * 3600)
			if t.ExpirationSeconds != nil && *t.ExpirationSeconds > exp {
				exp = *t.ExpirationSeconds
			}
			req := &authv1.TokenRequest{Spec: authv1.TokenRequestSpec{
				ExpirationSeconds: &exp,
				BoundObjectRef:    &authv1.BoundObjectReference{Kind: "Pod", APIVersion: "v1", Name: pod.Name, UID: pod.UID},
			}}
			if t.Audience != "" {
				req.Spec.Audiences = []string{t.Audience}
			}
			sa := pod.Spec.ServiceAccountName
			if sa == "" {
				sa = "default"
			}
			// The Node authorizer learns pod->node bindings from an informer; right
			// after scheduling it can still deny the request. Retry like the kubelet.
			var tr *authv1.TokenRequest
			var err error
			for delay := 5 * time.Millisecond; ; delay *= 2 {
				tr, err = m.Client.CoreV1().ServiceAccounts(pod.Namespace).CreateToken(context.Background(), sa, req, metav1.CreateOptions{})
				if err == nil || delay > 2*time.Second {
					break
				}
				time.Sleep(delay)
			}
			if err != nil {
				return nil, fmt.Errorf("service account token: %w", err)
			}
			files[t.Path] = []byte(tr.Status.Token)
		case src.ConfigMap != nil && m.ConfigMaps != nil:
			cm, err := m.ConfigMaps.ConfigMaps(pod.Namespace).Get(src.ConfigMap.Name)
			if err != nil {
				if src.ConfigMap.Optional != nil && *src.ConfigMap.Optional {
					continue
				}
				return nil, fmt.Errorf("configmap %s: %w", src.ConfigMap.Name, err)
			}
			all := map[string][]byte{}
			for k, v := range cm.Data {
				all[k] = []byte(v)
			}
			for k, v := range cm.BinaryData {
				all[k] = v
			}
			pick(files, all, src.ConfigMap.Items)
		case src.Secret != nil && m.Secrets != nil:
			sec, err := m.Secrets.Secrets(pod.Namespace).Get(src.Secret.Name)
			if err != nil {
				if src.Secret.Optional != nil && *src.Secret.Optional {
					continue
				}
				return nil, fmt.Errorf("secret %s: %w", src.Secret.Name, err)
			}
			pick(files, sec.Data, src.Secret.Items)
		case src.DownwardAPI != nil:
			for _, it := range src.DownwardAPI.Items {
				if it.FieldRef == nil {
					continue
				}
				switch it.FieldRef.FieldPath {
				case "metadata.namespace":
					files[it.Path] = []byte(pod.Namespace)
				case "metadata.name":
					files[it.Path] = []byte(pod.Name)
				case "metadata.uid":
					files[it.Path] = []byte(pod.UID)
				}
			}
		}
	}
	return files, nil
}

func pick(dst, src map[string][]byte, items []corev1.KeyToPath) {
	if len(items) == 0 {
		for k, v := range src {
			dst[k] = v
		}
		return
	}
	for _, it := range items {
		if v, ok := src[it.Key]; ok {
			dst[it.Path] = v
		}
	}
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
