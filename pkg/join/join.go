// Package join implements k3s-style token joining. The token has the form
// K10<sha256(ca.crt) hex>::<secret>; an agent fetches the CA over TLS without
// verification, checks it against the hash, then requests credentials over a
// connection pinned to that CA.
package join

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/GrowlyX/kvz/pkg/pki"
)

type Request struct {
	Name string `json:"name"`
	IP   string `json:"ip"`
}

// ServerJoinRequest asks to join as an additional control-plane server.
type ServerJoinRequest struct {
	Name    string `json:"name"`
	IP      string `json:"ip"`
	PeerURL string `json:"peerURL"`
}

// ServerBundle carries the cluster PKI and the etcd initial-cluster value.
type ServerBundle struct {
	Files          map[string][]byte `json:"files"`
	InitialCluster string            `json:"initialCluster"`
}

// ServerJoiner is implemented by HA-capable control planes.
type ServerJoiner interface {
	AddMember(ctx context.Context, name, peerURL string) (string, error)
	BootstrapFiles() (map[string][]byte, error)
}

type Bundle struct {
	PodCIDR string `json:"podCIDR"`
	CA      []byte `json:"ca"`
	Cert    []byte `json:"cert"`
	Key     []byte `json:"key"`
}

func caHash(caPEM []byte) string {
	s := sha256.Sum256(caPEM)
	return hex.EncodeToString(s[:])
}

// LoadOrCreateToken returns the full join token for ca.
func LoadOrCreateToken(dataDir string, ca *pki.CA) (token, secret string, err error) {
	p := filepath.Join(dataDir, "token")
	b, err := os.ReadFile(p)
	if err != nil {
		r := make([]byte, 16)
		_, _ = rand.Read(r)
		b = []byte(hex.EncodeToString(r))
		if err := os.WriteFile(p, b, 0o600); err != nil {
			return "", "", err
		}
	}
	secret = strings.TrimSpace(string(b))
	caPEM, err := os.ReadFile(filepath.Join(ca.Dir, "ca.crt"))
	if err != nil {
		return "", "", err
	}
	return "K10" + caHash(caPEM) + "::" + secret, secret, nil
}

// Serve runs the join endpoint.
func Serve(addr, secret string, ca *pki.CA, servingCert, servingKey string, cidrs *CIDRAllocator, sj ServerJoiner) error {
	caPEM, err := os.ReadFile(filepath.Join(ca.Dir, "ca.crt"))
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/ca", func(w http.ResponseWriter, r *http.Request) { w.Write(caPEM) })
	mux.HandleFunc("/v1/join", func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(secret)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var req Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		ips := []net.IP{}
		if ip := net.ParseIP(req.IP); ip != nil {
			ips = append(ips, ip)
		}
		crt, key, err := ca.Issue("node-"+req.Name, "system:node:"+req.Name, []string{"system:nodes"}, []string{req.Name}, ips, true)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		cb, _ := os.ReadFile(crt)
		kb, _ := os.ReadFile(key)
		podCIDR, err := cidrs.For(req.Name)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		_ = json.NewEncoder(w).Encode(Bundle{CA: caPEM, Cert: cb, Key: kb, PodCIDR: podCIDR})
	})
	mux.HandleFunc("/v1/server-join", func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(secret)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if sj == nil {
			http.Error(w, "server join not supported", http.StatusNotImplemented)
			return
		}
		var req ServerJoinRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" || req.PeerURL == "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		files, err := sj.BootstrapFiles()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		ic, err := sj.AddMember(r.Context(), req.Name, req.PeerURL)
		if err != nil {
			http.Error(w, "etcd member add: "+err.Error(), http.StatusConflict)
			return
		}
		_ = json.NewEncoder(w).Encode(ServerBundle{Files: files, InitialCluster: ic})
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	return srv.ListenAndServeTLS(servingCert, servingKey)
}

// Fetch performs the agent side of the join.
func Fetch(joinURL, token string, req Request) (*Bundle, error) {
	var b Bundle
	return &b, pinnedPost(joinURL, token, "/v1/join", req, &b)
}

// FetchServer performs the server side of an HA join.
func FetchServer(joinURL, token string, req ServerJoinRequest) (*ServerBundle, error) {
	var b ServerBundle
	return &b, pinnedPost(joinURL, token, "/v1/server-join", req, &b)
}

// pinnedPost fetches the CA, checks it against the token's hash, then POSTs
// req over a connection pinned to that CA, authenticated by the secret.
func pinnedPost(joinURL, token, path string, req, out any) error {
	if !strings.HasPrefix(token, "K10") || !strings.Contains(token, "::") {
		return fmt.Errorf("malformed token")
	}
	parts := strings.SplitN(strings.TrimPrefix(token, "K10"), "::", 2)
	wantHash, secret := parts[0], parts[1]
	insecure := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	resp, err := insecure.Get(joinURL + "/v1/ca")
	if err != nil {
		return err
	}
	caPEM, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if caHash(caPEM) != wantHash {
		return fmt.Errorf("server CA does not match token hash")
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	pinned := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	body, _ := json.Marshal(req)
	hr, _ := http.NewRequest("POST", joinURL+path, bytes.NewReader(body))
	hr.Header.Set("Authorization", "Bearer "+secret)
	resp, err = pinned.Do(hr)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("join failed: %s: %s", resp.Status, b)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
