// Command orrery-polaris-migrate links an existing deployment's Users to
// Polaris before Orrery switches to it (ADR-0029). It is one-off and safe to
// rerun; see docs/polaris-migration.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

func main() {
	if err := mainErr(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func mainErr() error {
	dsn := flag.String("database-url", os.Getenv("ORRERY_DATABASE_URL"), "Orrery PostgreSQL DSN (env ORRERY_DATABASE_URL)")
	issuer := flag.String("issuer", os.Getenv("POLARIS_ISSUER"), "Polaris issuer URL, exactly as oidc.issuer_url will be (env POLARIS_ISSUER)")
	clientID := flag.String("client-id", os.Getenv("POLARIS_CLIENT_ID"), "M2M Application client id (env POLARIS_CLIENT_ID)")
	flag.Parse()
	secret := os.Getenv("POLARIS_CLIENT_SECRET")
	if *dsn == "" || *issuer == "" || *clientID == "" || secret == "" {
		return errors.New("need --database-url, --issuer, --client-id and POLARIS_CLIENT_SECRET")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, *dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	cc := clientcredentials.Config{ClientID: *clientID, ClientSecret: secret, TokenURL: *issuer + "/token", AuthStyle: oauth2.AuthStyleInHeader}
	r, err := run(ctx, pool, cc.Client(ctx), *issuer)
	if err != nil {
		return err
	}
	r.write(os.Stdout)
	if r.failed() {
		return errors.New("some Users are not linked: fix them and rerun before switching Orrery to Polaris")
	}
	return nil
}
