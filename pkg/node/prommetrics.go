package node

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/component-base/metrics/legacyregistry"
)

// Kubelet metrics endpoints that Prometheus setups scrape through the
// apiserver's node proxy: /metrics (process/Go metrics plus kubelet gauges)
// and /metrics/cadvisor (per-container CPU and memory).

func (p *Provider) metricsHandler() http.Handler {
	base := legacyregistry.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pods, _ := p.GetPods(r.Context())
		running, containers := 0, 0
		for _, pod := range pods {
			if pod.Status.Phase == corev1.PodRunning {
				running++
			}
			for _, cs := range pod.Status.ContainerStatuses {
				if cs.State.Running != nil {
					containers++
				}
			}
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		fmt.Fprintf(w, "# HELP kubelet_running_pods Number of pods that have a running pod sandbox\n# TYPE kubelet_running_pods gauge\nkubelet_running_pods %d\n", running)
		fmt.Fprintf(w, "# HELP kubelet_running_containers Number of containers currently running\n# TYPE kubelet_running_containers gauge\nkubelet_running_containers{container_state=\"running\"} %d\n", containers)
		fmt.Fprintf(w, "# HELP kubernetes_build_info A metric with a constant '1' value labeled by version\n# TYPE kubernetes_build_info gauge\nkubernetes_build_info{git_version=%q} 1\n", Version)
		r2 := r.Clone(r.Context())
		r2.Header.Del("Accept-Encoding") // the registry would gzip its part of a shared body
		base.ServeHTTP(w, r2)
	})
}

// resourceHandler serves /metrics/resource, the endpoint upstream
// metrics-server scrapes (values carry millisecond timestamps).
func (p *Provider) resourceHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sum, err := p.GetStatsSummary(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var b strings.Builder
		n := sum.Node
		if n.CPU != nil && n.CPU.UsageCoreNanoSeconds != nil {
			fmt.Fprintf(&b, "# HELP node_cpu_usage_seconds_total Cumulative cpu time consumed by the node in core-seconds\n# TYPE node_cpu_usage_seconds_total counter\nnode_cpu_usage_seconds_total %g %d\n",
				float64(*n.CPU.UsageCoreNanoSeconds)/1e9, n.CPU.Time.UnixMilli())
		}
		if n.Memory != nil && n.Memory.WorkingSetBytes != nil {
			fmt.Fprintf(&b, "# HELP node_memory_working_set_bytes Current working set of the node in bytes\n# TYPE node_memory_working_set_bytes gauge\nnode_memory_working_set_bytes %d %d\n",
				*n.Memory.WorkingSetBytes, n.Memory.Time.UnixMilli())
		}
		var ccpu, cmem, pcpu, pmem, cstart []string
		for _, ps := range sum.Pods {
			pl := fmt.Sprintf(`namespace="%s",pod="%s"`, promLabel(ps.PodRef.Namespace), promLabel(ps.PodRef.Name))
			if ps.CPU != nil && ps.CPU.UsageCoreNanoSeconds != nil {
				pcpu = append(pcpu, fmt.Sprintf("pod_cpu_usage_seconds_total{%s} %g %d", pl, float64(*ps.CPU.UsageCoreNanoSeconds)/1e9, ps.CPU.Time.UnixMilli()))
			}
			if ps.Memory != nil && ps.Memory.WorkingSetBytes != nil {
				pmem = append(pmem, fmt.Sprintf("pod_memory_working_set_bytes{%s} %d %d", pl, *ps.Memory.WorkingSetBytes, ps.Memory.Time.UnixMilli()))
			}
			for _, c := range ps.Containers {
				cl := fmt.Sprintf(`container="%s",%s`, promLabel(c.Name), pl)
				cstart = append(cstart, fmt.Sprintf("container_start_time_seconds{%s} %d", cl, c.StartTime.Unix()))
				if c.CPU != nil && c.CPU.UsageCoreNanoSeconds != nil {
					ccpu = append(ccpu, fmt.Sprintf("container_cpu_usage_seconds_total{%s} %g %d", cl, float64(*c.CPU.UsageCoreNanoSeconds)/1e9, c.CPU.Time.UnixMilli()))
				}
				if c.Memory != nil && c.Memory.WorkingSetBytes != nil {
					cmem = append(cmem, fmt.Sprintf("container_memory_working_set_bytes{%s} %d %d", cl, *c.Memory.WorkingSetBytes, c.Memory.Time.UnixMilli()))
				}
			}
		}
		for _, f := range []struct {
			name, typ, help string
			rows            []string
		}{
			{"container_cpu_usage_seconds_total", "counter", "Cumulative cpu time consumed by the container in core-seconds", ccpu},
			{"container_memory_working_set_bytes", "gauge", "Current working set of the container in bytes", cmem},
			{"container_start_time_seconds", "gauge", "Start time of the container since unix epoch in seconds", cstart},
			{"pod_cpu_usage_seconds_total", "counter", "Cumulative cpu time consumed by the pod in core-seconds", pcpu},
			{"pod_memory_working_set_bytes", "gauge", "Current working set of the pod in bytes", pmem},
		} {
			if len(f.rows) == 0 {
				continue
			}
			sort.Strings(f.rows)
			fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n%s\n", f.name, f.help, f.name, f.typ, strings.Join(f.rows, "\n"))
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(b.String()))
	})
}

func promLabel(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

func (p *Provider) cadvisorHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sum, err := p.GetStatsSummary(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		type row struct {
			labels string
			cpu    float64
			mem    uint64
			hasCPU bool
			hasMem bool
		}
		var rows []row
		for _, ps := range sum.Pods {
			for _, c := range ps.Containers {
				rw := row{labels: fmt.Sprintf(`container="%s",namespace="%s",pod="%s"`, promLabel(c.Name), promLabel(ps.PodRef.Namespace), promLabel(ps.PodRef.Name))}
				if c.CPU != nil && c.CPU.UsageCoreNanoSeconds != nil {
					rw.cpu, rw.hasCPU = float64(*c.CPU.UsageCoreNanoSeconds)/1e9, true
				}
				if c.Memory != nil && c.Memory.WorkingSetBytes != nil {
					rw.mem, rw.hasMem = *c.Memory.WorkingSetBytes, true
				}
				rows = append(rows, rw)
			}
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].labels < rows[j].labels })
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		fmt.Fprint(w, "# HELP container_cpu_usage_seconds_total Cumulative cpu time consumed in seconds.\n# TYPE container_cpu_usage_seconds_total counter\n")
		for _, rw := range rows {
			if rw.hasCPU {
				fmt.Fprintf(w, "container_cpu_usage_seconds_total{%s} %g\n", rw.labels, rw.cpu)
			}
		}
		fmt.Fprint(w, "# HELP container_memory_working_set_bytes Current working set in bytes.\n# TYPE container_memory_working_set_bytes gauge\n")
		for _, rw := range rows {
			if rw.hasMem {
				fmt.Fprintf(w, "container_memory_working_set_bytes{%s} %d\n", rw.labels, rw.mem)
			}
		}
	})
}
