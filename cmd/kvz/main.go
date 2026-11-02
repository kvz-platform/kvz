// kvz: a single-binary, k3s-style Kubernetes distribution that runs natively
// on macOS. The control plane runs in-process (no Linux VM); pods run as
// Linux micro-VMs (Apple container), macOS VMs (Virtualization.framework) or
// native darwin processes.
package main

import (
	"context"
	"fmt"
	"github.com/GrowlyX/kvz/pkg/lb"
	"net"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	"github.com/GrowlyX/kvz/pkg/controlplane"
	"github.com/GrowlyX/kvz/pkg/join"
	"github.com/GrowlyX/kvz/pkg/version"
	kubectlcmd "k8s.io/kubectl/pkg/cmd"

	"github.com/GrowlyX/kvz/pkg/deploy"
	"github.com/GrowlyX/kvz/pkg/metrics"
	"github.com/GrowlyX/kvz/pkg/node"
	"github.com/GrowlyX/kvz/pkg/pki"
	"github.com/GrowlyX/kvz/pkg/storage"
	aggclient "k8s.io/kube-aggregator/pkg/client/clientset_generated/clientset"
)

func main() {
	if addr := os.Getenv("KVZ_PPROF"); addr != "" {
		go func() { _ = http.ListenAndServe(addr, nil) }()
	}
	root := &cobra.Command{Use: "kvz", Short: "Native-macOS Kubernetes (k3s-style, single binary)", SilenceUsage: true}
	root.AddCommand(serverCmd(), agentCmd(), versionCmd(), kubectlCmd(), installCmd(), uninstallCmd(), snapshotCmd())
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

// kubectlCmd embeds a version-matched kubectl (like `k3s kubectl`), defaulting
// to the local kvz kubeconfig.
func kubectlCmd() *cobra.Command {
	c := &cobra.Command{Use: "kubectl", Short: "Run an embedded, version-matched kubectl", DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if os.Getenv("KUBECONFIG") == "" {
				home, _ := os.UserHomeDir()
				os.Setenv("KUBECONFIG", filepath.Join(home, ".kvz", "kubeconfig.yaml"))
			}
			kc := kubectlcmd.NewDefaultKubectlCommand()
			kc.SetArgs(args)
			return kc.Execute()
		}}
	return c
}

func versionCmd() *cobra.Command {
	return &cobra.Command{Use: "version", Short: "Print the kvz and Kubernetes versions", Run: func(*cobra.Command, []string) { fmt.Println(version.String()) }}
}

func signalCtx() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func defaultIP() string {
	ifs, _ := net.Interfaces()
	var fallback string
	_, cgnat, _ := net.ParseCIDR("100.64.0.0/10")
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil {
				continue
			}
			if cgnat.Contains(ipn.IP) {
				return ipn.IP.String() // prefer Tailscale
			}
			if fallback == "" && strings.HasPrefix(i.Name, "en") {
				fallback = ipn.IP.String()
			}
		}
	}
	if fallback == "" {
		fallback = "127.0.0.1"
	}
	return fallback
}

func hostname() string {
	h, _ := os.Hostname()
	h = strings.ToLower(strings.TrimSuffix(h, ".local"))
	return h
}

type nodeFlags struct {
	name, nodeIP, dataDir, defRuntime string
	backends                          []string
	kubeletPort, maxPods, overlayPort int
}

func (nf *nodeFlags) add(c *cobra.Command, defDir string) {
	f := c.Flags()
	f.StringVar(&nf.name, "node-name", hostname(), "node name")
	f.StringVar(&nf.nodeIP, "node-ip", defaultIP(), "node IP (defaults to Tailscale IP if present)")
	f.StringVar(&nf.dataDir, "data-dir", defDir, "data directory")
	f.StringSliceVar(&nf.backends, "runtimes", []string{"linux", "darwin", "macos"}, "pod runtimes to enable (linux, apple, darwin, macos)")
	f.StringVar(&nf.defRuntime, "default-runtime", "linux", "runtime for pods without runtimeClassName")
	f.IntVar(&nf.kubeletPort, "kubelet-port", 10250, "kubelet API port")
	f.IntVar(&nf.maxPods, "max-pods", 1000, "pod capacity")
	f.IntVar(&nf.overlayPort, "overlay-port", 6446, "cross-node pod network port")
}

func serverCmd() *cobra.Command {
	var nf nodeFlags
	var apiPort, etcdPort int
	var disableAgent, noSync bool
	var etcdSync string
	var disable []string
	var restorePath string
	var snapInterval time.Duration
	var snapRetention int
	var secretsEnc, clusterInit bool
	var joinServer, joinToken string
	var apiExtra, cmExtra, schedExtra []string
	var sans []string
	home, _ := os.UserHomeDir()
	c := &cobra.Command{
		Use:   "server",
		Short: "Run the control plane (and a local node)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signalCtx()
			defer cancel()
			t0 := time.Now()
			if os.Getenv("KVZ_ETCD_SYNC") == "" {
				os.Setenv("KVZ_ETCD_SYNC", etcdSync)
			}
			serverDir := filepath.Join(nf.dataDir, "server")
			ha, etcdName, initialCluster, err := haSetup(serverDir, nf, clusterInit, joinServer, joinToken, etcdPort)
			if err != nil {
				return err
			}
			peerIP := ""
			if ha {
				peerIP = nf.nodeIP
			}
			cp, err := controlplane.Prepare(controlplane.Config{
				DataDir: serverDir, BindAddress: "0.0.0.0", AdvertiseIP: net.ParseIP(nf.nodeIP),
				ExtraSANs: append(sans, nf.name), APIPort: apiPort, EtcdPort: etcdPort,
				ServiceCIDR: "10.43.0.0/16", ClusterCIDR: "10.42.0.0/16", EtcdUnsafeNoSync: noSync,
				SecretsEncryption: secretsEnc, ExtraAPIArgs: apiExtra, ExtraCMArgs: cmExtra, ExtraSchedArgs: schedExtra,
				EtcdName: etcdName, PeerIP: peerIP, InitialCluster: initialCluster, LeaderElect: ha,
			})
			if err != nil {
				return err
			}
			// External admin kubeconfig.
			adminCrt, adminKey := filepath.Join(cp.CA.Dir, "admin.crt"), filepath.Join(cp.CA.Dir, "admin.key")
			// Local kubeconfig talks to loopback: routing to our own Tailscale/LAN
			// address went through the VPN's network extension and cut API
			// throughput ~3x. kubeconfig-external.yaml is for other machines.
			ext := filepath.Join(nf.dataDir, "kubeconfig.yaml")
			if err := pki.WriteKubeconfig(ext, fmt.Sprintf("https://127.0.0.1:%d", apiPort), filepath.Join(cp.CA.Dir, "ca.crt"), adminCrt, adminKey, "admin"); err != nil {
				return err
			}
			if err := pki.WriteKubeconfig(filepath.Join(nf.dataDir, "kubeconfig-external.yaml"), fmt.Sprintf("https://%s:%d", nf.nodeIP, apiPort), filepath.Join(cp.CA.Dir, "ca.crt"), adminCrt, adminKey, "admin"); err != nil {
				return err
			}
			token, secret, err := join.LoadOrCreateToken(filepath.Join(nf.dataDir, "server"), cp.CA)
			if err != nil {
				return err
			}
			if restorePath != "" {
				if err := restoreSnapshot(restorePath, filepath.Join(nf.dataDir, "server", "etcd"), etcdPort); err != nil {
					return fmt.Errorf("restore %s: %w", restorePath, err)
				}
				klog.InfoS("restored datastore from snapshot", "path", restorePath)
			}
			ready := make(chan struct{})
			errc := make(chan error, 3)
			go func() { errc <- cp.Run(ctx, ready) }()
			select {
			case <-ready:
			case err := <-errc:
				return err
			}
			klog.InfoS("control plane ready", "elapsed", time.Since(t0), "kubeconfig", ext)
			if err := bootstrap(ctx, cp.AdminKubeconfig); err != nil {
				return err
			}
			if snapInterval > 0 {
				go func() {
					for {
						select {
						case <-ctx.Done():
							return
						case <-time.After(snapInterval):
						}
						if p, err := saveSnapshot(ctx, etcdPort, snapshotDir(nf.dataDir), "scheduled"); err != nil {
							klog.ErrorS(err, "scheduled etcd snapshot")
						} else {
							klog.InfoS("etcd snapshot saved", "path", p)
							pruneSnapshots(snapshotDir(nf.dataDir), snapRetention)
						}
					}
				}()
			}
			disabled := map[string]bool{}
			for _, d := range disable {
				disabled[strings.TrimSpace(d)] = true
			}
			manifests := filepath.Join(nf.dataDir, "server", "manifests")
			if err := deploy.WriteBundled(manifests, disabled); err != nil {
				klog.ErrorS(err, "bundled manifests")
			}
			if cfg, err := clientcmd.BuildConfigFromFlags("", cp.AdminKubeconfig); err == nil {
				go deploy.Run(ctx, cfg, manifests)
			}
			if adm, err := adminClient(cp.AdminKubeconfig); err == nil {
				if !disabled["local-storage"] {
					if err := storage.EnsureDefaultClass(ctx, adm); err != nil {
						klog.ErrorS(err, "local-path storage class")
					}
					go storage.Run(ctx, adm, nf.name)
				}
				tls := func(n string) string { return filepath.Join(cp.CA.Dir, n) }
				if disabled["metrics-server"] {
					// skip
				} else if ms, err := metrics.New(adm, tls("ca.crt"), tls("apiserver-kubelet-client.crt"), tls("apiserver-kubelet-client.key")); err == nil {
					go func() {
						if err := ms.Run(ctx, fmt.Sprintf(":%d", apiPort+4), tls("ca.crt"), tls("apiserver.crt"), tls("apiserver.key")); err != nil {
							klog.ErrorS(err, "metrics API")
						}
					}()
					if cfg, err := clientcmd.BuildConfigFromFlags("", cp.AdminKubeconfig); err == nil {
						if agg, err := aggclient.NewForConfig(cfg); err == nil {
							if err := metrics.Register(ctx, adm, agg, nf.nodeIP, apiPort+4); err != nil {
								klog.ErrorS(err, "register metrics API")
							}
						}
					}
				}
			}
			adm, err := adminClient(cp.AdminKubeconfig)
			if err != nil {
				return err
			}
			cidrs := join.NewCIDRAllocator(adm, filepath.Join(serverDir, "podcidrs.json"))
			localCIDR, err := cidrs.For(nf.name)
			if err != nil {
				return fmt.Errorf("pod CIDR: %w", err)
			}
			var sj join.ServerJoiner
			if ha {
				sj = cp
			}
			go func() {
				errc <- join.Serve(fmt.Sprintf(":%d", apiPort+1), secret, cp.CA,
					filepath.Join(cp.CA.Dir, "apiserver.crt"), filepath.Join(cp.CA.Dir, "apiserver.key"), cidrs, sj)
			}()
			fmt.Fprintf(os.Stderr, "\nkvz server ready in %s\n  export KUBECONFIG=%s\n  join: kvz agent --server https://%s:%d --token %s\n\n",
				time.Since(t0).Round(time.Millisecond), ext, nf.nodeIP, apiPort, token)
			_ = os.WriteFile(filepath.Join(nf.dataDir, "join-token"), []byte(token+"\n"), 0o600)
			if !disableAgent {
				crt, key, err := cp.CA.Issue("node-"+nf.name, "system:node:"+nf.name, []string{"system:nodes"}, []string{nf.name}, []net.IP{net.ParseIP(nf.nodeIP)}, true)
				if err != nil {
					return err
				}
				kc := filepath.Join(nf.dataDir, "agent", "kubeconfig")
				if err := pki.WriteKubeconfig(kc, cp.Server, filepath.Join(cp.CA.Dir, "ca.crt"), crt, key, "node"); err != nil {
					return err
				}
				go func() {
					errc <- node.Run(ctx, node.Options{Name: nf.name, Kubeconfig: kc, DataDir: filepath.Join(nf.dataDir, "agent"),
						NodeIP: nf.nodeIP, ListenPort: nf.kubeletPort, ServingCert: crt, ServingKey: key,
						ClientCA: filepath.Join(cp.CA.Dir, "ca.crt"), DefaultHandler: nf.defRuntime, MaxPods: nf.maxPods, Backends: nf.backends,
						PodCIDR: localCIDR, OverlaySecret: join.OverlaySecret(token), OverlayPort: nf.overlayPort})
				}()
			}
			select {
			case <-ctx.Done():
				return nil
			case err := <-errc:
				return err
			}
		},
	}
	nf.add(c, filepath.Join(home, ".kvz"))
	c.Flags().IntVar(&apiPort, "https-listen-port", 6443, "apiserver port (join endpoint uses port+1)")
	c.Flags().BoolVar(&secretsEnc, "secrets-encryption", false, "encrypt Secrets at rest (aescbc)")
	c.Flags().BoolVar(&clusterInit, "cluster-init", false, "start an HA-capable cluster (etcd peers on the node IP, mTLS) that more servers can join")
	c.Flags().StringVar(&joinServer, "server", "", "join an existing HA cluster as another server (URL of any server, e.g. https://10.0.0.1:6443)")
	c.Flags().StringVar(&joinToken, "token", "", "join token of the cluster to join (with --server)")
	c.Flags().StringArrayVar(&apiExtra, "kube-apiserver-arg", nil, "extra kube-apiserver flag (name=value), repeatable")
	c.Flags().StringArrayVar(&cmExtra, "kube-controller-manager-arg", nil, "extra kube-controller-manager flag (name=value), repeatable")
	c.Flags().StringArrayVar(&schedExtra, "kube-scheduler-arg", nil, "extra kube-scheduler flag (name=value), repeatable")
	c.Flags().IntVar(&etcdPort, "etcd-port", 2379, "embedded etcd client port (peer uses +1)")
	c.Flags().BoolVar(&disableAgent, "disable-agent", false, "do not run a local node")
	c.Flags().BoolVar(&noSync, "etcd-unsafe-no-fsync", false, "benchmark knob: disable etcd fsync")
	c.Flags().StringVar(&restorePath, "cluster-reset-restore-path", "", "restore the datastore from this etcd snapshot before starting")
	c.Flags().DurationVar(&snapInterval, "etcd-snapshot-interval", 12*time.Hour, "automatic etcd snapshot interval (0 disables)")
	c.Flags().IntVar(&snapRetention, "etcd-snapshot-retention", 5, "number of automatic snapshots to keep")
	c.Flags().StringSliceVar(&disable, "disable", nil, "bundled components to skip: traefik, helm-controller, local-storage, metrics-server")
	c.Flags().StringVar(&etcdSync, "etcd-sync", "barrier", "etcd WAL sync on macOS: full (F_FULLFSYNC), barrier (F_BARRIERFSYNC, crash-consistent), none")
	c.Flags().StringSliceVar(&sans, "tls-san", nil, "extra apiserver SANs")
	return c
}

func agentCmd() *cobra.Command {
	var nf nodeFlags
	var server, token string
	home, _ := os.UserHomeDir()
	c := &cobra.Command{
		Use:   "agent",
		Short: "Join this Mac to a kvz server as a node",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signalCtx()
			defer cancel()
			u := strings.TrimSuffix(server, "/")
			host, port, err := net.SplitHostPort(strings.TrimPrefix(u, "https://"))
			if err != nil {
				return err
			}
			var p int
			fmt.Sscan(port, &p)
			b, err := join.Fetch(fmt.Sprintf("https://%s:%d", host, p+1), token, join.Request{Name: nf.name, IP: nf.nodeIP})
			if err != nil {
				return err
			}
			dir := filepath.Join(nf.dataDir, "agent")
			_ = os.MkdirAll(dir, 0o700)
			ca, crt, key := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "node.crt"), filepath.Join(dir, "node.key")
			for path, data := range map[string][]byte{ca: b.CA, crt: b.Cert, key: b.Key} {
				if err := os.WriteFile(path, data, 0o600); err != nil {
					return err
				}
			}
			// client-side apiserver load balancer: fails over across servers
			apiLB, err := lb.Start(ctx, net.JoinHostPort(host, port), filepath.Join(dir, "servers.json"))
			if err != nil {
				return err
			}
			kc := filepath.Join(dir, "kubeconfig")
			if err := pki.WriteKubeconfig(kc, apiLB.URL(), ca, crt, key, "node"); err != nil {
				return err
			}
			if lcs, err := adminClient(kc); err == nil {
				go apiLB.Watch(ctx, lcs)
			}
			return node.Run(ctx, node.Options{Name: nf.name, Kubeconfig: kc, DataDir: dir, NodeIP: nf.nodeIP,
				ListenPort: nf.kubeletPort, ServingCert: crt, ServingKey: key, ClientCA: ca,
				DefaultHandler: nf.defRuntime, MaxPods: nf.maxPods, Backends: nf.backends,
				PodCIDR: b.PodCIDR, OverlaySecret: join.OverlaySecret(token), OverlayPort: nf.overlayPort})
		},
	}
	nf.add(c, filepath.Join(home, ".kvz"))
	c.Flags().StringVar(&server, "server", "", "server URL, e.g. https://10.0.0.1:6443")
	c.Flags().StringVar(&token, "token", "", "join token")
	_ = c.MarkFlagRequired("server")
	_ = c.MarkFlagRequired("token")
	return c
}

func adminClient(kubeconfig string) (kubernetes.Interface, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, err
	}
	cfg.QPS, cfg.Burst = 200, 400
	return kubernetes.NewForConfig(cfg)
}

// bootstrap installs RBAC for nodes and the RuntimeClasses.
func bootstrap(ctx context.Context, kubeconfig string) error {
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	cr := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "kvz:node"}, Rules: []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"secrets", "configmaps", "services", "pods"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{"discovery.k8s.io"}, Resources: []string{"endpointslices"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{""}, Resources: []string{"nodes", "namespaces", "persistentvolumes"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{"networking.k8s.io"}, Resources: []string{"networkpolicies"}, Verbs: []string{"get", "list", "watch"}},
	}}
	if _, err := cs.RbacV1().ClusterRoles().Create(ctx, cr, metav1.CreateOptions{}); apierrors.IsAlreadyExists(err) {
		_, err = cs.RbacV1().ClusterRoles().Update(ctx, cr, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	crb := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "kvz:node"},
		RoleRef:  rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "kvz:node"},
		Subjects: []rbacv1.Subject{{APIGroup: "rbac.authorization.k8s.io", Kind: "Group", Name: "system:nodes"}}}
	if _, err := cs.RbacV1().ClusterRoleBindings().Create(ctx, crb, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	// kube-dns: the conventional cluster DNS service. Each node's pod switch
	// maps 10.43.0.10:53 to its local resolver (see node.Run).
	dns := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "kube-dns", Namespace: "kube-system",
		Labels: map[string]string{"k8s-app": "kube-dns", "kubernetes.io/name": "CoreDNS", "kubernetes.io/cluster-service": "true"}},
		Spec: corev1.ServiceSpec{ClusterIP: "10.43.0.10", Ports: []corev1.ServicePort{
			{Name: "dns", Port: 53, Protocol: corev1.ProtocolUDP}, {Name: "dns-tcp", Port: 53, Protocol: corev1.ProtocolTCP}}}}
	// Get first: creating an existing Service with a fixed ClusterIP fails
	// with "IP already allocated" (Invalid) rather than AlreadyExists.
	if _, err := cs.CoreV1().Services("kube-system").Get(ctx, "kube-dns", metav1.GetOptions{}); apierrors.IsNotFound(err) {
		if _, err := cs.CoreV1().Services("kube-system").Create(ctx, dns, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	} else if err != nil {
		return err
	}
	return node.EnsureRuntimeClasses(ctx, cs)
}

// haSetup decides the server's etcd identity. HA mode (--cluster-init or
// --server) is remembered in the data dir. Joining fetches the cluster PKI and
// an etcd initial-cluster value from an existing server (first start only).
func haSetup(serverDir string, nf nodeFlags, clusterInit bool, joinServer, joinToken string, etcdPort int) (ha bool, etcdName, initialCluster string, err error) {
	marker := filepath.Join(serverDir, "ha")
	_, statErr := os.Stat(marker)
	ha = clusterInit || joinServer != "" || statErr == nil
	etcdName = "kvz"
	if b, err := os.ReadFile(filepath.Join(serverDir, "etcd-name")); err == nil {
		etcdName = strings.TrimSpace(string(b))
	} else if joinServer != "" {
		etcdName = nf.name
	}
	if _, err := os.Stat(filepath.Join(serverDir, "etcd", "member")); err != nil && joinServer != "" {
		if joinToken == "" {
			return false, "", "", fmt.Errorf("--server needs --token")
		}
		u, err := url.Parse(joinServer)
		if err != nil || u.Host == "" {
			return false, "", "", fmt.Errorf("bad --server URL %q", joinServer)
		}
		host, port, err := net.SplitHostPort(u.Host)
		if err != nil {
			host, port = u.Host, "6443"
		}
		p, _ := strconv.Atoi(port)
		peerURL := fmt.Sprintf("https://%s:%d", nf.nodeIP, etcdPort+1)
		var sb *join.ServerBundle
		for attempt := 0; ; attempt++ {
			sb, err = join.FetchServer(fmt.Sprintf("https://%s:%d", host, p+1), joinToken, join.ServerJoinRequest{Name: etcdName, IP: nf.nodeIP, PeerURL: peerURL})
			if err == nil || attempt >= 10 {
				break
			}
			klog.InfoS("server join failed, retrying", "err", err)
			time.Sleep(3 * time.Second)
		}
		if err != nil {
			return false, "", "", fmt.Errorf("join %s: %w", joinServer, err)
		}
		for rel, b := range sb.Files {
			dst := filepath.Join(serverDir, filepath.Clean("/" + rel)[1:])
			if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
				return false, "", "", err
			}
			if err := os.WriteFile(dst, b, 0o600); err != nil {
				return false, "", "", err
			}
		}
		initialCluster = sb.InitialCluster
		klog.InfoS("joined HA cluster", "server", joinServer, "etcdInitialCluster", initialCluster)
	}
	if ha {
		if err := os.MkdirAll(serverDir, 0o700); err != nil {
			return false, "", "", err
		}
		_ = os.WriteFile(marker, nil, 0o600)
		_ = os.WriteFile(filepath.Join(serverDir, "etcd-name"), []byte(etcdName+"\n"), 0o600)
	}
	return ha, etcdName, initialCluster, nil
}
