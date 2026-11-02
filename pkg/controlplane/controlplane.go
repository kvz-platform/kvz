// Package controlplane runs etcd, kube-apiserver, kube-controller-manager and
// kube-scheduler in-process, natively on darwin (no Linux VM).
package controlplane

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/pflag"
	"go.etcd.io/etcd/server/v3/embed"
	logsapi "k8s.io/component-base/logs/api/v1"
	"k8s.io/klog/v2"
	apiapp "k8s.io/kubernetes/cmd/kube-apiserver/app"
	cmapp "k8s.io/kubernetes/cmd/kube-controller-manager/app"
	schedapp "k8s.io/kubernetes/cmd/kube-scheduler/app"
	schedopts "k8s.io/kubernetes/cmd/kube-scheduler/app/options"

	"github.com/GrowlyX/kvz/pkg/pki"
)

func init() {
	// Three k8s components share one process; let them all "apply" logging.
	logsapi.ReapplyHandling = logsapi.ReapplyHandlingIgnoreUnchanged
}

type Config struct {
	DataDir          string
	BindAddress      string   // apiserver bind, e.g. 0.0.0.0
	AdvertiseIP      net.IP   // IP nodes use to reach the apiserver
	ExtraSANs        []string // extra DNS/IP SANs for the serving cert
	APIPort          int
	EtcdPort         int
	ServiceCIDR      string
	ClusterCIDR      string
	DisableCM        bool
	DisableSched     bool
	EtcdUnsafeNoSync bool // benchmark knob: skip fsync in etcd
}

type ControlPlane struct {
	Cfg             Config
	CA              *pki.CA
	AdminKubeconfig string
	Server          string
}

func ip(s string) net.IP { return net.ParseIP(s) }

// Prepare generates PKI and kubeconfigs (idempotent).
func Prepare(cfg Config) (*ControlPlane, error) {
	tlsDir := filepath.Join(cfg.DataDir, "tls")
	ca, err := pki.LoadOrCreateCA(tlsDir)
	if err != nil {
		return nil, err
	}
	dns := []string{"localhost", "kubernetes", "kubernetes.default", "kubernetes.default.svc", "kubernetes.default.svc.cluster.local"}
	ips := []net.IP{ip("127.0.0.1"), ip("10.43.0.1"), cfg.AdvertiseIP}
	if h, err := os.Hostname(); err == nil {
		dns = append(dns, h)
	}
	for _, s := range cfg.ExtraSANs {
		if p := net.ParseIP(s); p != nil {
			ips = append(ips, p)
		} else {
			dns = append(dns, s)
		}
	}
	if _, _, err := ca.Issue("apiserver", "kube-apiserver", nil, dns, ips, true); err != nil {
		return nil, err
	}
	if _, _, err := ca.Issue("apiserver-kubelet-client", "kube-apiserver-kubelet-client", []string{"system:masters"}, nil, nil, false); err != nil {
		return nil, err
	}
	if _, _, err := pki.ServiceAccountKey(tlsDir); err != nil {
		return nil, err
	}
	server := fmt.Sprintf("https://127.0.0.1:%d", cfg.APIPort)
	cp := &ControlPlane{Cfg: cfg, CA: ca, Server: server}
	for _, u := range []struct{ name, cn, org string }{
		{"admin", "kvz-admin", "system:masters"},
		{"controller-manager", "system:kube-controller-manager", ""},
		{"scheduler", "system:kube-scheduler", ""},
	} {
		var orgs []string
		if u.org != "" {
			orgs = []string{u.org}
		}
		crt, key, err := ca.Issue(u.name, u.cn, orgs, nil, nil, false)
		if err != nil {
			return nil, err
		}
		kc := filepath.Join(cfg.DataDir, "cred", u.name+".kubeconfig")
		if err := pki.WriteKubeconfig(kc, server, filepath.Join(tlsDir, "ca.crt"), crt, key, u.name); err != nil {
			return nil, err
		}
	}
	cp.AdminKubeconfig = filepath.Join(cfg.DataDir, "cred", "admin.kubeconfig")
	return cp, nil
}

// NodeKubeconfig issues a node identity and writes a kubeconfig for it that
// points at server (use the advertise IP for remote agents).
func (cp *ControlPlane) NodeKubeconfig(nodeName, server, out string) error {
	crt, key, err := cp.CA.Issue("node-"+nodeName, "system:node:"+nodeName, []string{"system:nodes"}, nil, nil, false)
	if err != nil {
		return err
	}
	return pki.WriteKubeconfig(out, server, filepath.Join(cp.CA.Dir, "ca.crt"), crt, key, "node-"+nodeName)
}

func (cp *ControlPlane) startEtcd(ctx context.Context) (*embed.Etcd, error) {
	c := embed.NewConfig()
	c.Dir = filepath.Join(cp.Cfg.DataDir, "etcd")
	c.Name = "kvz"
	c.LogLevel = "error"
	cu, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", cp.Cfg.EtcdPort))
	pu, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", cp.Cfg.EtcdPort+1))
	c.ListenClientUrls, c.AdvertiseClientUrls = []url.URL{*cu}, []url.URL{*cu}
	c.ListenPeerUrls, c.AdvertisePeerUrls = []url.URL{*pu}, []url.URL{*pu}
	c.InitialCluster = c.InitialClusterFromName(c.Name)
	c.UnsafeNoFsync = cp.Cfg.EtcdUnsafeNoSync
	c.QuotaBackendBytes = 8 << 30
	c.AutoCompactionMode = "periodic"
	c.AutoCompactionRetention = "5m"
	e, err := embed.StartEtcd(c)
	if err != nil {
		return nil, err
	}
	select {
	case <-e.Server.ReadyNotify():
	case <-time.After(60 * time.Second):
		e.Close()
		return nil, fmt.Errorf("etcd did not become ready")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return e, nil
}

// Run starts every component and blocks until ctx is done. ready is closed
// once the apiserver answers /readyz.
func (cp *ControlPlane) Run(ctx context.Context, ready chan<- struct{}) error {
	t0 := time.Now()
	e, err := cp.startEtcd(ctx)
	if err != nil {
		return err
	}
	defer e.Close()
	klog.InfoS("etcd ready", "elapsed", time.Since(t0))

	d, tlsDir, cred := cp.Cfg.DataDir, filepath.Join(cp.Cfg.DataDir, "tls"), filepath.Join(cp.Cfg.DataDir, "cred")
	p := func(n string) string { return filepath.Join(tlsDir, n) }
	apiArgs := []string{
		"--etcd-servers=" + fmt.Sprintf("http://127.0.0.1:%d", cp.Cfg.EtcdPort),
		"--bind-address=" + cp.Cfg.BindAddress,
		fmt.Sprintf("--secure-port=%d", cp.Cfg.APIPort),
		"--advertise-address=" + cp.Cfg.AdvertiseIP.String(),
		"--tls-cert-file=" + p("apiserver.crt"),
		"--tls-private-key-file=" + p("apiserver.key"),
		"--client-ca-file=" + p("ca.crt"),
		"--kubelet-client-certificate=" + p("apiserver-kubelet-client.crt"),
		"--kubelet-client-key=" + p("apiserver-kubelet-client.key"),
		"--service-account-key-file=" + p("sa.pub"),
		"--service-account-signing-key-file=" + p("sa.key"),
		"--service-account-issuer=https://kubernetes.default.svc.cluster.local",
		"--api-audiences=https://kubernetes.default.svc.cluster.local",
		"--service-cluster-ip-range=" + cp.Cfg.ServiceCIDR,
		"--authorization-mode=Node,RBAC",
		"--enable-admission-plugins=NodeRestriction",
		"--allow-privileged=true",
		"--kubelet-preferred-address-types=InternalIP,Hostname",
		"--profiling=false",
		"--anonymous-auth=false",
		"--endpoint-reconciler-type=lease",
		"--storage-backend=etcd3",
		"--watch-cache=true",
		"--max-requests-inflight=800",
		"--max-mutating-requests-inflight=400",
		"--delete-collection-workers=16", // default 1: DeleteCollection deleted pods one by one
	}
	_ = d
	api := apiapp.NewAPIServerCommand()
	api.SetArgs(apiArgs)
	errc := make(chan error, 3)
	go func() { errc <- fmt.Errorf("kube-apiserver exited: %w", api.ExecuteContext(ctx)) }()

	if err := cp.waitReady(ctx); err != nil {
		return err
	}
	klog.InfoS("kube-apiserver ready", "elapsed", time.Since(t0))
	if ready != nil {
		close(ready)
	}

	if !cp.Cfg.DisableCM {
		cm := cmapp.NewControllerManagerCommand()
		cm.SetArgs([]string{
			"--kubeconfig=" + filepath.Join(cred, "controller-manager.kubeconfig"),
			"--authentication-kubeconfig=" + filepath.Join(cred, "controller-manager.kubeconfig"),
			"--authorization-kubeconfig=" + filepath.Join(cred, "controller-manager.kubeconfig"),
			"--service-account-private-key-file=" + p("sa.key"),
			"--root-ca-file=" + p("ca.crt"),
			"--cluster-signing-cert-file=" + p("ca.crt"),
			"--cluster-signing-key-file=" + p("ca.key"),
			"--use-service-account-credentials=true",
			"--leader-elect=false",
			"--bind-address=127.0.0.1",
			"--secure-port=0",
			"--cluster-cidr=" + cp.Cfg.ClusterCIDR,
			"--allocate-node-cidrs=false",
			"--controllers=*,-nodeipam,-route,-service,-cloud-node-lifecycle,-persistentvolume-binder,-attachdetach,-persistentvolume-expander",
			"--kube-api-qps=200", "--kube-api-burst=400",
			"--concurrent-deployment-syncs=10", "--concurrent-replicaset-syncs=10",
			"--profiling=false",
		})
		go func() { errc <- fmt.Errorf("kube-controller-manager exited: %w", cm.ExecuteContext(ctx)) }()
	}
	if !cp.Cfg.DisableSched {
		sargs := []string{
			"--kubeconfig=" + filepath.Join(cred, "scheduler.kubeconfig"),
			"--authentication-kubeconfig=" + filepath.Join(cred, "scheduler.kubeconfig"),
			"--authorization-kubeconfig=" + filepath.Join(cred, "scheduler.kubeconfig"),
			"--leader-elect=false",
			"--bind-address=127.0.0.1",
			"--secure-port=0",
			"--profiling=false",
			"--kube-api-qps=500", "--kube-api-burst=1000",
		}
		go func() { errc <- fmt.Errorf("kube-scheduler exited: %w", runScheduler(ctx, sargs)) }()
	}
	select {
	case <-ctx.Done():
		return nil
	case err := <-errc:
		return err
	}
}

// runScheduler avoids NewSchedulerCommand's RunE, which installs a second
// process-wide signal handler and panics when embedded.
func runScheduler(ctx context.Context, args []string) error {
	opts := schedopts.NewOptions()
	fs := pflag.NewFlagSet("kube-scheduler", pflag.ContinueOnError)
	for _, f := range opts.Flags.FlagSets {
		fs.AddFlagSet(f)
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := opts.ComponentGlobalsRegistry.Set(); err != nil {
		return err
	}
	cc, sched, err := schedapp.Setup(ctx, opts)
	if err != nil {
		return err
	}
	return schedapp.Run(ctx, cc, sched)
}

func (cp *ControlPlane) waitReady(ctx context.Context) error {
	pool := x509.NewCertPool()
	pool.AddCert(cp.CA.Cert)
	cert, err := tls.LoadX509KeyPair(filepath.Join(cp.CA.Dir, "admin.crt"), filepath.Join(cp.CA.Dir, "admin.key"))
	if err != nil {
		return err
	}
	cl := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, Certificates: []tls.Certificate{cert}}}}
	for {
		resp, err := cl.Get(cp.Server + "/readyz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
