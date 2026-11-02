//go:build darwin

package microvmrt

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// probe/handler are the wire forms consumed by kvz-init (named ports resolved).
type probe struct {
	Exec        []string          `json:"exec,omitempty"`
	HTTPPath    string            `json:"http_path,omitempty"`
	HTTPPort    int               `json:"http_port,omitempty"`
	HTTPScheme  string            `json:"http_scheme,omitempty"`
	HTTPHost    string            `json:"http_host,omitempty"`
	HTTPHeaders map[string]string `json:"http_headers,omitempty"`
	TCPPort     int               `json:"tcp_port,omitempty"`
	Initial     int               `json:"initial,omitempty"`
	Period      int               `json:"period,omitempty"`
	Timeout     int               `json:"timeout,omitempty"`
	Success     int               `json:"success,omitempty"`
	Failure     int               `json:"failure,omitempty"`
}

type handler struct {
	Exec     []string `json:"exec,omitempty"`
	HTTPPath string   `json:"http_path,omitempty"`
	HTTPPort int      `json:"http_port,omitempty"`
	HTTPHost string   `json:"http_host,omitempty"`
	TCPPort  int      `json:"tcp_port,omitempty"`
	Sleep    int      `json:"sleep,omitempty"`
}

func port(c corev1.Container, p intstr.IntOrString) int {
	if p.Type == intstr.Int {
		return p.IntValue()
	}
	for _, cp := range c.Ports {
		if cp.Name == p.StrVal {
			return int(cp.ContainerPort)
		}
	}
	return 0
}

func orDefault(v, d int32) int {
	if v <= 0 {
		return int(d)
	}
	return int(v)
}

func convProbe(c corev1.Container, p *corev1.Probe) *probe {
	if p == nil {
		return nil
	}
	out := &probe{Initial: int(p.InitialDelaySeconds), Period: orDefault(p.PeriodSeconds, 10), Timeout: orDefault(p.TimeoutSeconds, 1),
		Success: orDefault(p.SuccessThreshold, 1), Failure: orDefault(p.FailureThreshold, 3)}
	switch {
	case p.Exec != nil:
		out.Exec = p.Exec.Command
	case p.HTTPGet != nil:
		out.HTTPPath, out.HTTPPort, out.HTTPScheme, out.HTTPHost = p.HTTPGet.Path, port(c, p.HTTPGet.Port), string(p.HTTPGet.Scheme), p.HTTPGet.Host
		if out.HTTPPath == "" {
			out.HTTPPath = "/"
		}
		if len(p.HTTPGet.HTTPHeaders) > 0 {
			out.HTTPHeaders = map[string]string{}
			for _, h := range p.HTTPGet.HTTPHeaders {
				out.HTTPHeaders[h.Name] = h.Value
			}
		}
	case p.TCPSocket != nil:
		out.TCPPort = port(c, p.TCPSocket.Port)
	case p.GRPC != nil:
		out.TCPPort = int(p.GRPC.Port) // approximation: connectivity check
	}
	return out
}

func convHandler(c corev1.Container, h *corev1.LifecycleHandler) *handler {
	if h == nil {
		return nil
	}
	out := &handler{}
	switch {
	case h.Exec != nil:
		out.Exec = h.Exec.Command
	case h.HTTPGet != nil:
		out.HTTPPath, out.HTTPPort, out.HTTPHost = h.HTTPGet.Path, port(c, h.HTTPGet.Port), h.HTTPGet.Host
	case h.TCPSocket != nil:
		out.TCPPort = port(c, h.TCPSocket.Port)
	case h.Sleep != nil:
		out.Sleep = int(h.Sleep.Seconds)
	default:
		return nil
	}
	return out
}
