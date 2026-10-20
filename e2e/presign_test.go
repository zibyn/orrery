//go:build e2e

package e2e

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
)

// TestPresignAuthorization is #84's acceptance scenario for the Presign RPC: a
// Node Token is required, malformed requests are InvalidArgument, and an
// object outside this Node's authorized set is InvalidArgument for the whole
// batch (node-protocol.md). The reference tables arrive in #86/#87, so every
// object is unauthorized for now.
func TestPresignAuthorization(t *testing.T) {
	admin := adminToken(t)
	name := fmt.Sprintf("e2e-presign-node-%d", time.Now().UnixNano())
	state, _ := enrollAgent(t, admin, name)

	raw, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	var identity struct {
		NodeToken string `json:"node_token"`
	}
	if err := json.Unmarshal(raw, &identity); err != nil || identity.NodeToken == "" {
		t.Fatalf("identity has no Node Token: %v %s", err, raw)
	}

	client, closeClient := nodeClient(t)
	defer closeClient()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+identity.NodeToken)

	const digest = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	cases := []struct {
		name string
		req  *nodev1.PresignRequest
		want codes.Code
	}{
		{"unknown method", &nodev1.PresignRequest{Sha256: []string{digest}, Method: "DELETE"}, codes.InvalidArgument},
		{"no objects", &nodev1.PresignRequest{Method: "GET"}, codes.InvalidArgument},
		{"both kinds", &nodev1.PresignRequest{Sha256: []string{digest}, Manifests: []string{"m"}, Method: "GET"}, codes.InvalidArgument},
		{"manifest for PUT", &nodev1.PresignRequest{Manifests: []string{"m"}, Method: "PUT"}, codes.InvalidArgument},
		{"unauthorized object GET", &nodev1.PresignRequest{Sha256: []string{digest}, Method: "GET"}, codes.InvalidArgument},
		{"unauthorized manifest GET", &nodev1.PresignRequest{Manifests: []string{"sha256:abc"}, Method: "GET"}, codes.InvalidArgument},
		{"unauthorized object PUT", &nodev1.PresignRequest{Sha256: []string{digest}, Method: "PUT"}, codes.InvalidArgument},
	}
	for _, tc := range cases {
		_, err := client.Presign(ctx, tc.req)
		if got := status.Code(err); got != tc.want {
			t.Fatalf("%s: code = %v, want %v (%v)", tc.name, got, tc.want, err)
		}
	}

	if _, err := client.Presign(context.Background(), &nodev1.PresignRequest{Sha256: []string{digest}, Method: "GET"}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("Presign without a Node Token: %v, want Unauthenticated", err)
	}
}

func nodeClient(t *testing.T) (nodev1.NodeClient, func()) {
	t.Helper()
	ca, err := os.ReadFile(nodeCA())
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		t.Fatalf("%s contains no certificates", nodeCA())
	}
	conn, err := grpc.NewClient(nodeURL(), grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		RootCAs:    pool,
		MinVersion: tls.VersionTLS12,
	})))
	if err != nil {
		t.Fatal(err)
	}
	return nodev1.NewNodeClient(conn), func() { conn.Close() }
}
