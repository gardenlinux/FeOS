// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Command ferund is FeRun, the control plane of FeOS (ADR 0015): every
// instance serves the REST API, MCP, the management UI and the ports to
// clients, and the nodes' port to feosd, on one shared PostgreSQL.
//
//	ferund [serve] [flags]          run an instance (the default)
//	ferund init-ca --root-dir R --dir D --dns-domain X ...
//	ferund new-issuer --root-dir R --dir D --dns-domain X ...
//	ferund join-token --database-url URL [--kind node|ingress] [--ttl 1h] [--node NAME]
//	ferund service-token --database-url URL --kind sink|auth --name NAME
//	ferund migrate --database-url URL
//
// Every flag has an environment variable FERUN_<NAME> (internal/config).
// SIGTERM and SIGINT stop an instance as §7 of the design says: requests,
// tool calls and ops in flight finish within --shutdown-grace.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/gardenlinux/feos/go/internal/config"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/fleet"
	ingresssvc "github.com/gardenlinux/feos/go/internal/ingress/service"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/pki"
	"github.com/gardenlinux/feos/go/internal/server"
	"github.com/gardenlinux/feos/go/internal/store"
)

// version is the build's version, set with -ldflags "-X main.version=…";
// otherwise the module's version from the build info, or "dev".
var version = ""

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// run runs a subcommand and returns the exit code: 0, 1 for a failure, 2
// for a usage error.
func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	cmd := config.CmdServe
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	} else if len(args) > 0 && (args[0] == "-h" || args[0] == "-help" || args[0] == "--help") {
		config.Usage(stdout, "")
		return 0
	}
	var err error
	switch cmd {
	case config.CmdServe:
		err = serve(args, getenv, stderr)
	case config.CmdInitCA:
		err = initCA(args, getenv, stdout)
	case config.CmdNewIssuer:
		err = newIssuer(args, getenv, stdout)
	case config.CmdJoinToken:
		err = joinToken(args, getenv, stdout, stderr)
	case config.CmdServiceToken:
		err = serviceToken(args, getenv, stdout, stderr)
	case config.CmdMigrate:
		err = migrate(args, getenv, stderr)
	case "help":
		topic := ""
		if len(args) > 0 {
			topic = args[0]
		}
		config.Usage(stdout, topic)
		return 0
	case "version":
		fmt.Fprintln(stdout, buildVersion())
		return 0
	default:
		fmt.Fprintf(stderr, "ferund: unknown command %q\n\n", cmd)
		config.Usage(stderr, "")
		return 2
	}
	if errors.Is(err, flag.ErrHelp) {
		config.Usage(stdout, cmd)
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "ferund %s: %v\n", cmd, err)
		var u usageError
		if errors.As(err, &u) {
			return 2
		}
		return 1
	}
	return 0
}

// usageError marks an error in the flags.
type usageError struct{ error }

func (u usageError) Unwrap() error { return u.error }

func flagErr(err error) error {
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return err
	}
	return usageError{err}
}

// buildVersion is the version to report.
func buildVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

func newLogger(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}))
}

// serve runs an instance until SIGTERM or SIGINT, then stops it.
func serve(args []string, getenv func(string) string, stderr io.Writer) error {
	cfg, err := config.Parse(args, getenv)
	if err != nil {
		return flagErr(err)
	}
	cfg.Version = buildVersion()
	log := newLogger(stderr, cfg.LogLevel)
	slog.SetDefault(log)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go func() {
		// A second signal ends the process at once.
		<-ctx.Done()
		stop()
	}()
	inst, err := server.New(ctx, cfg, log)
	if err != nil {
		return err
	}
	return inst.Run(ctx)
}

// initCA makes the platform CA.
func initCA(args []string, getenv func(string) string, stdout io.Writer) error {
	c, err := config.ParseInitCA(args, getenv)
	if err != nil {
		return flagErr(err)
	}
	if err := pki.InitCA(pki.InitCAOptions{RootDir: c.RootDir, Dir: c.Dir, TrustDomain: c.TrustDomain,
		ControlPlaneDomain: c.ControlPlaneDomain, DNSDomains: c.DNSDomains}); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "made the root in %s (keep its key offline) and the issuer in %s (an instance's --ca-dir)\n", c.RootDir, c.Dir)
	return nil
}

// newIssuer makes a new issuer under the platform CA's root.
func newIssuer(args []string, getenv func(string) string, stdout io.Writer) error {
	c, err := config.ParseNewIssuer(args, getenv)
	if err != nil {
		return flagErr(err)
	}
	if err := pki.NewIssuer(pki.InitCAOptions{RootDir: c.RootDir, Dir: c.Dir, TrustDomain: c.TrustDomain,
		ControlPlaneDomain: c.ControlPlaneDomain, DNSDomains: c.DNSDomains}); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "made a new issuer under the root in %s, in %s (an instance's --ca-dir)\n", c.RootDir, c.Dir)
	return nil
}

// joinToken issues a join token in the database and prints it: for a
// node through fleet, for an instance of ferun-ingress (--kind ingress)
// through the ingress service.
func joinToken(args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	c, err := config.ParseJoinToken(args, getenv)
	if err != nil {
		return flagErr(err)
	}
	log := newLogger(stderr, c.LogLevel)
	iss, err := pki.LoadIssuer(c.CADir, c.TrustDomain)
	if err != nil {
		return fmt.Errorf("load the issuer from %s: %w", c.CADir, err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	d, err := db.Open(ctx, c.DatabaseURL, log)
	if err != nil {
		return fmt.Errorf("open the database: %w", err)
	}
	defer d.Close()
	issue := fleet.New(fleet.Config{TrustDomain: c.TrustDomain, Log: log}, fleet.Deps{DB: d, Issuer: iss}).IssueJoinToken
	if c.Kind == model.JoinKindIngress {
		issue = ingresssvc.New(ingresssvc.Config{TrustDomain: c.TrustDomain, Log: log},
			ingresssvc.Deps{DB: d, Issuer: iss}).IssueJoinToken
	}
	id, token, expires, err := issue(ctx, "ferund join-token", c.Node, c.TTL)
	if err != nil {
		return err
	}
	pinned := "any new name"
	if c.Node != "" {
		pinned = c.Kind + " " + c.Node
	}
	fmt.Fprintf(stderr, "%s join token %s for %s, valid until %s\n", c.Kind, id, pinned,
		expires.UTC().Format("2006-01-02 15:04:05 MST"))
	fmt.Fprintln(stdout, token)
	return nil
}

// serviceToken issues a service token (ADR 0026) in the database and
// prints it once; the database keeps its hash, and an activity row says
// who made it.
func serviceToken(args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	c, err := config.ParseServiceToken(args, getenv)
	if err != nil {
		return flagErr(err)
	}
	log := newLogger(stderr, c.LogLevel)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	d, err := db.Open(ctx, c.DatabaseURL, log)
	if err != nil {
		return fmt.Errorf("open the database: %w", err)
	}
	defer d.Close()
	token, hash, err := pki.NewServiceToken(c.Kind)
	if err != nil {
		return fmt.Errorf("make a service token: %w", err)
	}
	const by = "ferund service-token"
	var t store.ServiceToken
	err = store.New(d).InTx(ctx, func(tx *store.Store) error {
		var err error
		if t, err = tx.CreateServiceToken(ctx, hash, c.Kind, c.Name, by); err != nil {
			return err
		}
		detail, _ := json.Marshal(map[string]any{"kind": c.Kind, "name": c.Name, "id": t.ID.String(), "by": by})
		return tx.InsertActivity(ctx, store.ActivityRow{TsNs: time.Now().UnixNano(), Source: store.SourceFerund,
			Kind: "service_token_created", Category: "credential",
			Name: fmt.Sprintf("%s token %s created by %s", c.Kind, c.Name, by), Detail: detail})
	})
	if err != nil {
		return fmt.Errorf("store the service token: %w", err)
	}
	fmt.Fprintf(stderr, "%s service token %s for %s\n", c.Kind, t.ID, c.Name)
	fmt.Fprintln(stdout, token)
	return nil
}

// migrate applies the migrations.
func migrate(args []string, getenv func(string) string, stderr io.Writer) error {
	c, err := config.ParseMigrate(args, getenv)
	if err != nil {
		return flagErr(err)
	}
	log := newLogger(stderr, c.LogLevel)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	d, err := db.Open(ctx, c.DatabaseURL, log)
	if err != nil {
		return fmt.Errorf("open the database: %w", err)
	}
	defer d.Close()
	if err := d.Migrate(ctx, nil); err != nil {
		return err
	}
	log.Info("the database is migrated")
	return nil
}
