package upgrade

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/GrowlyX/kvz/pkg/version"
)

func TestReleaseNumbers(t *testing.T) {
	for _, c := range []struct {
		cur, next string
		down      bool
	}{
		{"v1.35.10-12", "v1.35.11-13", false},
		{"v1.35.11-13", "v1.35.10-12", true},
		{"v1.35.10-dev+abc", "v1.35.10-3", false},
		{"v1.35.10-3", "v1.35.10-dev+abc", false},
		{"", "v1.35.10-3", false},
	} {
		if got := isDowngrade(c.cur, c.next); got != c.down {
			t.Errorf("isDowngrade(%q, %q) = %v", c.cur, c.next, got)
		}
	}
	if got := ReleaseURL("", "v1.35.10-2"); got != "https://github.com/kvz-platform/kvz/releases/download/v1.35.10-2/kvz-v1.35.10-2-darwin-arm64.tar.gz" {
		t.Errorf("ReleaseURL = %s", got)
	}
}

func TestDurationJSON(t *testing.T) {
	var d struct{ T Duration }
	if err := json.Unmarshal([]byte(`{"T":"90s"}`), &d); err != nil || d.T.Duration != 90*time.Second {
		t.Fatalf("string: %v %v", d.T, err)
	}
	if err := json.Unmarshal([]byte(`{"T":3000000000}`), &d); err != nil || d.T.Duration != 3*time.Second {
		t.Fatalf("nanoseconds: %v %v", d.T, err)
	}
}

func TestResolveChannel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			http.Redirect(w, r, "/releases/tag/v1.35.10-7", http.StatusFound)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()
	hc := NewController(nil, nil).http
	v, err := ResolveChannel(context.Background(), hc, srv.URL+"/releases/latest")
	if err != nil || v != "v1.35.10-7" {
		t.Fatalf("got %q, %v", v, err)
	}
	if _, err := ResolveChannel(context.Background(), hc, srv.URL+"/nope"); err == nil {
		t.Fatal("non-redirect channel should fail")
	}
}

func TestParseChecksum(t *testing.T) {
	sum := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got, err := parseChecksum([]byte(sum + "  kvz-v1-darwin-arm64.tar.gz\n")); err != nil || got != sum {
		t.Fatalf("got %q %v", got, err)
	}
	if _, err := parseChecksum([]byte("<html>not found</html>")); err == nil {
		t.Fatal("garbage accepted")
	}
}

func writeTarball(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := gzip.NewWriter(f)
	tw := tar.NewWriter(zw)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	tw.Close()
	zw.Close()
	f.Close()
}

func TestExtract(t *testing.T) {
	dir := t.TempDir()
	tb := filepath.Join(dir, "r.tar.gz")
	writeTarball(t, tb, map[string]string{
		"kvz-v1-darwin-arm64/bin/kvz":                 "BIN",
		"kvz-v1-darwin-arm64/share/kvz/kernel/Image":  "KERNEL",
		"kvz-v1-darwin-arm64/README.md":               "readme",
		"kvz-v1-darwin-arm64/../../etc/evil":          "nope",
		"kvz-v1-darwin-arm64/share/kvz/kernel/config": "CFG",
	})
	got, err := extract(tb, filepath.Join(dir, "out"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("extracted %v", got)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "out", "bin", "kvz")); string(b) != "BIN" {
		t.Fatalf("bin/kvz = %q", b)
	}
	writeTarball(t, tb, map[string]string{"x/README.md": "r"})
	if _, err := extract(tb, filepath.Join(dir, "out2")); err == nil {
		t.Fatal("tarball without bin/kvz accepted")
	}
}

// A packaged install: <prefix>/bin/kvz and <prefix>/share/kvz/kernel.
func TestInstallAndRollback(t *testing.T) {
	prefix := t.TempDir()
	exe := filepath.Join(prefix, "bin", "kvz")
	kimg := filepath.Join(prefix, "share", "kvz", "kernel", "Image")
	os.MkdirAll(filepath.Dir(exe), 0o755)
	os.MkdirAll(filepath.Dir(kimg), 0o755)
	os.WriteFile(exe, []byte("old-bin"), 0o755)
	os.WriteFile(kimg, []byte("old-kernel"), 0o644)

	m := installTarget(exe, []string{"bin/kvz", "share/kvz/kernel/Image"})
	if m["bin/kvz"] != exe || m["share/kvz/kernel/Image"] != kimg {
		t.Fatalf("targets %v", m)
	}
	staged := t.TempDir()
	os.WriteFile(filepath.Join(staged, "kvz"), []byte("new-bin"), 0o755)
	os.WriteFile(filepath.Join(staged, "Image"), []byte("new-kernel"), 0o644)
	if err := swap(filepath.Join(staged, "kvz"), exe); err != nil {
		t.Fatal(err)
	}
	if err := swap(filepath.Join(staged, "Image"), kimg); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "new-bin" {
		t.Fatalf("exe = %q", b)
	}
	if st, _ := os.Stat(exe); st.Mode().Perm() != 0o755 {
		t.Fatalf("exe mode %v", st.Mode())
	}

	data := t.TempDir()
	old := version.Release
	version.Release = "v1.35.10-9"
	defer func() { version.Release = old }()
	saveState(data, &state{From: "v1.35.10-8", To: "v1.35.10-9", Exe: exe, Files: []string{exe, kimg}})
	for i := 1; i <= maxBootAttempts; i++ {
		if p, err := BootCheck(data); err != nil || p != "" {
			t.Fatalf("attempt %d: rollback=%q err=%v", i, p, err)
		}
	}
	p, err := BootCheck(data)
	if err != nil || p != exe {
		t.Fatalf("expected rollback to %s, got %q %v", exe, p, err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old-bin" {
		t.Fatalf("exe after rollback = %q", b)
	}
	if b, _ := os.ReadFile(kimg); string(b) != "old-kernel" {
		t.Fatalf("kernel after rollback = %q", b)
	}
	s := loadState(data)
	if s.Failed != "v1.35.10-9" || s.Reason == "" {
		t.Fatalf("state %+v", s)
	}
	// the rolled-back (old) binary leaves the state alone
	version.Release = "v1.35.10-8"
	if p, err := BootCheck(data); p != "" || err != nil {
		t.Fatalf("old binary: %q %v", p, err)
	}
}

func TestSourceBuildKeepsKernel(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "bin", "kvz")
	m := installTarget(exe, []string{"bin/kvz", "share/kvz/kernel/Image"})
	if len(m) != 1 || m["bin/kvz"] != exe {
		t.Fatalf("targets %v", m)
	}
}

func node(name, release string, cp bool) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"kubernetes.io/hostname": name},
		Annotations: map[string]string{AnnRelease: release}},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	if cp {
		n.Labels[controlPlaneLabel] = "true"
	}
	return n
}

func plan(t *testing.T, spec map[string]any, ann map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "upgrade.cattle.io/v1", "kind": "Plan",
		"metadata": map[string]any{"name": "kvz", "namespace": "system-upgrade"},
		"spec":     spec,
	}}
	if ann != nil {
		u.SetAnnotations(ann)
	}
	return u
}

type rig struct {
	t   *testing.T
	cs  *fake.Clientset
	dyn *dynfake.FakeDynamicClient
	c   *Controller
}

func newRig(t *testing.T, p *unstructured.Unstructured, nodes ...*corev1.Node) *rig {
	objs := []runtime.Object{}
	for _, n := range nodes {
		objs = append(objs, n)
	}
	cs := fake.NewSimpleClientset(objs...)
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{planGVR: "PlanList"}, p)
	return &rig{t: t, cs: cs, dyn: dyn, c: NewController(cs, dyn)}
}

func (r *rig) reconcile() {
	r.t.Helper()
	if err := r.c.ReconcileAll(context.Background()); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) node(name string) *corev1.Node {
	n, err := r.cs.CoreV1().Nodes().Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		r.t.Fatal(err)
	}
	return n
}

// agentUpgrades plays the node agent: the node comes back on the new release.
func (r *rig) agentUpgrades(name string) {
	n := r.node(name)
	n.Annotations[AnnRelease] = n.Annotations[AnnApply]
	if _, err := r.cs.CoreV1().Nodes().Update(context.Background(), n, metav1.UpdateOptions{}); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) status() PlanStatus {
	u, err := r.dyn.Resource(planGVR).Namespace("system-upgrade").Get(context.Background(), "kvz", metav1.GetOptions{})
	if err != nil {
		r.t.Fatal(err)
	}
	var p Plan
	runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &p)
	return p.Status
}

func cond(s PlanStatus, typ string) Condition {
	for _, c := range s.Conditions {
		if c.Type == typ {
			return c
		}
	}
	return Condition{}
}

func TestControllerRollsControlPlaneFirst(t *testing.T) {
	const from, to = "v1.35.10-4", "v1.35.10-5"
	r := newRig(t, plan(t, map[string]any{"version": to, "cordon": true, "concurrency": int64(2)}, nil),
		node("worker-a", from, false), node("server", from, true), node("worker-b", from, false), node("current", to, false))

	r.reconcile()
	s := r.node("server")
	if s.Annotations[AnnPlan] != "system-upgrade/kvz" || !s.Spec.Unschedulable || s.Annotations[AnnCordoned] != "true" {
		t.Fatalf("server not claimed/cordoned: %v unsched=%v", s.Annotations, s.Spec.Unschedulable)
	}
	// workers wait for the control plane even though concurrency is 2
	if r.node("worker-a").Annotations[AnnPlan] != "" {
		t.Fatal("worker started while the control plane is upgrading")
	}
	// a node already on the version is marked done without cordoning
	cur := r.node("current")
	if cur.Labels[LabelPrefix+"kvz"] == "" || cur.Spec.Unschedulable {
		t.Fatalf("current node: labels=%v unsched=%v", cur.Labels, cur.Spec.Unschedulable)
	}

	r.reconcile()
	s = r.node("server")
	if s.Annotations[AnnApply] != to || s.Annotations[AnnURL] != ReleaseURL("", to) {
		t.Fatalf("server not told to apply: %v", s.Annotations)
	}
	if st := r.status(); cond(st, "Complete").Status != "False" || len(st.Applying) != 1 || st.LatestVersion != to {
		t.Fatalf("status %+v", st)
	}

	r.agentUpgrades("server")
	r.reconcile()
	s = r.node("server")
	if s.Spec.Unschedulable || s.Annotations[AnnPlan] != "" || s.Annotations[AnnApply] != "" || s.Labels[LabelPrefix+"kvz"] != r.status().LatestHash {
		t.Fatalf("server not finished: %v %v unsched=%v", s.Labels, s.Annotations, s.Spec.Unschedulable)
	}
	// both workers start together now (concurrency 2)
	if r.node("worker-a").Annotations[AnnPlan] == "" || r.node("worker-b").Annotations[AnnPlan] == "" {
		t.Fatal("workers didn't start")
	}
	r.reconcile()
	r.agentUpgrades("worker-a")
	r.agentUpgrades("worker-b")
	r.reconcile()
	st := r.status()
	if c := cond(st, "Complete"); c.Status != "True" || len(st.Applying) != 0 {
		t.Fatalf("plan not complete: %+v", st)
	}
}

func TestControllerKeepsPreviouslyCordonedNode(t *testing.T) {
	n := node("n1", "v1.35.10-1", false)
	n.Spec.Unschedulable = true // an admin cordoned it
	r := newRig(t, plan(t, map[string]any{"version": "v1.35.10-2", "cordon": true}, nil), n)
	r.reconcile()
	r.reconcile()
	r.agentUpgrades("n1")
	r.reconcile()
	if !r.node("n1").Spec.Unschedulable {
		t.Fatal("upgrade uncordoned a node it didn't cordon")
	}
}

func TestControllerRefusesDowngrade(t *testing.T) {
	r := newRig(t, plan(t, map[string]any{"version": "v1.35.10-1"}, nil), node("n1", "v1.35.10-3", false))
	r.reconcile()
	if r.node("n1").Annotations[AnnPlan] != "" {
		t.Fatal("downgrade started")
	}
	if c := cond(r.status(), "Complete"); c.Status != "False" || c.Message == "" {
		t.Fatalf("condition %+v", c)
	}
	r = newRig(t, plan(t, map[string]any{"version": "v1.35.10-1"}, map[string]string{PlanAnnAllowDowngrade: "true"}), node("n1", "v1.35.10-3", false))
	r.reconcile()
	if r.node("n1").Annotations[AnnPlan] == "" {
		t.Fatal("allowed downgrade didn't start")
	}
}

func TestControllerNodeSelectorAndURL(t *testing.T) {
	p := plan(t, map[string]any{"version": "v1.35.10-2",
		"nodeSelector": map[string]any{"matchExpressions": []any{map[string]any{"key": controlPlaneLabel, "operator": "DoesNotExist"}}}},
		map[string]string{PlanAnnReleaseURL: "http://mirror.local/{version}.tgz"})
	r := newRig(t, p, node("server", "v1.35.10-1", true), node("w", "v1.35.10-1", false))
	r.reconcile()
	r.reconcile()
	if r.node("server").Annotations[AnnPlan] != "" {
		t.Fatal("selector ignored")
	}
	if got := r.node("w").Annotations[AnnURL]; got != "http://mirror.local/v1.35.10-2.tgz" {
		t.Fatalf("url %q", got)
	}
}

func TestControllerReleasesNodesOfDeletedPlan(t *testing.T) {
	r := newRig(t, plan(t, map[string]any{"version": "v1.35.10-2", "cordon": true}, nil), node("n1", "v1.35.10-1", false))
	r.reconcile()
	r.reconcile()
	if n := r.node("n1"); n.Annotations[AnnApply] == "" || !n.Spec.Unschedulable {
		t.Fatalf("not applying: %v", n.Annotations)
	}
	if err := r.dyn.Resource(planGVR).Namespace("system-upgrade").Delete(context.Background(), "kvz", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	r.reconcile()
	n := r.node("n1")
	if n.Spec.Unschedulable || n.Annotations[AnnPlan] != "" || n.Annotations[AnnApply] != "" {
		t.Fatalf("node not released: unsched=%v %v", n.Spec.Unschedulable, n.Annotations)
	}
}
