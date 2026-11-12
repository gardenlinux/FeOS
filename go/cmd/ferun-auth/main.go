// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Command ferun-auth is the access provider of the kinds ferun and oidc
// (ADR 0054): it signs a guarded environment's visitors in at the
// installation's issuer or at a tenant's, and answers their challenges at
// ferund.
//
//	ferun-auth [serve] [flags]   run it (the default)
//	ferun-auth version           print the version
//	ferun-auth help              print the flags
//
// Every flag has an environment variable FERUN_AUTH_<NAME>
// (internal/ferunauth). SIGTERM and SIGINT stop it after
// --shutdown-delay; a second signal ends it at once.
package main

import (
	"context"
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

	"github.com/gardenlinux/feos/go/internal/ferunauth"
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
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "serve":
	case "version":
		fmt.Fprintln(stdout, buildVersion())
		return 0
	case "help":
		ferunauth.Usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "ferun-auth: unknown command %q\n\n", cmd)
		ferunauth.Usage(stderr)
		return 2
	}
	cfg, err := ferunauth.Parse(args, getenv)
	if errors.Is(err, flag.ErrHelp) {
		ferunauth.Usage(stdout)
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "ferun-auth: %v\n", err)
		return 2
	}
	log := ferunauth.NewLogger(stderr, cfg.LogFormat, cfg.LogLevel).With("version", buildVersion())
	slog.SetDefault(log)
	srv, err := ferunauth.New(cfg, log)
	if err != nil {
		fmt.Fprintf(stderr, "ferun-auth: %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go func() {
		// A second signal ends the process at once.
		<-ctx.Done()
		stop()
	}()
	if err := srv.Run(ctx); err != nil {
		fmt.Fprintf(stderr, "ferun-auth: %v\n", err)
		return 1
	}
	return 0
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
