// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Command ferun-ingress is the ingress of FeRun (ADR 0021): it joins the
// control plane, takes a place on the pods' WireGuard mesh, keeps the
// table of public environments and serves their hosts over HTTPS,
// forwarding each request to an instance of the routed service.
//
//	ferun-ingress [serve] [flags]   run the ingress (the default)
//	ferun-ingress prepare           enable IPv6 and copy the instance's join token (an init container, as root)
//	ferun-ingress status            print the running instance's /statusz
//	ferun-ingress version           print the version
//
// Every flag has an environment variable FERUN_INGRESS_<NAME>
// (internal/ingress). SIGTERM and SIGINT stop serve as §7.6 of the design
// says: /readyz fails at once, the listeners serve on for
// --shutdown-delay, requests finish within --shutdown-grace, and the
// instance's holds are released. A second signal ends it at once.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/gardenlinux/feos/go/internal/config/oldnames"
	"github.com/gardenlinux/feos/go/internal/ingress"
	"github.com/gardenlinux/feos/go/internal/ingress/agent"
)

// version is the build's version, set with -ldflags "-X main.version=…";
// otherwise the module's version from the build info, or "dev".
var version = ""

// maxStatus bounds the body status reads.
const maxStatus = 16 << 20

func main() {
	if err := oldnames.Refuse(os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "ferun-ingress: %v\n", err)
		os.Exit(2)
	}
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// run runs a subcommand and returns the exit code: 0, 1 for a failure, 2
// for a usage error.
func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	cmd := ingress.CmdServe
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	} else if len(args) > 0 && (args[0] == "-h" || args[0] == "-help" || args[0] == "--help") {
		ingress.Usage(stdout, "")
		return 0
	}
	var err error
	switch cmd {
	case ingress.CmdServe:
		err = serve(args, getenv, stderr)
	case ingress.CmdPrepare:
		err = prepare(args, getenv, stderr)
	case ingress.CmdStatus:
		err = status(args, getenv, stdout)
	case ingress.CmdVersion:
		fmt.Fprintln(stdout, buildVersion())
		return 0
	case "help":
		topic := ""
		if len(args) > 0 {
			topic = args[0]
		}
		ingress.Usage(stdout, topic)
		return 0
	default:
		fmt.Fprintf(stderr, "ferun-ingress: unknown command %q\n\n", cmd)
		ingress.Usage(stderr, "")
		return 2
	}
	if errors.Is(err, flag.ErrHelp) {
		ingress.Usage(stdout, cmd)
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "ferun-ingress %s: %v\n", cmd, err)
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

// serve runs the ingress until SIGTERM or SIGINT, then stops it.
func serve(args []string, getenv func(string) string, stderr io.Writer) error {
	cfg, err := ingress.Parse(args, getenv)
	if err != nil {
		return flagErr(err)
	}
	cfg.Version = buildVersion()
	log := ingress.NewLogger(stderr, cfg.LogFormat, cfg.LogLevel)
	slog.SetDefault(log)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go func() {
		// A second signal ends the process at once.
		<-ctx.Done()
		stop()
	}()
	prog, err := ingress.New(cfg, ingress.Deps{Mesh: agent.NewKernelMesh(log), Log: log})
	if err != nil {
		return err
	}
	return prog.Run(ctx)
}

// prepare enables IPv6 in the network namespace and copies the
// instance's own join token.
func prepare(args []string, getenv func(string) string, stderr io.Writer) error {
	c, err := ingress.ParsePrepare(args, getenv)
	if err != nil {
		return flagErr(err)
	}
	log := ingress.NewLogger(stderr, c.LogFormat, c.LogLevel)
	if err := ingress.Prepare(ingress.IPv6ConfDir, log); err != nil {
		return err
	}
	if c.JoinTokenSource == "" {
		return nil
	}
	return ingress.CopyJoinToken(c.JoinTokenSource, c.JoinTokenFile, log)
}

// status prints the running instance's /statusz.
func status(args []string, getenv func(string) string, stdout io.Writer) error {
	c, err := ingress.ParseStatus(args, getenv)
	if err != nil {
		return flagErr(err)
	}
	url := "http://" + c.HealthListen + "/statusz"
	client := &http.Client{Timeout: c.Timeout}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxStatus))
	if err != nil {
		return fmt.Errorf("read %s: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s: %s", url, resp.Status, strings.TrimSpace(string(body)))
	}
	_, err = stdout.Write(body)
	return err
}
