package image

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"sigs.k8s.io/yaml"
)

// Registries is the k3s-compatible registries.yaml: per-registry mirror
// endpoints (tried in order, then the upstream registry) and per-host auth/TLS.
type Registries struct {
	Mirrors map[string]Mirror         `json:"mirrors"`
	Configs map[string]RegistryConfig `json:"configs"`
}

type Mirror struct {
	Endpoint []string          `json:"endpoint"`
	Rewrite  map[string]string `json:"rewrite"` // repository prefix rewrites, e.g. "^library/(.*)": "mirror/$1" (prefix form only)
}

type RegistryConfig struct {
	Auth *struct {
		Username      string `json:"username"`
		Password      string `json:"password"`
		Auth          string `json:"auth"`
		IdentityToken string `json:"identity_token"`
	} `json:"auth"`
	TLS *struct {
		InsecureSkipVerify bool   `json:"insecure_skip_verify"`
		CAFile             string `json:"ca_file"`
		CertFile           string `json:"cert_file"`
		KeyFile            string `json:"key_file"`
	} `json:"tls"`
}

// LoadRegistries reads a registries.yaml (a missing file is not an error).
func (s *Store) LoadRegistries(path string) error {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var r Registries
	if err := yaml.Unmarshal(b, &r); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	s.reg = &r
	return nil
}

var platform = remote.WithPlatform(v1.Platform{OS: "linux", Architecture: "arm64"})

type candidate struct {
	ref  name.Reference
	opts []remote.Option
}

func normHost(h string) string {
	if h == "index.docker.io" || h == "registry-1.docker.io" {
		return "docker.io"
	}
	return h
}

// candidates lists where to fetch ref from: configured mirrors first, then
// the registry itself.
func (s *Store) candidates(ref string, kc authn.Keychain) ([]candidate, error) {
	r, err := name.ParseReference(ref)
	if err != nil {
		return nil, err
	}
	if s.reg == nil {
		return []candidate{{r, []remote.Option{remote.WithAuthFromKeychain(kc), platform}}}, nil
	}
	host := normHost(r.Context().RegistryStr())
	var out []candidate
	m, ok := s.reg.Mirrors[host]
	if !ok {
		m, ok = s.reg.Mirrors["*"]
	}
	if ok {
		repo := r.Context().RepositoryStr()
		for from, to := range m.Rewrite {
			if p := strings.TrimPrefix(from, "^"); strings.HasPrefix(repo, strings.TrimSuffix(p, "(.*)")) {
				repo = strings.Replace(to, "$1", strings.TrimPrefix(repo, strings.TrimSuffix(p, "(.*)")), 1)
			}
		}
		for _, ep := range m.Endpoint {
			u, err := url.Parse(ep)
			if err != nil || u.Host == "" {
				continue
			}
			var nopts []name.Option
			if u.Scheme == "http" {
				nopts = append(nopts, name.Insecure)
			}
			base := u.Host + strings.TrimSuffix(u.Path, "/")
			base = strings.TrimSuffix(base, "/v2")
			var mr name.Reference
			if d, isDigest := r.(name.Digest); isDigest {
				mr, err = name.NewDigest(base+"/"+repo+"@"+d.DigestStr(), nopts...)
			} else {
				mr, err = name.NewTag(base+"/"+repo+":"+r.Identifier(), nopts...)
			}
			if err != nil {
				continue
			}
			out = append(out, candidate{mr, s.hostOpts(u.Host, kc)})
		}
	}
	out = append(out, candidate{r, s.hostOpts(host, kc)})
	return out, nil
}

func (s *Store) hostOpts(host string, kc authn.Keychain) []remote.Option {
	opts := []remote.Option{platform}
	cfg, ok := s.reg.Configs[host]
	if !ok && host == "docker.io" {
		cfg, ok = s.reg.Configs["registry-1.docker.io"]
	}
	if ok && cfg.Auth != nil {
		opts = append(opts, remote.WithAuth(authn.FromConfig(authn.AuthConfig{
			Username: cfg.Auth.Username, Password: cfg.Auth.Password, Auth: cfg.Auth.Auth, IdentityToken: cfg.Auth.IdentityToken})))
	} else {
		opts = append(opts, remote.WithAuthFromKeychain(kc))
	}
	if ok && cfg.TLS != nil {
		tc := &tls.Config{InsecureSkipVerify: cfg.TLS.InsecureSkipVerify}
		if cfg.TLS.CAFile != "" {
			if pem, err := os.ReadFile(cfg.TLS.CAFile); err == nil {
				pool, _ := x509.SystemCertPool()
				if pool == nil {
					pool = x509.NewCertPool()
				}
				pool.AppendCertsFromPEM(pem)
				tc.RootCAs = pool
			}
		}
		if cfg.TLS.CertFile != "" && cfg.TLS.KeyFile != "" {
			if c, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile); err == nil {
				tc.Certificates = []tls.Certificate{c}
			}
		}
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.TLSClientConfig = tc
		opts = append(opts, remote.WithTransport(tr))
	}
	return opts
}

// head and image try each candidate in order.
func (s *Store) head(ref string, kc authn.Keychain) (*v1.Descriptor, error) {
	cs, err := s.candidates(ref, kc)
	if err != nil {
		return nil, err
	}
	var last error
	for _, c := range cs {
		d, err := remote.Head(c.ref, c.opts...)
		if err == nil {
			return d, nil
		}
		last = err
	}
	return nil, last
}

func (s *Store) image(ref string, kc authn.Keychain) (v1.Image, error) {
	cs, err := s.candidates(ref, kc)
	if err != nil {
		return nil, err
	}
	var last error
	for _, c := range cs {
		img, err := remote.Image(c.ref, c.opts...)
		if err == nil {
			return img, nil
		}
		last = err
	}
	return nil, last
}
