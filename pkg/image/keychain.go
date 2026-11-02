package image

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
)

// SecretKeychain resolves registry credentials from Kubernetes
// imagePullSecrets (.dockerconfigjson / .dockercfg payloads).
type SecretKeychain struct {
	auths map[string]authn.AuthConfig
}

type dockerAuth struct {
	Auth     string `json:"auth"`
	Username string `json:"username"`
	Password string `json:"password"`
}

func normRegistry(r string) string {
	r = strings.TrimPrefix(strings.TrimPrefix(r, "https://"), "http://")
	r = strings.TrimSuffix(strings.SplitN(r, "/", 2)[0], "/")
	switch r {
	case "docker.io", "registry-1.docker.io", "index.docker.io":
		return "index.docker.io"
	}
	return r
}

// NewSecretKeychain parses secret payloads (data keys .dockerconfigjson or .dockercfg).
func NewSecretKeychain(payloads []map[string][]byte) *SecretKeychain {
	k := &SecretKeychain{auths: map[string]authn.AuthConfig{}}
	for _, data := range payloads {
		var auths map[string]dockerAuth
		if b, ok := data[".dockerconfigjson"]; ok {
			var cfg struct {
				Auths map[string]dockerAuth `json:"auths"`
			}
			if json.Unmarshal(b, &cfg) == nil {
				auths = cfg.Auths
			}
		} else if b, ok := data[".dockercfg"]; ok {
			_ = json.Unmarshal(b, &auths)
		}
		for reg, a := range auths {
			ac := authn.AuthConfig{Username: a.Username, Password: a.Password}
			if a.Auth != "" && ac.Username == "" {
				if dec, err := base64.StdEncoding.DecodeString(a.Auth); err == nil {
					if u, p, ok := strings.Cut(string(dec), ":"); ok {
						ac.Username, ac.Password = u, p
					}
				}
			}
			k.auths[normRegistry(reg)] = ac
		}
	}
	return k
}

func (k *SecretKeychain) Resolve(r authn.Resource) (authn.Authenticator, error) {
	if ac, ok := k.auths[normRegistry(r.RegistryStr())]; ok {
		return authn.FromConfig(ac), nil
	}
	return authn.Anonymous, nil
}

// Keychain combines pod pull secrets with the host's docker config.
func Keychain(payloads []map[string][]byte) authn.Keychain {
	if len(payloads) == 0 {
		return authn.DefaultKeychain
	}
	return authn.NewMultiKeychain(NewSecretKeychain(payloads), authn.DefaultKeychain)
}
