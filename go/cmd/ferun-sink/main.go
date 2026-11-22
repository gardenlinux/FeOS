// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Command ferun-sink is the platform's log store (ADR 0026): it takes the
// nodes' OTLP logs over mTLS, keeps them in its PostgreSQL and then as
// chunks in an object store, and answers projects' reads over HTTP.
//
//	ferun-sink [serve] [flags]   run the sink (the default)
//	ferun-sink version           print the version
//
// Every flag has an environment variable FERUN_SINK_<NAME>
// (internal/sink). SIGTERM and SIGINT stop it; a second signal ends it at
// once.
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

	"github.com/gardenlinux/feos/go/internal/config/oldnames"
	"github.com/gardenlinux/feos/go/internal/sink"
)

// version is the build's version, set with -ldflags "-X main.version=…";
// otherwise the module's version from the build info, or "dev".
var version = ""

func main() {
	if err := oldnames.Refuse(os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "ferun-sink: %v\n", err)
		os.Exit(2)
	}
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
		sink.Usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "ferun-sink: unknown command %q\n\n", cmd)
		sink.Usage(stderr)
		return 2
	}
	cfg, err := sink.Parse(args, getenv)
	if errors.Is(err, flag.ErrHelp) {
		sink.Usage(stdout)
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "ferun-sink: %v\n", err)
		return 2
	}
	log := sink.NewLogger(stderr, cfg.LogFormat, cfg.LogLevel).With("version", buildVersion())
	slog.SetDefault(log)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go func() {
		// A second signal ends the process at once.
		<-ctx.Done()
		stop()
	}()
	prog, err := sink.New(ctx, cfg, sink.Deps{}, log)
	if err != nil {
		fmt.Fprintf(stderr, "ferun-sink: %v\n", err)
		return 1
	}
	if err := prog.Run(ctx); err != nil {
		fmt.Fprintf(stderr, "ferun-sink: %v\n", err)
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
