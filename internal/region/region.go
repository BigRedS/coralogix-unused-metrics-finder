package region

import (
	"fmt"
	"sort"
	"strings"
)

// Short region code -> API hostname (no scheme).
var regionAPIHosts = map[string]string{
	"eu1": "api.coralogix.com",
	"us1": "api.coralogix.us",
	"us2": "api.cx498.coralogix.com",
	"us3": "api.us3.coralogix.com",
	"eu2": "api.eu2.coralogix.com",
	"ap1": "api.coralogix.in",
	"ap2": "api.coralogixsg.com",
	"ap3": "api.ap3.coralogix.com",
}

// RegionChoice is a short region code and its metrics/management API hostname (no scheme).
type RegionChoice struct {
	Code string `json:"code"`
	Host string `json:"host"`
}

// SortedRegionChoices returns stable UI dropdown entries (code ascending).
func SortedRegionChoices() []RegionChoice {
	out := make([]RegionChoice, 0, len(regionAPIHosts))
	for code, host := range regionAPIHosts {
		out = append(out, RegionChoice{Code: code, Host: host})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}

// Team login domain -> API hostname.
var loginDomainToAPIHost = map[string]string{
	"eu1.coralogix.com": "api.coralogix.com",
	"us1.coralogix.com": "api.coralogix.us",
	"us2.coralogix.com": "api.cx498.coralogix.com",
	"us3.coralogix.com": "api.us3.coralogix.com",
	"eu2.coralogix.com": "api.eu2.coralogix.com",
	"ap1.coralogix.com": "api.coralogix.in",
	"ap2.coralogix.com": "api.coralogixsg.com",
	"ap3.coralogix.com": "api.ap3.coralogix.com",
}

// ResolveAPIHost maps a region code, login domain, or API hostname to the management/metrics API host.
func ResolveAPIHost(regionOrDomain string) (string, error) {
	key := strings.ToLower(strings.TrimSpace(regionOrDomain))
	if h, ok := regionAPIHosts[key]; ok {
		return h, nil
	}
	if h, ok := loginDomainToAPIHost[key]; ok {
		return h, nil
	}
	for _, h := range regionAPIHosts {
		if key == h {
			return h, nil
		}
	}
	if strings.HasPrefix(key, "api.") && isHostname(key) {
		return key, nil
	}
	codes := make([]string, 0, len(regionAPIHosts))
	for c := range regionAPIHosts {
		codes = append(codes, c)
	}
	return "", fmt.Errorf(
		"unknown region or domain %q: expected one of %v, a login domain (e.g. eu2.coralogix.com), or an API host (e.g. api.eu2.coralogix.com)",
		regionOrDomain, codes,
	)
}

func isHostname(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '.' || c == '-' {
			continue
		}
		return false
	}
	return true
}

// grpcHostLabel is the first label of Coralogix's dedicated gRPC endpoint.
//
// The REST host (api.<domain>) is not a gRPC endpoint. On the legacy-domain clusters — eu1
// (api.coralogix.com), us1 (api.coralogix.us), ap1 (api.coralogix.in) and ap2
// (api.coralogixsg.com) — it does not negotiate ALPN at all, and grpc-go rejects such
// connections outright ("credentials: cannot check peer: missing selected ALPN property"), so
// every gRPC call fails at the TLS handshake. ng-api-grpc.<domain> negotiates h2 on all
// regions, including the four where api.<domain> happens to work.
const grpcHostLabel = "ng-api-grpc."

// GRPCHost maps a management/metrics API host to the gRPC host serving the same account
// (api.eu2.coralogix.com -> ng-api-grpc.eu2.coralogix.com).
//
// A host that is not "api.<domain>" is returned unchanged: that covers an explicit gRPC host
// (--grpc-host) and an already-mapped one, so applying GRPCHost twice is harmless.
func GRPCHost(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	if rest, ok := strings.CutPrefix(h, "api."); ok {
		return grpcHostLabel + rest
	}
	return h
}

func MgmtOpenAPIV5Base(apiHost string) string {
	return "https://" + apiHost + "/mgmt/openapi/5"
}

func MetricsBase(apiHost string) string {
	return "https://" + apiHost + "/metrics"
}
