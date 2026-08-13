package region

import "testing"

// Every region's gRPC host was verified to negotiate ALPN h2, while api.<domain> does not on
// eu1, us1, ap1 and ap2 — grpc-go rejects those connections at the handshake. If this mapping
// regresses, billing and team-name lookup silently stop working on half the regions.
func TestGRPCHostForEveryRegion(t *testing.T) {
	want := map[string]string{
		"eu1": "ng-api-grpc.coralogix.com",
		"us1": "ng-api-grpc.coralogix.us",
		"us2": "ng-api-grpc.cx498.coralogix.com",
		"us3": "ng-api-grpc.us3.coralogix.com",
		"eu2": "ng-api-grpc.eu2.coralogix.com",
		"ap1": "ng-api-grpc.coralogix.in",
		"ap2": "ng-api-grpc.coralogixsg.com",
		"ap3": "ng-api-grpc.ap3.coralogix.com",
	}

	if len(want) != len(regionAPIHosts) {
		t.Fatalf("expectations cover %d regions but regionAPIHosts has %d — add the new region here", len(want), len(regionAPIHosts))
	}

	for code, wantHost := range want {
		apiHost, err := ResolveAPIHost(code)
		if err != nil {
			t.Errorf("ResolveAPIHost(%q): %v", code, err)
			continue
		}
		if got := GRPCHost(apiHost); got != wantHost {
			t.Errorf("GRPCHost(%q) = %q, want %q", apiHost, got, wantHost)
		}
	}
}

func TestGRPCHostIsIdempotent(t *testing.T) {
	once := GRPCHost("api.eu2.coralogix.com")
	if twice := GRPCHost(once); twice != once {
		t.Errorf("GRPCHost is not idempotent: %q -> %q", once, twice)
	}
}

func TestGRPCHostPassesThroughNonAPIHosts(t *testing.T) {
	// An explicit --grpc-host must reach the dialler untouched.
	for _, host := range []string{
		"ng-api-grpc.coralogix.com",
		"grpc.internal.example.com",
		"localhost",
	} {
		if got := GRPCHost(host); got != host {
			t.Errorf("GRPCHost(%q) = %q, want it unchanged", host, got)
		}
	}
}

func TestGRPCHostNormalisesCaseAndSpace(t *testing.T) {
	if got, want := GRPCHost("  API.EU2.Coralogix.com "), "ng-api-grpc.eu2.coralogix.com"; got != want {
		t.Errorf("GRPCHost = %q, want %q", got, want)
	}
}
