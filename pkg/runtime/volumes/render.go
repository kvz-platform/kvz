package volumes

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// content is the rendered file set of a configMap, secret, downwardAPI or
// projected volume.
type content struct {
	files map[string][]byte
	modes map[string]os.FileMode
	def   *int32
}

func newContent(def *int32) *content {
	return &content{files: map[string][]byte{}, modes: map[string]os.FileMode{}, def: def}
}

// add selects keys like the kubelet: all keys without items; with items, a
// missing key fails the mount unless the source is optional.
func (c *content) add(kind string, data map[string][]byte, items []corev1.KeyToPath, optional bool) error {
	if len(items) == 0 {
		for k, b := range data {
			c.files[k] = b
		}
		return nil
	}
	for _, it := range items {
		b, ok := data[it.Key]
		if !ok {
			if optional {
				continue
			}
			return fmt.Errorf("%s references non-existent %s key: %s", kind, map[string]string{"configmap": "config", "secret": "secret"}[kind], it.Key)
		}
		c.files[it.Path] = b
		if it.Mode != nil {
			c.modes[it.Path] = os.FileMode(*it.Mode)
		}
	}
	return nil
}

func cmData(data map[string]string, bin map[string][]byte) map[string][]byte {
	out := make(map[string][]byte, len(data)+len(bin))
	for k, s := range data {
		out[k] = []byte(s)
	}
	for k, b := range bin {
		out[k] = b
	}
	return out
}

func isOpt(b *bool) bool { return b != nil && *b }

func (m *Materializer) configMap(ns, name string, items []corev1.KeyToPath, optional bool, c *content) error {
	if m.ConfigMaps == nil {
		return nil
	}
	cm, err := m.ConfigMaps.ConfigMaps(ns).Get(name)
	if err != nil {
		if optional && apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("configmap %q not found", name)
	}
	return c.add("configmap", cmData(cm.Data, cm.BinaryData), items, optional)
}

func (m *Materializer) secret(ns, name string, items []corev1.KeyToPath, optional bool, c *content) error {
	if m.Secrets == nil {
		return nil
	}
	s, err := m.Secrets.Secrets(ns).Get(name)
	if err != nil {
		if optional && apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("secret %q not found", name)
	}
	return c.add("secret", s.Data, items, optional)
}

func (c *content) downward(pod *corev1.Pod, items []corev1.DownwardAPIVolumeFile) {
	for k, b := range downwardFiles(pod, items) {
		c.files[k] = b
	}
	for _, it := range items {
		if it.Mode != nil {
			c.modes[it.Path] = os.FileMode(*it.Mode)
		}
	}
}

// render computes a volume's files. On refresh, service-account tokens are
// reused from dst until 80% of their lifetime has passed.
func (m *Materializer) render(pod *corev1.Pod, v corev1.Volume, dst string, refresh bool) (*content, error) {
	switch {
	case v.ConfigMap != nil:
		c := newContent(v.ConfigMap.DefaultMode)
		return c, m.configMap(pod.Namespace, v.ConfigMap.Name, v.ConfigMap.Items, isOpt(v.ConfigMap.Optional), c)
	case v.Secret != nil:
		c := newContent(v.Secret.DefaultMode)
		return c, m.secret(pod.Namespace, v.Secret.SecretName, v.Secret.Items, isOpt(v.Secret.Optional), c)
	case v.DownwardAPI != nil:
		c := newContent(v.DownwardAPI.DefaultMode)
		c.downward(pod, v.DownwardAPI.Items)
		return c, nil
	case v.Projected != nil:
		c := newContent(v.Projected.DefaultMode)
		for _, src := range v.Projected.Sources {
			var err error
			switch {
			case src.ConfigMap != nil:
				err = m.configMap(pod.Namespace, src.ConfigMap.Name, src.ConfigMap.Items, isOpt(src.ConfigMap.Optional), c)
			case src.Secret != nil:
				err = m.secret(pod.Namespace, src.Secret.Name, src.Secret.Items, isOpt(src.Secret.Optional), c)
			case src.DownwardAPI != nil:
				c.downward(pod, src.DownwardAPI.Items)
			case src.ServiceAccountToken != nil && m.Client != nil:
				err = m.token(pod, src.ServiceAccountToken, dst, refresh, c)
			}
			if err != nil {
				return nil, err
			}
		}
		return c, nil
	}
	return newContent(nil), nil
}

func (m *Materializer) token(pod *corev1.Pod, t *corev1.ServiceAccountTokenProjection, dst string, refresh bool, c *content) error {
	exp := int64(24 * 3600)
	if t.ExpirationSeconds != nil && *t.ExpirationSeconds > exp {
		exp = *t.ExpirationSeconds
	}
	if refresh {
		p := filepath.Join(dst, t.Path)
		if st, err := os.Stat(p); err == nil && time.Since(st.ModTime()) < time.Duration(exp)*time.Second*8/10 {
			if b, err := os.ReadFile(p); err == nil {
				c.files[t.Path] = b
				return nil
			}
		}
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
		return fmt.Errorf("service account token: %w", err)
	}
	c.files[t.Path] = []byte(tr.Status.Token)
	return nil
}

// syncDir makes dir contain exactly c's files (atomic per-file replacement,
// unchanged files untouched, stale files removed).
func syncDir(dir string, c *content) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if c.def != nil {
		mode = os.FileMode(*c.def)
	}
	for name, b := range c.files {
		if strings.Contains(name, "..") || filepath.IsAbs(name) {
			continue
		}
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		m := mode
		if fm, ok := c.modes[name]; ok {
			m = fm
		}
		if old, err := os.ReadFile(p); err == nil && string(old) == string(b) {
			if st, err := os.Stat(p); err == nil && st.Mode().Perm() != m.Perm() {
				_ = os.Chmod(p, m)
			}
			continue
		}
		tmp := p + ".kvz-tmp"
		if err := os.WriteFile(tmp, b, 0o600); err != nil {
			return err
		}
		_ = os.Chmod(tmp, m)
		if err := os.Rename(tmp, p); err != nil { // atomic for readers in the guest
			return err
		}
	}
	// prune files that are no longer part of the volume, then empty dirs
	var dirs []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == dir {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		if d.IsDir() {
			dirs = append(dirs, p)
			return nil
		}
		if _, keep := c.files[filepath.ToSlash(rel)]; !keep {
			_ = os.Remove(p)
		}
		return nil
	})
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	for _, d := range dirs {
		_ = os.Remove(d) // fails (kept) when not empty
	}
	return nil
}

// Refresh re-renders configMap, secret, downwardAPI and projected volumes of a
// running pod (the kubelet's periodic volume sync).
func (m *Materializer) Refresh(pod *corev1.Pod, podDir string) {
	for _, v := range pod.Spec.Volumes {
		if v.ConfigMap == nil && v.Secret == nil && v.DownwardAPI == nil && v.Projected == nil {
			continue
		}
		dst := filepath.Join(podDir, "volumes", v.Name)
		if c, err := m.render(pod, v, dst, true); err == nil {
			_ = syncDir(dst, c)
		}
	}
}
