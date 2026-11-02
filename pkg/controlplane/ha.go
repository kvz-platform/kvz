package controlplane

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"
	"k8s.io/klog/v2"
)

// HA: servers form an embedded etcd cluster. Peers talk mTLS on
// https://<node-ip>:<etcd-port+1> with certificates from the cluster CA; a
// joining server is added as a learner and promoted once it has caught up.

func (cp *ControlPlane) etcdClient() (*clientv3.Client, error) {
	return clientv3.New(clientv3.Config{Endpoints: []string{fmt.Sprintf("http://127.0.0.1:%d", cp.Cfg.EtcdPort)},
		DialTimeout: 5 * time.Second, Logger: zap.NewNop()})
}

// PeerURL is this server's etcd peer URL.
func (cp *ControlPlane) PeerURL() string {
	if cp.Cfg.PeerIP == "" {
		return fmt.Sprintf("http://127.0.0.1:%d", cp.Cfg.EtcdPort+1)
	}
	return fmt.Sprintf("https://%s:%d", cp.Cfg.PeerIP, cp.Cfg.EtcdPort+1)
}

// AddMember adds a joining server as an etcd learner and returns the
// --initial-cluster value it must start with.
func (cp *ControlPlane) AddMember(ctx context.Context, name, peerURL string) (string, error) {
	if cp.Cfg.PeerIP == "" {
		return "", fmt.Errorf("this server was not started with --cluster-init (etcd peers are loopback-only)")
	}
	cli, err := cp.etcdClient()
	if err != nil {
		return "", err
	}
	defer cli.Close()
	ml, err := cli.MemberList(ctx)
	if err != nil {
		return "", err
	}
	for _, m := range ml.Members { // rejoin after a wipe: replace the stale member
		if m.Name == name || (len(m.PeerURLs) > 0 && m.PeerURLs[0] == peerURL) {
			if _, err := cli.MemberRemove(ctx, m.ID); err != nil {
				return "", fmt.Errorf("remove stale member %s: %w", m.Name, err)
			}
		}
	}
	resp, err := cli.MemberAddAsLearner(ctx, []string{peerURL})
	if err != nil {
		return "", err
	}
	var parts []string
	for _, m := range resp.Members {
		n := m.Name
		if m.ID == resp.Member.ID {
			n = name
		}
		if n == "" {
			continue // another member that never started
		}
		for _, u := range m.PeerURLs {
			parts = append(parts, n+"="+u)
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, ","), nil
}

// BootstrapFiles are the cluster-wide secrets a joining server needs (paths
// relative to the server data dir).
func (cp *ControlPlane) BootstrapFiles() (map[string][]byte, error) {
	out := map[string][]byte{}
	for _, f := range []string{"tls/ca.crt", "tls/ca.key", "tls/sa.key", "tls/sa.pub", "token", "cred/encryption-config.yaml"} {
		b, err := os.ReadFile(filepath.Join(cp.Cfg.DataDir, f))
		if err != nil {
			if os.IsNotExist(err) && f == "cred/encryption-config.yaml" {
				continue
			}
			return nil, err
		}
		out[f] = b
	}
	return out, nil
}

// haLoop keeps this member's advertised peer URL current (a single server
// converted with --cluster-init still advertises loopback) and promotes
// learners that have caught up.
func (cp *ControlPlane) haLoop(ctx context.Context, selfID uint64) {
	cli, err := cp.etcdClient()
	if err != nil {
		klog.ErrorS(err, "etcd client for HA loop")
		return
	}
	defer cli.Close()
	want := cp.PeerURL()
	if ml, err := cli.MemberList(ctx); err == nil {
		for _, m := range ml.Members {
			if m.ID == selfID && (len(m.PeerURLs) != 1 || m.PeerURLs[0] != want) {
				if _, err := cli.MemberUpdate(ctx, m.ID, []string{want}); err != nil {
					klog.ErrorS(err, "update etcd peer URL", "url", want)
				} else {
					klog.InfoS("etcd peer URL updated", "url", want)
				}
			}
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
		ml, err := cli.MemberList(ctx)
		if err != nil {
			continue
		}
		for _, m := range ml.Members {
			if m.IsLearner {
				if _, err := cli.MemberPromote(ctx, m.ID); err == nil {
					klog.InfoS("etcd learner promoted", "member", m.Name)
				}
			}
		}
	}
}
