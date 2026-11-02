// Package storage is kvz's built-in dynamic provisioner (the equivalent of
// k3s's local-path-provisioner): PVCs of StorageClass "local-path" get a
// node-pinned hostPath PV under /Users/Shared/kvz-storage on the node the
// scheduler picked (WaitForFirstConsumer). The node runtime creates the
// directory on first mount and shares it into the pod VM over virtiofs.
package storage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

const (
	ClassName    = "local-path"
	Provisioner  = "kvz.dev/local-path"
	Root         = "/Users/Shared/kvz-storage"
	selectedNode = "volume.kubernetes.io/selected-node"
)

// EnsureDefaultClass creates the default local-path StorageClass.
func EnsureDefaultClass(ctx context.Context, cs kubernetes.Interface) error {
	mode := storagev1.VolumeBindingWaitForFirstConsumer
	reclaim := corev1.PersistentVolumeReclaimDelete
	sc := &storagev1.StorageClass{
		ObjectMeta:        metav1.ObjectMeta{Name: ClassName, Annotations: map[string]string{"storageclass.kubernetes.io/is-default-class": "true"}},
		Provisioner:       Provisioner,
		VolumeBindingMode: &mode,
		ReclaimPolicy:     &reclaim,
	}
	_, err := cs.StorageV1().StorageClasses().Create(ctx, sc, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

// Run provisions and reclaims local-path volumes. localNode is the server's
// own node name (directories of deleted PVs are removed when local).
func Run(ctx context.Context, cs kubernetes.Interface, localNode string) {
	f := informers.NewSharedInformerFactory(cs, 10*time.Minute)
	pvcInf := f.Core().V1().PersistentVolumeClaims()
	pvInf := f.Core().V1().PersistentVolumes()
	scInf := f.Storage().V1().StorageClasses()
	kick := make(chan struct{}, 1)
	nudge := func(any) {
		select {
		case kick <- struct{}{}:
		default:
		}
	}
	h := cache.ResourceEventHandlerFuncs{AddFunc: nudge, UpdateFunc: func(_, o any) { nudge(o) }, DeleteFunc: nudge}
	_, _ = pvcInf.Informer().AddEventHandler(h)
	_, _ = pvInf.Informer().AddEventHandler(h)
	_ = scInf.Informer() // register before Start, or its lister stays empty
	f.Start(ctx.Done())
	f.WaitForCacheSync(ctx.Done())
	_ = scInf
	for {
		select {
		case <-ctx.Done():
			return
		case <-kick:
		case <-time.After(30 * time.Second):
		}
		pvcs, _ := pvcInf.Lister().List(labels.Everything())
		for _, pvc := range pvcs {
			if pvc.Spec.VolumeName != "" || pvc.Status.Phase == corev1.ClaimBound {
				continue
			}
			class := ClassName
			if pvc.Spec.StorageClassName != nil {
				class = *pvc.Spec.StorageClassName
			}
			if sc, err := scInf.Lister().Get(class); err != nil || sc.Provisioner != Provisioner {
				continue
			}
			node := pvc.Annotations[selectedNode]
			if node == "" {
				continue // WaitForFirstConsumer: the scheduler has not picked a node yet
			}
			name := "pvc-" + string(pvc.UID)
			if _, err := pvInf.Lister().Get(name); err == nil {
				continue
			}
			size := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
			if size.IsZero() {
				size = resource.MustParse("1Gi")
			}
			hp := corev1.HostPathDirectoryOrCreate
			reclaim := corev1.PersistentVolumeReclaimDelete
			pv := &corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: map[string]string{"pv.kubernetes.io/provisioned-by": Provisioner}},
				Spec: corev1.PersistentVolumeSpec{
					Capacity:                      corev1.ResourceList{corev1.ResourceStorage: size},
					AccessModes:                   pvc.Spec.AccessModes,
					PersistentVolumeReclaimPolicy: reclaim,
					StorageClassName:              class,
					VolumeMode:                    pvc.Spec.VolumeMode,
					ClaimRef:                      &corev1.ObjectReference{Kind: "PersistentVolumeClaim", Namespace: pvc.Namespace, Name: pvc.Name, UID: pvc.UID, APIVersion: "v1"},
					PersistentVolumeSource: corev1.PersistentVolumeSource{HostPath: &corev1.HostPathVolumeSource{
						Path: filepath.Join(Root, pvc.Namespace+"_"+pvc.Name+"_"+name), Type: &hp}},
					NodeAffinity: &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
						MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "kubernetes.io/hostname", Operator: corev1.NodeSelectorOpIn, Values: []string{node}}}}}}},
				},
			}
			if _, err := cs.CoreV1().PersistentVolumes().Create(ctx, pv, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
				klog.ErrorS(err, "local-path: create PV", "pvc", pvc.Namespace+"/"+pvc.Name)
			} else {
				klog.InfoS("local-path: provisioned", "pv", name, "node", node)
			}
		}
		// Reclaim released PVs.
		pvs, _ := pvInf.Lister().List(labels.Everything())
		for _, pv := range pvs {
			if pv.Annotations["pv.kubernetes.io/provisioned-by"] != Provisioner || pv.Status.Phase != corev1.VolumeReleased ||
				pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimDelete {
				continue
			}
			if pv.Spec.HostPath != nil && nodeOf(pv) == localNode {
				_ = os.RemoveAll(pv.Spec.HostPath.Path)
			} // other nodes remove their own data in GCOrphans
			if err := cs.CoreV1().PersistentVolumes().Delete(ctx, pv.Name, metav1.DeleteOptions{}); err == nil {
				klog.InfoS("local-path: reclaimed", "pv", pv.Name)
			}
		}
	}
}

func nodeOf(pv *corev1.PersistentVolume) string {
	if pv.Spec.NodeAffinity == nil || pv.Spec.NodeAffinity.Required == nil {
		return ""
	}
	for _, t := range pv.Spec.NodeAffinity.Required.NodeSelectorTerms {
		for _, e := range t.MatchExpressions {
			if e.Key == "kubernetes.io/hostname" && len(e.Values) > 0 {
				return e.Values[0]
			}
		}
	}
	return ""
}

// GCOrphans runs on every node: it deletes local-path directories under Root
// whose PersistentVolume no longer exists (volumes released on this node while
// the reclaiming server was another machine).
func GCOrphans(ctx context.Context, cs kubernetes.Interface) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
		pvs, err := cs.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
		if err != nil {
			continue
		}
		live := map[string]bool{}
		for _, pv := range pvs.Items {
			if pv.Spec.HostPath != nil {
				live[filepath.Clean(pv.Spec.HostPath.Path)] = true
			}
		}
		entries, _ := os.ReadDir(Root)
		for _, e := range entries {
			// only directories this provisioner created: <ns>_<pvc>_pvc-<uid>
			if !e.IsDir() || !strings.Contains(e.Name(), "_pvc-") {
				continue
			}
			p := filepath.Join(Root, e.Name())
			if live[p] {
				continue
			}
			if st, err := os.Stat(p); err == nil && time.Since(st.ModTime()) < 2*time.Minute {
				continue // just provisioned; the PV may not be in our list yet
			}
			if err := os.RemoveAll(p); err == nil {
				klog.InfoS("local-path: removed orphaned volume data", "path", p)
			}
		}
	}
}
