// Package controlplane runs etcd, kube-apiserver, kube-controller-manager and
// kube-scheduler in-process, natively on darwin (no Linux VM).
package controlplane

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"github.com/GrowlyX/kvz/pkg/egress"
	"go.etcd.io/etcd/client/pkg/v3/transport"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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
	// HA (embedded etcd cluster): PeerIP enables mTLS etcd peers on the node
	// IP; InitialCluster is set when joining an existing cluster.
	EtcdName       string
	PeerIP         string
	InitialCluster string
	LeaderElect    bool
	// SecretsEncryption encrypts Secrets at rest in etcd (aescbc, k3s-style).
	SecretsEncryption bool
	// Extra component flags ("name=value"), appended last so they override.
	ExtraAPIArgs, ExtraCMArgs, ExtraSchedArgs []string
}

// leaderElect: single servers skip leader election (no lease to wait for on
// restart); HA servers elect one active controller-manager/scheduler.
func leaderElect(on bool) string {
	if on {
		return "--leader-elect=true"
	}
	return "--leader-elect=false"
}

func extraArgs(in []string) []string {
	out := make([]string, 0, len(in))
	for _, a := range in {
		if !strings.HasPrefix(a, "--") {
			a = "--" + a
		}
		out = append(out, a)
	}
	return out
}

// encryptionConfig writes (once) an EncryptionConfiguration with a random
// AES-CBC key; identity stays as a fallback reader for pre-existing data.
func encryptionConfig(credDir string) (string, error) {
	path := filepath.Join(credDir, "encryption-config.yaml")
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", err
	}
	cfg := `apiVersion: apiserver.config.k8s.io/v1
kind: EncryptionConfiguration
resources:
- resources: [secrets]
  providers:
  - aescbc:
      keys:
      - name: kvz-` + time.Now().UTC().Format("20060102") + `
        secret: ` + base64.StdEncoding.EncodeToString(key) + `
  - identity: {}
`
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		return "", err
	}
	return path, os.WriteFile(path, []byte(cfg), 0o600)
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
	// identity the aggregator presents to extension API servers (metrics.k8s.io)
	if _, _, err := ca.Issue("front-proxy-client", "front-proxy-client", nil, nil, nil, false); err != nil {
		return nil, err
	}
	if _, _, err := pki.ServiceAccountKey(tlsDir); err != nil {
		return nil, err
	}
	if cfg.PeerIP != "" {
		if _, _, err := ca.Issue("etcd-peer", "etcd-peer", nil, []string{"localhost"}, []net.IP{net.ParseIP(cfg.PeerIP), net.ParseIP("127.0.0.1")}, true); err != nil {
			return nil, err
		}
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
	c.Name = cp.Cfg.EtcdName
	if c.Name == "" {
		c.Name = "kvz"
	}
	c.LogLevel = "error"
	cu, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", cp.Cfg.EtcdPort))
	pu, _ := url.Parse(cp.PeerURL())
	c.ListenClientUrls, c.AdvertiseClientUrls = []url.URL{*cu}, []url.URL{*cu}
	c.ListenPeerUrls, c.AdvertisePeerUrls = []url.URL{*pu}, []url.URL{*pu}
	c.InitialCluster = c.InitialClusterFromName(c.Name)
	if cp.Cfg.PeerIP != "" {
		tlsDir := filepath.Join(cp.Cfg.DataDir, "tls")
		c.PeerTLSInfo = transport.TLSInfo{CertFile: filepath.Join(tlsDir, "etcd-peer.crt"), KeyFile: filepath.Join(tlsDir, "etcd-peer.key"),
			TrustedCAFile: filepath.Join(tlsDir, "ca.crt"), ClientCertAuth: true}
		c.PeerAutoTLS = false
	}
	if cp.Cfg.InitialCluster != "" {
		c.InitialCluster = cp.Cfg.InitialCluster
		c.ClusterState = embed.ClusterStateFlagExisting
	}
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
	if cp.Cfg.PeerIP != "" {
		go cp.haLoop(ctx, uint64(e.Server.MemberID()))
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
	// apiserver -> pod network (webhooks, aggregated APIs, proxy) via kvz's
	// CONNECT proxy; see pkg/egress.
	egSock := filepath.Join(d, "egress.sock")
	egCfg := filepath.Join(d, "egress-selector.yaml")
	if err := os.WriteFile(egCfg, []byte(egress.ConfigYAML(egSock)), 0o600); err != nil {
		return err
	}
	go func() {
		if err := egress.Serve(ctx, egSock); err != nil {
			klog.ErrorS(err, "egress proxy")
		}
	}()
	apiArgs := []string{
		"--egress-selector-config-file=" + egCfg,
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
		"--enable-aggregator-routing=true", // extension APIs are served from the host (endpoints), not via a ClusterIP
		"--proxy-client-cert-file=" + p("front-proxy-client.crt"),
		"--proxy-client-key-file=" + p("front-proxy-client.key"),
		"--requestheader-client-ca-file=" + p("ca.crt"),
		"--requestheader-allowed-names=front-proxy-client",
		"--requestheader-username-headers=X-Remote-User",
		"--requestheader-group-headers=X-Remote-Group",
		"--requestheader-extra-headers-prefix=X-Remote-Extra-",
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
	// Once enabled, encryption stays on: data written encrypted can only be
	// read with the key, so an existing config is always used.
	_, encErr := os.Stat(filepath.Join(cred, "encryption-config.yaml"))
	if cp.Cfg.SecretsEncryption || encErr == nil {
		ep, err := encryptionConfig(cred)
		if err != nil {
			return err
		}
		apiArgs = append(apiArgs, "--encryption-provider-config="+ep, "--encryption-provider-config-automatic-reload=true")
	}
	apiArgs = append(apiArgs, extraArgs(cp.Cfg.ExtraAPIArgs)...)
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
		cm.SetArgs(append([]string{
			"--kubeconfig=" + filepath.Join(cred, "controller-manager.kubeconfig"),
			"--authentication-kubeconfig=" + filepath.Join(cred, "controller-manager.kubeconfig"),
			"--authorization-kubeconfig=" + filepath.Join(cred, "controller-manager.kubeconfig"),
			"--service-account-private-key-file=" + p("sa.key"),
			"--root-ca-file=" + p("ca.crt"),
			"--cluster-signing-cert-file=" + p("ca.crt"),
			"--cluster-signing-key-file=" + p("ca.key"),
			"--use-service-account-credentials=true",
			leaderElect(cp.Cfg.LeaderElect),
			"--bind-address=127.0.0.1",
			"--secure-port=0",
			"--cluster-cidr=" + cp.Cfg.ClusterCIDR,
			"--allocate-node-cidrs=false",
			"--controllers=*,-nodeipam,-route,-service,-cloud-node-lifecycle,-attachdetach,-persistentvolume-expander",
			"--kube-api-qps=200", "--kube-api-burst=400",
			"--concurrent-deployment-syncs=10", "--concurrent-replicaset-syncs=10",
			"--leader-elect-lease-duration=8s", "--leader-elect-renew-deadline=6s", "--leader-elect-retry-period=1s",
			"--profiling=false",
		}, extraArgs(cp.Cfg.ExtraCMArgs)...))
		go func() { errc <- fmt.Errorf("kube-controller-manager exited: %w", cm.ExecuteContext(ctx)) }()
	}
	if !cp.Cfg.DisableSched {
		sargs := []string{
			"--kubeconfig=" + filepath.Join(cred, "scheduler.kubeconfig"),
			"--authentication-kubeconfig=" + filepath.Join(cred, "scheduler.kubeconfig"),
			"--authorization-kubeconfig=" + filepath.Join(cred, "scheduler.kubeconfig"),
			leaderElect(cp.Cfg.LeaderElect),
			"--leader-elect-lease-duration=8s", "--leader-elect-renew-deadline=6s", "--leader-elect-retry-period=1s",
			"--bind-address=127.0.0.1",
			"--secure-port=0",
			"--profiling=false",
			"--kube-api-qps=500", "--kube-api-burst=1000",
		}
		sargs = append(sargs, extraArgs(cp.Cfg.ExtraSchedArgs)...)
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
