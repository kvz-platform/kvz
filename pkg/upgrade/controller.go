package upgrade

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/klog/v2"
	"k8s.io/kubectl/pkg/drain"
)

//go:embed crd.yaml
var crdYAML []byte

var (
	planGVR = schema.GroupVersionResource{Group: Group, Version: Version, Resource: Resource}
	crdGVR  = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
)

// channelPoll is how often a channel URL is re-resolved (upstream default).
const channelPoll = 15 * time.Minute

// Controller reconciles Plans. It keeps no state that matters across
// restarts: progress lives in node annotations, so the server that hosts the
// controller can upgrade (and restart) itself mid-plan.
type Controller struct {
	cs   kubernetes.Interface
	dyn  dynamic.Interface
	http *http.Client
	now  func() time.Time

	mu       sync.Mutex
	draining map[string]bool // node -> drain running
	channels map[string]resolved
}

type resolved struct {
	version string
	at      time.Time
}

func NewController(cs kubernetes.Interface, dyn dynamic.Interface) *Controller {
	return &Controller{cs: cs, dyn: dyn, now: time.Now,
		http: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		draining: map[string]bool{}, channels: map[string]resolved{}}
}

// Run installs the Plan CRD and reconciles Plans while this server holds the
// controller lease. identity names this server in the lease.
func Run(ctx context.Context, cfg *rest.Config, identity string) {
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		klog.ErrorS(err, "upgrade controller: client")
		return
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		klog.ErrorS(err, "upgrade controller: dynamic client")
		return
	}
	for {
		if err = EnsureCRD(ctx, dyn); err == nil {
			break
		}
		klog.ErrorS(err, "upgrade controller: Plan CRD (will retry)")
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
	c := NewController(cs, dyn)
	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Name: "kvz-upgrade-controller", Namespace: "kube-system"},
		Client:     cs.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: identity},
	}
	for ctx.Err() == nil {
		leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
			Lock: lock, ReleaseOnCancel: true,
			LeaseDuration: 15 * time.Second, RenewDeadline: 10 * time.Second, RetryPeriod: 2 * time.Second,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: func(ctx context.Context) {
					klog.InfoS("upgrade controller: leading", "identity", identity)
					c.loop(ctx)
				},
				OnStoppedLeading: func() {},
			},
		})
	}
}

func (c *Controller) loop(ctx context.Context) {
	for {
		if err := c.ReconcileAll(ctx); err != nil && ctx.Err() == nil {
			klog.ErrorS(err, "upgrade controller: reconcile")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// EnsureCRD server-side applies the Plan CRD.
func EnsureCRD(ctx context.Context, dyn dynamic.Interface) error {
	var obj unstructured.Unstructured
	if err := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(string(crdYAML)), 4096).Decode(&obj.Object); err != nil {
		return err
	}
	body, err := obj.MarshalJSON()
	if err != nil {
		return err
	}
	force := true
	_, err = dyn.Resource(crdGVR).Patch(ctx, obj.GetName(), types.ApplyPatchType, body, metav1.PatchOptions{FieldManager: "kvz-upgrade", Force: &force})
	return err
}

// MarkControlPlane labels a server's own node the way k3s does, so Plans
// written for k3s (node-role.kubernetes.io/control-plane In [true]) select
// it. Nodes can't set node-role labels on themselves (NodeRestriction), so the
// server does it with admin credentials once the node has registered.
func MarkControlPlane(ctx context.Context, cs kubernetes.Interface, nodeName string) {
	patch := []byte(fmt.Sprintf(`{"metadata":{"labels":{%q:"true",%q:"true"}}}`, controlPlaneLabel, "node-role.kubernetes.io/master"))
	for ctx.Err() == nil {
		_, err := cs.CoreV1().Nodes().Patch(ctx, nodeName, types.MergePatchType, patch, metav1.PatchOptions{})
		if err == nil {
			return
		}
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
		}
	}
}

// ReconcileAll reconciles every Plan once.
func (c *Controller) ReconcileAll(ctx context.Context) error {
	list, err := c.dyn.Resource(planGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	nodes, err := c.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	var errs []error
	plans := map[string]bool{}
	for i := range list.Items {
		plans[list.Items[i].GetNamespace()+"/"+list.Items[i].GetName()] = true
	}
	for i := range nodes.Items {
		if err := c.releaseOrphan(ctx, &nodes.Items[i], plans); err != nil {
			errs = append(errs, err)
		}
	}
	for i := range list.Items {
		var p Plan
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(list.Items[i].Object, &p); err != nil {
			errs = append(errs, fmt.Errorf("plan %s/%s: %w", list.Items[i].GetNamespace(), list.Items[i].GetName(), err))
			continue
		}
		if err := c.reconcile(ctx, &p, nodes.Items); err != nil {
			errs = append(errs, fmt.Errorf("plan %s/%s: %w", p.Namespace, p.Name, err))
		}
	}
	return errors.Join(errs...)
}

// releaseOrphan frees a node claimed by a plan that was deleted mid-upgrade,
// uncordoning it if the plan had cordoned it.
func (c *Controller) releaseOrphan(ctx context.Context, n *corev1.Node, plans map[string]bool) error {
	owner := n.Annotations[AnnPlan]
	if owner == "" || plans[owner] {
		return nil
	}
	var spec map[string]any
	if n.Annotations[AnnCordoned] == "true" {
		spec = map[string]any{"unschedulable": false}
	}
	klog.InfoS("upgrade: releasing node of deleted plan", "node", n.Name, "plan", owner)
	n.Annotations[AnnPlan] = ""
	return c.patchNode(ctx, n.Name, map[string]any{AnnPlan: nil, AnnApply: nil, AnnURL: nil, AnnCordoned: nil, AnnStatus: nil}, nil, spec)
}

func planKey(p *Plan) string { return p.Namespace + "/" + p.Name }

// planHash identifies what a plan installs; nodes carrying it in their plan
// label are done.
func planHash(version, url string) string {
	s := sha256.Sum256([]byte(version + "\x00" + url))
	return hex.EncodeToString(s[:])[:56]
}

func (c *Controller) reconcile(ctx context.Context, p *Plan, nodes []corev1.Node) error {
	st := p.Status.DeepCopy()
	ver, err := c.resolve(ctx, p)
	if err != nil {
		setCond(st, "LatestResolved", "False", "Error", err.Error(), c.now())
		setCond(st, "Complete", "False", "Error", err.Error(), c.now())
		return c.updateStatus(ctx, p, st)
	}
	how := "Version"
	if p.Spec.Version == "" {
		how = "Channel"
	}
	setCond(st, "LatestResolved", "True", how, "", c.now())
	url := ReleaseURL(p.Annotations[PlanAnnReleaseURL], ver)
	hash := planHash(ver, url)
	st.LatestVersion, st.LatestHash = ver, hash

	sel := labels.Everything()
	if p.Spec.NodeSelector != nil {
		if sel, err = metav1.LabelSelectorAsSelector(p.Spec.NodeSelector); err != nil {
			setCond(st, "Complete", "False", "Error", "nodeSelector: "+err.Error(), c.now())
			return c.updateStatus(ctx, p, st)
		}
	}
	key := planKey(p)
	var applying, pending []*corev1.Node
	for i := range nodes {
		n := &nodes[i]
		if !sel.Matches(labels.Set(n.Labels)) {
			continue
		}
		switch owner := n.Annotations[AnnPlan]; {
		case owner == key:
			applying = append(applying, n)
		case owner != "":
			// another plan is upgrading it; pick it up afterwards
			pending = append(pending, n)
		case n.Labels[LabelPrefix+p.Name] != hash:
			pending = append(pending, n)
		}
	}

	var msgs []string
	var still []*corev1.Node
	for _, n := range applying {
		done, msg, err := c.progress(ctx, p, n, ver, url, hash)
		if err != nil {
			msg = err.Error()
		}
		if msg != "" {
			msgs = append(msgs, n.Name+": "+msg)
		}
		if !done {
			still = append(still, n)
		}
	}
	applying = still

	// Control-plane nodes go first, as k3s upgrades servers before agents:
	// a kubelet must never be newer than the API server.
	sort.SliceStable(pending, func(i, j int) bool {
		ci, cj := isControlPlane(pending[i]), isControlPlane(pending[j])
		if ci != cj {
			return ci
		}
		return pending[i].Name < pending[j].Name
	})
	conc := int(p.Spec.Concurrency)
	if conc <= 0 {
		conc = 1
	}
	cpBusy := false
	for _, n := range applying {
		cpBusy = cpBusy || isControlPlane(n)
	}
	var waiting []string
	for _, n := range pending {
		if n.Annotations[AnnPlan] != "" {
			waiting = append(waiting, n.Name)
			continue
		}
		if n.Annotations[AnnRelease] == ver {
			// already on the plan's version: just record it
			if err := c.patchNode(ctx, n.Name, nil, map[string]any{LabelPrefix + p.Name: hash}, nil); err != nil {
				return err
			}
			continue
		}
		if isDowngrade(n.Annotations[AnnRelease], ver) && p.Annotations[PlanAnnAllowDowngrade] != "true" {
			msgs = append(msgs, fmt.Sprintf("%s: refusing downgrade %s -> %s (etcd data can't move back; set %s=true to force)",
				n.Name, n.Annotations[AnnRelease], ver, PlanAnnAllowDowngrade))
			waiting = append(waiting, n.Name)
			continue
		}
		if len(applying) >= conc || (cpBusy && !isControlPlane(n)) {
			waiting = append(waiting, n.Name)
			continue
		}
		if err := c.start(ctx, p, n); err != nil {
			return err
		}
		klog.InfoS("upgrade: starting node", "plan", key, "node", n.Name, "from", n.Annotations[AnnRelease], "to", ver)
		applying = append(applying, n)
		cpBusy = cpBusy || isControlPlane(n)
	}

	st.Applying = nil
	for _, n := range applying {
		st.Applying = append(st.Applying, n.Name)
	}
	sort.Strings(st.Applying)
	if len(applying) == 0 && len(waiting) == 0 {
		setCond(st, "Complete", "True", "Complete", "", c.now())
	} else {
		msg := strings.Join(msgs, "; ")
		if msg == "" && len(waiting) > 0 {
			msg = fmt.Sprintf("%d node(s) waiting", len(waiting))
		}
		setCond(st, "Complete", "False", "Applying", msg, c.now())
	}
	return c.updateStatus(ctx, p, st)
}

// start claims a node for the plan and cordons it if asked to.
func (c *Controller) start(ctx context.Context, p *Plan, n *corev1.Node) error {
	ann := map[string]any{AnnPlan: planKey(p), AnnStatus: "Pending"}
	var spec map[string]any
	if (p.Spec.Cordon || p.Spec.Drain != nil) && !n.Spec.Unschedulable {
		ann[AnnCordoned] = "true"
		spec = map[string]any{"unschedulable": true}
	}
	return c.patchNode(ctx, n.Name, ann, nil, spec)
}

// progress advances one node of a plan and reports whether it is done.
func (c *Controller) progress(ctx context.Context, p *Plan, n *corev1.Node, ver, url, hash string) (bool, string, error) {
	status := n.Annotations[AnnStatus]
	if n.Annotations[AnnRelease] == ver && nodeReady(n) {
		var spec map[string]any
		if n.Annotations[AnnCordoned] == "true" {
			spec = map[string]any{"unschedulable": false}
		}
		err := c.patchNode(ctx, n.Name,
			map[string]any{AnnPlan: nil, AnnApply: nil, AnnURL: nil, AnnCordoned: nil, AnnStatus: nil},
			map[string]any{LabelPrefix + p.Name: hash}, spec)
		if err == nil {
			klog.InfoS("upgrade: node done", "plan", planKey(p), "node", n.Name, "version", ver)
		}
		return err == nil, "", err
	}
	if n.Annotations[AnnApply] == ver && n.Annotations[AnnURL] == url {
		if status == "" || status == "Pending" || status == "Drained" {
			status = "waiting for the node's kvz agent"
		}
		return false, status, nil
	}
	if p.Spec.Drain == nil {
		return false, "Applying", c.patchNode(ctx, n.Name, map[string]any{AnnApply: ver, AnnURL: url}, nil, nil)
	}
	c.mu.Lock()
	busy := c.draining[n.Name]
	if !busy {
		c.draining[n.Name] = true
	}
	c.mu.Unlock()
	if !busy {
		go c.drain(ctx, p, n.Name, ver, url)
	}
	return false, "Draining", nil
}

func (c *Controller) drain(ctx context.Context, p *Plan, node, ver, url string) {
	defer func() {
		c.mu.Lock()
		delete(c.draining, node)
		c.mu.Unlock()
	}()
	d := p.Spec.Drain
	h := &drain.Helper{Ctx: ctx, Client: c.cs, Force: d.Force, GracePeriodSeconds: -1,
		IgnoreAllDaemonSets: d.IgnoreDaemonSets == nil || *d.IgnoreDaemonSets,
		DeleteEmptyDirData:  (d.DeleteEmptydirData != nil && *d.DeleteEmptydirData) || (d.DeleteLocalData != nil && *d.DeleteLocalData),
		DisableEviction:     d.DisableEviction,
		Out:                 klogWriter{node}, ErrOut: klogWriter{node}}
	if d.GracePeriod != nil {
		h.GracePeriodSeconds = int(*d.GracePeriod)
	}
	if d.Timeout != nil {
		h.Timeout = d.Timeout.Duration
	}
	if d.SkipWaitForDelete > 0 {
		h.SkipWaitForDeleteTimeoutSeconds = d.SkipWaitForDelete
	}
	if d.PodSelector != nil {
		if s, err := metav1.LabelSelectorAsSelector(d.PodSelector); err == nil {
			h.PodSelector = s.String()
		}
	}
	klog.InfoS("upgrade: draining", "node", node)
	if err := drain.RunNodeDrain(h, node); err != nil {
		klog.ErrorS(err, "upgrade: drain failed (will retry)", "node", node)
		_ = c.patchNode(ctx, node, map[string]any{AnnStatus: "DrainFailed: " + oneLine(err.Error())}, nil, nil)
		return
	}
	_ = c.patchNode(ctx, node, map[string]any{AnnApply: ver, AnnURL: url, AnnStatus: "Drained"}, nil, nil)
}

// resolve returns the release a plan wants: spec.version, or the last path
// element of where spec.channel redirects to (upstream semantics; works with
// https://github.com/kvz-platform/kvz/releases/latest).
func (c *Controller) resolve(ctx context.Context, p *Plan) (string, error) {
	if p.Spec.Version != "" {
		return p.Spec.Version, nil
	}
	if p.Spec.Channel == "" {
		return "", fmt.Errorf("plan needs spec.version or spec.channel")
	}
	c.mu.Lock()
	r, ok := c.channels[p.Spec.Channel]
	c.mu.Unlock()
	if ok && c.now().Sub(r.at) < channelPoll {
		return r.version, nil
	}
	v, err := ResolveChannel(ctx, c.http, p.Spec.Channel)
	if err != nil {
		if ok {
			klog.ErrorS(err, "upgrade: channel refresh failed, keeping last version", "channel", p.Spec.Channel)
			return r.version, nil
		}
		return "", err
	}
	c.mu.Lock()
	c.channels[p.Spec.Channel] = resolved{version: v, at: c.now()}
	c.mu.Unlock()
	return v, nil
}

// ResolveChannel follows one redirect and returns the target's last path
// element. hc must not follow redirects itself.
func ResolveChannel(ctx context.Context, hc *http.Client, channel string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, channel, nil)
	if err != nil {
		return "", err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("channel %s: %w", channel, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 300 || resp.StatusCode > 399 {
		return "", fmt.Errorf("channel %s: expected a redirect, got %s", channel, resp.Status)
	}
	loc, err := resp.Location()
	if err != nil {
		return "", fmt.Errorf("channel %s: %w", channel, err)
	}
	v := path.Base(loc.Path)
	if v == "" || v == "/" || v == "." {
		return "", fmt.Errorf("channel %s: redirect %s has no version", channel, loc)
	}
	return v, nil
}

func (c *Controller) updateStatus(ctx context.Context, p *Plan, st *PlanStatus) error {
	if reflect.DeepEqual(&p.Status, st) {
		return nil
	}
	var m map[string]any
	if b, err := json.Marshal(st); err != nil {
		return err
	} else if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	if len(st.Applying) == 0 {
		m["applying"] = nil // a merge patch only removes fields set to null
	}
	b, err := json.Marshal(map[string]any{"status": m})
	if err != nil {
		return err
	}
	_, err = c.dyn.Resource(planGVR).Namespace(p.Namespace).Patch(ctx, p.Name, types.MergePatchType, b, metav1.PatchOptions{}, "status")
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func (c *Controller) patchNode(ctx context.Context, name string, ann, lbl, spec map[string]any) error {
	meta := map[string]any{}
	if ann != nil {
		meta["annotations"] = ann
	}
	if lbl != nil {
		meta["labels"] = lbl
	}
	body := map[string]any{"metadata": meta}
	if spec != nil {
		body["spec"] = spec
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	_, err = c.cs.CoreV1().Nodes().Patch(ctx, name, types.MergePatchType, b, metav1.PatchOptions{})
	return err
}

// setCond sets a condition, bumping lastUpdateTime only when it changes.
func setCond(st *PlanStatus, typ, status, reason, msg string, now time.Time) {
	for i := range st.Conditions {
		c := &st.Conditions[i]
		if c.Type == typ {
			if c.Status != status || c.Reason != reason || c.Message != msg {
				*c = Condition{Type: typ, Status: status, Reason: reason, Message: msg, LastUpdateTime: now.UTC().Format(time.RFC3339)}
			}
			return
		}
	}
	st.Conditions = append(st.Conditions, Condition{Type: typ, Status: status, Reason: reason, Message: msg, LastUpdateTime: now.UTC().Format(time.RFC3339)})
}

func isControlPlane(n *corev1.Node) bool { return n.Labels[controlPlaneLabel] == "true" }

func nodeReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

type klogWriter struct{ node string }

func (w klogWriter) Write(b []byte) (int, error) {
	if s := strings.TrimSpace(string(b)); s != "" {
		klog.InfoS("upgrade: drain", "node", w.node, "msg", s)
	}
	return len(b), nil
}
