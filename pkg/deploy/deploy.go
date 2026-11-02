// Package deploy is kvz's auto-deploying manifests controller (the k3s
// "server/manifests" equivalent): every *.yaml / *.json file in the manifests
// directory is server-side applied at startup and re-applied when it changes.
package deploy

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/klog/v2"
)

//go:embed traefik.yaml
var traefikYAML []byte

// Bundled components written into the manifests dir unless disabled.
var Bundled = map[string][]byte{"traefik": traefikYAML}

// WriteBundled writes default component manifests (skipping disabled ones and
// removing manifests of components that became disabled).
func WriteBundled(dir string, disabled map[string]bool) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for name, body := range Bundled {
		p := filepath.Join(dir, name+".yaml")
		if disabled[name] {
			_ = os.Remove(p)
			continue
		}
		if err := os.WriteFile(p, body, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// Run applies dir now and whenever a file's content changes.
func Run(ctx context.Context, cfg *rest.Config, dir string) {
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		klog.ErrorS(err, "deploy: dynamic client")
		return
	}
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dc))
	applied := map[string][32]byte{}
	for {
		files, _ := filepath.Glob(filepath.Join(dir, "*"))
		for _, f := range files {
			if !(strings.HasSuffix(f, ".yaml") || strings.HasSuffix(f, ".yml") || strings.HasSuffix(f, ".json")) || strings.HasPrefix(filepath.Base(f), ".") {
				continue
			}
			b, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			sum := sha256.Sum256(b)
			if applied[f] == sum {
				continue
			}
			if err := apply(ctx, dyn, mapper, b); err != nil {
				klog.ErrorS(err, "deploy: apply failed (will retry)", "file", f)
				mapper.Reset() // CRDs may have just appeared
				continue
			}
			applied[f] = sum
			klog.InfoS("deploy: applied", "file", filepath.Base(f))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func apply(ctx context.Context, dyn dynamic.Interface, mapper meta.RESTMapper, doc []byte) error {
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(doc), 4096)
	var firstErr error
	for {
		var obj unstructured.Unstructured
		if err := dec.Decode(&obj.Object); err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		if len(obj.Object) == 0 {
			continue
		}
		gvk := obj.GroupVersionKind()
		m, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		var ri dynamic.ResourceInterface = dyn.Resource(m.Resource)
		if m.Scope.Name() == meta.RESTScopeNameNamespace {
			ns := obj.GetNamespace()
			if ns == "" {
				ns = "default"
			}
			ri = dyn.Resource(m.Resource).Namespace(ns)
		}
		body, err := obj.MarshalJSON()
		if err != nil {
			return err
		}
		force := true
		_, err = ri.Patch(ctx, obj.GetName(), types.ApplyPatchType, body, metav1.PatchOptions{FieldManager: "kvz-deploy", Force: &force})
		if err != nil && !apierrors.IsAlreadyExists(err) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
