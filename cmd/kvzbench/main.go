// kvzbench: cluster-agnostic Kubernetes micro-benchmarks. Point it at any
// kubeconfig (kvz, k3s, kind, ...) and it emits one JSON object per suite.
//
//	kvzbench api  --n 2000 --c 32
//	kvzbench pods --n 50 --c 10 --image docker.io/library/nginx:alpine [--runtime-class linux]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

type Stats struct {
	N    int     `json:"n"`
	Min  float64 `json:"min_ms"`
	P50  float64 `json:"p50_ms"`
	P90  float64 `json:"p90_ms"`
	P99  float64 `json:"p99_ms"`
	Max  float64 `json:"max_ms"`
	Mean float64 `json:"mean_ms"`
}

func stats(d []time.Duration) Stats {
	if len(d) == 0 {
		return Stats{}
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	ms := func(x time.Duration) float64 { return math.Round(float64(x.Microseconds())/10) / 100 }
	q := func(p float64) float64 {
		return ms(d[int(math.Min(float64(len(d)-1), math.Ceil(p*float64(len(d)))-1))])
	}
	var sum time.Duration
	for _, x := range d {
		sum += x
	}
	return Stats{N: len(d), Min: ms(d[0]), P50: q(.50), P90: q(.90), P99: q(.99), Max: ms(d[len(d)-1]), Mean: ms(sum / time.Duration(len(d)))}
}

func emit(v map[string]any) {
	v["ts"] = time.Now().UTC().Format(time.RFC3339)
	v["label"] = *label
	b, _ := json.Marshal(v)
	fmt.Println(string(b))
}

var (
	kubeconfig = flag.String("kubeconfig", os.Getenv("KUBECONFIG"), "kubeconfig")
	label      = flag.String("label", "", "free-form label added to output (platform/host)")
	n          = flag.Int("n", 1000, "operations / pods")
	conc       = flag.Int("c", 16, "concurrency")
	image      = flag.String("image", "docker.io/library/nginx:alpine", "pod image")
	rclass     = flag.String("runtime-class", "", "runtimeClassName for pods")
	command    = flag.String("command", "", "pod command (sh -c) — needed for darwin runtime")
	node       = flag.String("node", "", "pin pods to node")
	ns         = flag.String("namespace", "kvzbench", "namespace")
	timeout    = flag.Duration("timeout", 10*time.Minute, "suite timeout")
	cpuReq     = flag.String("cpu", "", "cpu request/limit per pod")
	memReq     = flag.String("memory", "", "memory request/limit per pod")
	hold       = flag.Duration("hold", 0, "keep pods running this long after all are ready (for footprint sampling)")
	readyFile  = flag.String("ready-file", "", "touch this file once all pods are ready")
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: kvzbench api|pods [flags]")
		os.Exit(2)
	}
	suite := os.Args[1]
	_ = flag.CommandLine.Parse(os.Args[2:])
	cfg, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		panic(err)
	}
	cfg.QPS, cfg.Burst = 5000, 5000
	cs := kubernetes.NewForConfigOrDie(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	_, _ = cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: *ns}}, metav1.CreateOptions{})
	// wait for default SA (pods are rejected until it exists)
	for i := 0; i < 100; i++ {
		if _, err := cs.CoreV1().ServiceAccounts(*ns).Get(ctx, "default", metav1.GetOptions{}); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	switch suite {
	case "api":
		apiSuite(ctx, cs)
	case "pods":
		podSuite(ctx, cs)
	default:
		fmt.Fprintln(os.Stderr, "unknown suite", suite)
		os.Exit(2)
	}
}

func parallel(nops, c int, f func(i int) time.Duration) ([]time.Duration, time.Duration) {
	out := make([]time.Duration, nops)
	var next int64 = -1
	var wg sync.WaitGroup
	t0 := time.Now()
	for w := 0; w < c; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(atomic.AddInt64(&next, 1))
				if i >= nops {
					return
				}
				out[i] = f(i)
			}
		}()
	}
	wg.Wait()
	return out, time.Since(t0)
}

func apiSuite(ctx context.Context, cs *kubernetes.Clientset) {
	cms := cs.CoreV1().ConfigMaps(*ns)
	run := fmt.Sprintf("r%d", time.Now().UnixNano()%1e6)
	var errs int64
	data := map[string]string{"payload": string(make([]byte, 512))}
	lat, wall := parallel(*n, *conc, func(i int) time.Duration {
		t := time.Now()
		_, err := cms.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-%d", run, i), Labels: map[string]string{"run": run}}, Data: data}, metav1.CreateOptions{})
		if err != nil {
			atomic.AddInt64(&errs, 1)
		}
		return time.Since(t)
	})
	emit(map[string]any{"suite": "api", "op": "create", "c": *conc, "stats": stats(lat), "ops_per_s": math.Round(float64(*n) / wall.Seconds()), "errors": errs})

	lat, wall = parallel(*n, *conc, func(i int) time.Duration {
		t := time.Now()
		if _, err := cms.Get(ctx, fmt.Sprintf("%s-%d", run, i), metav1.GetOptions{}); err != nil {
			atomic.AddInt64(&errs, 1)
		}
		return time.Since(t)
	})
	emit(map[string]any{"suite": "api", "op": "get", "c": *conc, "stats": stats(lat), "ops_per_s": math.Round(float64(*n) / wall.Seconds())})

	lat, wall = parallel(*n, *conc, func(i int) time.Duration {
		t := time.Now()
		cm, err := cms.Get(ctx, fmt.Sprintf("%s-%d", run, i), metav1.GetOptions{})
		if err == nil {
			cm.Data["v"] = "2"
			_, err = cms.Update(ctx, cm, metav1.UpdateOptions{})
		}
		if err != nil {
			atomic.AddInt64(&errs, 1)
		}
		return time.Since(t)
	})
	emit(map[string]any{"suite": "api", "op": "get+update", "c": *conc, "stats": stats(lat), "ops_per_s": math.Round(float64(*n) / wall.Seconds())})

	var ll []time.Duration
	for i := 0; i < 20; i++ {
		t := time.Now()
		l, err := cms.List(ctx, metav1.ListOptions{LabelSelector: "run=" + run})
		if err != nil || len(l.Items) != *n {
			atomic.AddInt64(&errs, 1)
		}
		ll = append(ll, time.Since(t))
	}
	emit(map[string]any{"suite": "api", "op": fmt.Sprintf("list-%d", *n), "stats": stats(ll)})

	// watch fan-out latency: time from create to event delivery
	w, err := cms.Watch(ctx, metav1.ListOptions{LabelSelector: "run=" + run + "-w"})
	if err == nil {
		var wl []time.Duration
		for i := 0; i < 200; i++ {
			t := time.Now()
			_, _ = cms.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-w-%d", run, i), Labels: map[string]string{"run": run + "-w"}}}, metav1.CreateOptions{})
			for ev := range w.ResultChan() {
				if ev.Type == watch.Added {
					break
				}
			}
			wl = append(wl, time.Since(t))
		}
		w.Stop()
		emit(map[string]any{"suite": "api", "op": "create->watch-event", "stats": stats(wl)})
		_ = cms.DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{LabelSelector: "run=" + run + "-w"})
	}

	lat, wall = parallel(*n, *conc, func(i int) time.Duration {
		t := time.Now()
		_ = cms.Delete(ctx, fmt.Sprintf("%s-%d", run, i), metav1.DeleteOptions{})
		return time.Since(t)
	})
	emit(map[string]any{"suite": "api", "op": "delete", "c": *conc, "stats": stats(lat), "ops_per_s": math.Round(float64(*n) / wall.Seconds()), "errors": errs})
}

func podSuite(ctx context.Context, cs *kubernetes.Clientset) {
	pods := cs.CoreV1().Pods(*ns)
	run := fmt.Sprintf("p%d", time.Now().UnixNano()%1e6)
	sel := "run=" + run
	var mu sync.Mutex
	created := map[string]time.Time{}
	ready := map[string]time.Time{}
	scheduled := map[string]time.Time{}
	running := map[string]time.Time{}
	gone := map[string]time.Time{}
	allReady := make(chan struct{})
	allGone := make(chan struct{})
	deleting := atomic.Bool{}
	observe := func(p *corev1.Pod) {
		now := time.Now()
		mu.Lock()
		defer mu.Unlock()
		if p.Spec.NodeName != "" {
			if _, ok := scheduled[p.Name]; !ok {
				scheduled[p.Name] = now
			}
		}
		if p.Status.Phase == corev1.PodRunning {
			if _, ok := running[p.Name]; !ok {
				running[p.Name] = now
			}
		}
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				if _, ok := ready[p.Name]; !ok && !deleting.Load() {
					ready[p.Name] = now
					if len(ready) == *n {
						close(allReady)
					}
				}
			}
		}
	}
	// Informer (list+watch with automatic re-watch) so a dropped watch
	// cannot stall the measurement.
	lw := cache.NewFilteredListWatchFromClient(cs.CoreV1().RESTClient(), "pods", *ns, func(o *metav1.ListOptions) { o.LabelSelector = sel })
	inf := cache.NewSharedIndexInformer(lw, &corev1.Pod{}, 0, cache.Indexers{})
	_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(o any) { observe(o.(*corev1.Pod)) },
		UpdateFunc: func(_, o any) { observe(o.(*corev1.Pod)) },
		DeleteFunc: func(o any) {
			p, ok := o.(*corev1.Pod)
			if !ok {
				if d, ok2 := o.(cache.DeletedFinalStateUnknown); ok2 {
					p, _ = d.Obj.(*corev1.Pod)
				}
			}
			if p == nil {
				return
			}
			mu.Lock()
			if _, ok := gone[p.Name]; !ok {
				gone[p.Name] = time.Now()
				if len(gone) == *n {
					close(allGone)
				}
			}
			mu.Unlock()
		},
	})
	stopInf := make(chan struct{})
	defer close(stopInf)
	go inf.Run(stopInf)
	cache.WaitForCacheSync(ctx.Done(), inf.HasSynced)

	res := corev1.ResourceRequirements{Limits: corev1.ResourceList{}, Requests: corev1.ResourceList{}}
	if *cpuReq != "" {
		res.Limits[corev1.ResourceCPU] = resource.MustParse(*cpuReq)
		res.Requests[corev1.ResourceCPU] = resource.MustParse(*cpuReq)
	}
	if *memReq != "" {
		res.Limits[corev1.ResourceMemory] = resource.MustParse(*memReq)
		res.Requests[corev1.ResourceMemory] = resource.MustParse(*memReq)
	}
	mk := func(i int) *corev1.Pod {
		grace := int64(1)
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-%d", run, i), Labels: map[string]string{"run": run}},
			Spec: corev1.PodSpec{
				TerminationGracePeriodSeconds: &grace,
				Containers:                    []corev1.Container{{Name: "c", Image: *image, ImagePullPolicy: corev1.PullIfNotPresent, Resources: res}},
				Tolerations:                   []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			},
		}
		if *command != "" {
			p.Spec.Containers[0].Command = []string{"/bin/sh", "-c", *command}
		}
		if *rclass != "" {
			p.Spec.RuntimeClassName = rclass
		}
		if *node != "" {
			p.Spec.NodeName = *node
		}
		return p
	}
	t0 := time.Now()
	var cerrs int64
	parallel(*n, *conc, func(i int) time.Duration {
		p := mk(i)
		mu.Lock()
		created[p.Name] = time.Now()
		mu.Unlock()
		if _, err := pods.Create(ctx, p, metav1.CreateOptions{}); err != nil {
			atomic.AddInt64(&cerrs, 1)
			fmt.Fprintln(os.Stderr, "create:", err)
		}
		return 0
	})
	createWall := time.Since(t0)
	timedOut := false
	select {
	case <-allReady:
	case <-ctx.Done():
		timedOut = true
	}
	readyWall := time.Since(t0)
	mu.Lock()
	var sched, run_, rdy []time.Duration
	nodes := map[string]int{}
	for name, c := range created {
		if t, ok := scheduled[name]; ok {
			sched = append(sched, t.Sub(c))
		}
		if t, ok := running[name]; ok {
			run_ = append(run_, t.Sub(c))
		}
		if t, ok := ready[name]; ok {
			rdy = append(rdy, t.Sub(c))
		}
	}
	mu.Unlock()
	if l, err := pods.List(ctx, metav1.ListOptions{LabelSelector: sel}); err == nil {
		for _, p := range l.Items {
			nodes[p.Spec.NodeName]++
		}
	}
	emit(map[string]any{"suite": "pods", "phase": "startup", "n": *n, "c": *conc, "image": *image, "runtime_class": *rclass,
		"create_errors": cerrs, "timed_out": timedOut, "create_wall_s": createWall.Seconds(), "all_ready_wall_s": math.Round(readyWall.Seconds()*1000) / 1000,
		"pods_per_s": math.Round(float64(len(rdy))/readyWall.Seconds()*100) / 100,
		"scheduled":  stats(sched), "running": stats(run_), "ready": stats(rdy), "nodes": nodes})

	if *readyFile != "" {
		_ = os.WriteFile(*readyFile, []byte("ready"), 0o644)
	}
	if *hold > 0 {
		time.Sleep(*hold)
	}
	deleting.Store(true)
	td := time.Now()
	// default grace (1s from the spec) so the runtime actually tears down
	_ = pods.DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{LabelSelector: sel})
	select {
	case <-allGone:
	case <-time.After(5 * time.Minute):
	}
	mu.Lock()
	var gl []time.Duration
	for _, t := range gone {
		gl = append(gl, t.Sub(td))
	}
	mu.Unlock()
	emit(map[string]any{"suite": "pods", "phase": "teardown", "n": *n, "all_gone_wall_s": math.Round(time.Since(td).Seconds()*1000) / 1000, "gone": stats(gl)})
	_ = apierrors.IsNotFound
}
