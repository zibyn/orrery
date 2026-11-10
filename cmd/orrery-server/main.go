package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/zibyn/orrery/internal/api"
	"github.com/zibyn/orrery/internal/buildinfo"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	args := os.Args[1:]
	command := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command, args = args[0], args[1:]
	}
	switch command {
	case "serve":
		return serve(args, logger)
	case "user-token":
		return runUserToken(args, logger)
	case "openapi":
		return runOpenAPI(logger)
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		return fmt.Errorf("unknown command %q (try %s help)", command, os.Args[0])
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `usage: orrery-server [command]

commands:
  serve       run the Control Plane (default)
  user-token  mint an API Token for a machine User (--name) or a Polaris-linked User (--sub);
              --session mints a console Session instead (e2e, operator console access)
  openapi     print the generated OpenAPI document

environment:
  ORRERY_CONFIG_FILE             deployment config.yaml (defaults.md); when unset,
                                settings come from the ORRERY_* variables below
  ORRERY_API_ADDR                listen address (default :8080)
  ORRERY_DATABASE_URL            PostgreSQL DSN (environment mode only)
  ORRERY_DEFAULT_GPU_QUOTA       new User GPU quota (default 0)
  ORRERY_DEFAULT_SANDBOX_QUOTA   new User Sandbox quota (default 0)
  ORRERY_APPS_DOMAIN             wildcard domain for port forwarding (off when empty)
  ORRERY_PORT_TICKET_KEY         signs port tickets and Gateway cookies
  ORRERY_PLATFORM_SNAPSHOTS      name=ref@sha256:…,… imported at startup (platform images)
  ORRERY_DEFAULT_SNAPSHOT        platform Snapshot used when a create omits snapshot
  ORRERY_DEFAULT_GPU_SNAPSHOT    same, for a create with gpu.count > 0 (default ORRERY_DEFAULT_SNAPSHOT)
  ORRERY_REGISTRY_ADDR           Node-reachable registry address (e.g. 127.0.0.1:15000)
  ORRERY_REGISTRY_PUSH_ADDR      Control Plane push address (default ORRERY_REGISTRY_ADDR)
  ORRERY_REGISTRY_CREDENTIAL     Docker config.json for Nodes and pushes; list both hosts
  ORRERY_BUILD_CPU               Snapshot build CPU ceiling (default 4)
  ORRERY_BUILD_MEMORY            Snapshot build memory ceiling in bytes (default 8 GiB)
  ORRERY_BUILD_TIMEOUT           Snapshot build timeout (default 1h)
`)
}

func serve(args []string, logger *slog.Logger) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := api.LoadConfig(logger)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, err := api.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := api.Migrate(ctx, pool, cfg.DatabaseURL); err != nil {
		return err
	}
	logger.Info("migrations applied")
	server := api.New(cfg, pool, logger)
	logger.Info("listening", "addr", cfg.Addr, "version", buildinfo.Version())
	return server.Serve(ctx)
}

func runUserToken(args []string, logger *slog.Logger) error {
	fs := flag.NewFlagSet("user-token", flag.ContinueOnError)
	var opts api.UserTokenOptions
	fs.StringVar(&opts.Name, "name", "", "find or create the machine User with this name")
	fs.StringVar(&opts.Sub, "sub", "", "the Polaris subject of an existing linked User")
	fs.BoolVar(&opts.Admin, "admin", false, "with --name: the machine User is, or becomes, an admin")
	fs.StringVar(&opts.Scope, "scope", "user", "token scope: read_only, user, or admin")
	fs.StringVar(&opts.TokenName, "token-name", "", "token name (generated when omitted)")
	fs.BoolVar(&opts.Session, "session", false, "mint a Session (the orrery_session console cookie value) instead of an API Token: how e2e and an operator reach the console without a Polaris login")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Logs go to stderr so stdout carries only the token JSON.
	logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	cfg, err := api.LoadConfig(logger)
	if err != nil {
		return err
	}
	ctx := context.Background()
	pool, err := api.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := api.Migrate(ctx, pool, cfg.DatabaseURL); err != nil {
		return err
	}
	server := api.New(cfg, pool, logger)
	token, err := server.UserToken(ctx, opts)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(token)
}

func runOpenAPI(logger *slog.Logger) error {
	server := api.New(api.Config{}, nil, logger)
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(server.OpenAPIDocument())
}
