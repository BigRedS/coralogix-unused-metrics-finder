package metricusage

import "testing"

// The REST API host must never be dialled for gRPC, whichever host a caller passes in.
func TestNewClientDialsGRPCHost(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"api.coralogix.com", "ng-api-grpc.coralogix.com"},
		{"api.eu2.coralogix.com", "ng-api-grpc.eu2.coralogix.com"},
		{"ng-api-grpc.coralogix.com", "ng-api-grpc.coralogix.com"},
		{"grpc.internal.example.com", "grpc.internal.example.com"},
	} {
		c, err := NewClient(tc.in, "k")
		if err != nil {
			t.Fatalf("NewClient(%q): %v", tc.in, err)
		}
		if c.GRPCHost != tc.want {
			t.Errorf("NewClient(%q) dialled %q, want %q", tc.in, c.GRPCHost, tc.want)
		}
		c.Close()
	}
}
