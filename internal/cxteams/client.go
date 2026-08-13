// Package cxteams calls the Coralogix TeamService.ListTeams gRPC method to discover
// the human-readable team name attached to an API key. It's best-effort: callers
// should treat a failure (PermissionDenied, network error, …) as "no team name
// available" and proceed without it. The service lives on the same regional
// api.<region>.coralogix.com:443 host as the other Coralogix gRPC APIs.
package cxteams

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	cxteamspb "github.com/BigRedS/coralogix-unused-metrics-finder/internal/gen/cxteams"
	"github.com/BigRedS/coralogix-unused-metrics-finder/internal/region"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type Client struct {
	svc  cxteamspb.TeamServiceClient
	conn *grpc.ClientConn
}

// NewClient dials the Coralogix gRPC endpoint for host. Pass either the account's API host
// (api.eu2.coralogix.com) or an explicit gRPC host; region.GRPCHost normalises both. The REST
// API host is not a gRPC endpoint — see region.GRPCHost for why.
func NewClient(host, apiKey string) (*Client, error) {
	target := region.GRPCHost(host) + ":443"
	conn, err := grpc.NewClient(
		target,
		grpc.WithTransportCredentials(credentials.NewClientTLSFromCert(nil, "")),
		grpc.WithPerRPCCredentials(bearerAuth{token: apiKey}),
	)
	if err != nil {
		return nil, fmt.Errorf("grpc dial %s: %w", target, err)
	}
	return &Client{
		svc:  cxteamspb.NewTeamServiceClient(conn),
		conn: conn,
	}, nil
}

func (c *Client) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

type bearerAuth struct{ token string }

func (b bearerAuth) GetRequestMetadata(_ context.Context, _ ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + b.token}, nil
}

func (b bearerAuth) RequireTransportSecurity() bool { return true }

// FetchTeamName returns the human-readable team name for the API key. If the API
// key lacks team-admin scope, the call will fail with PermissionDenied — callers
// should treat any error as "team name unavailable" and not abort their work.
// The default_team field is preferred (when ListTeams returns multiple teams for
// an org-level key, default_team is the team the API key is scoped to).
func (c *Client) FetchTeamName(ctx context.Context) (string, error) {
	resp, err := c.svc.ListTeams(ctx, &cxteamspb.ListTeamsRequest{})
	if err != nil {
		return "", err
	}
	if dt := resp.GetDefaultTeam(); dt != nil && dt.GetTeamName() != "" {
		return dt.GetTeamName(), nil
	}
	for _, t := range resp.GetTeams() {
		if t.GetTeamName() != "" {
			return t.GetTeamName(), nil
		}
	}
	return "", fmt.Errorf("ListTeams returned no team name")
}

// SanitizeForFilename converts an arbitrary team name into a string safe to use
// as a filename prefix: ASCII alphanumerics and ".-_" pass through, anything else
// (including whitespace) collapses to a single underscore, and leading/trailing
// underscores are trimmed.
func SanitizeForFilename(s string) string {
	cleaned := unsafeChars.ReplaceAllString(s, "_")
	cleaned = collapseUnderscores.ReplaceAllString(cleaned, "_")
	return strings.Trim(cleaned, "_")
}

var (
	unsafeChars         = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
	collapseUnderscores = regexp.MustCompile(`_+`)
)
