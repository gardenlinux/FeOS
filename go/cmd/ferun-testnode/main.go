// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Command ferun-testnode runs a test node (internal/testnode): a fake of
// feosd that joins a control plane and serves pods in memory, for local
// development of ferund and its UI without microVMs.
//
//	ferun-testnode --join-token feos-join-<secret>.<pin> --server-name ferun.example.com \
//	    --addr 127.0.0.1:7443 [--name NAME] [--trust-domain feos.local] [--state-dir DIR] ...
//
// For a control plane's environments end to end: --fake-exec (probes and
// provisioning pass), --fake-processes (attached services start, list and
// stop), --exit 'job/*=0@3s' (deploy jobs finish), --start-log (log
// tails show lines) and --port-addr 8080=127.0.0.1:18080 (routes and
// probes reach a local server).
//
// Every flag has an environment variable FERUN_TESTNODE_<NAME> (upper
// snake case of the flag); a flag wins over its variable. SIGINT and
// SIGTERM stop the node; SIGHUP restarts it gracefully, as an upgrade of
// feosd does (a "stopping" event first, then a new start); SIGUSR1 logs
// its pods, volumes and security groups as one JSON line.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	networkv1 "github.com/gardenlinux/feos/go/gen/feos/network/v1"
	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	volumev1 "github.com/gardenlinux/feos/go/gen/feos/volume/v1"
	"github.com/gardenlinux/feos/go/internal/testnode"
)

func main() {
	if err := run(os.Args[1:], os.Getenv); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "ferun-testnode:", err)
		os.Exit(1)
	}
}

// addrs is a repeatable flag of addresses; a value may list several,
// separated by commas.
type addrs []string

func (a *addrs) String() string { return strings.Join(*a, ",") }

func (a *addrs) Set(v string) error {
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			*a = append(*a, s)
		}
	}
	return nil
}

// list is a repeatable flag whose values are taken whole (an exit rule's
// glob may hold commas).
type list []string

func (l *list) String() string { return strings.Join(*l, " ") }

func (l *list) Set(v string) error {
	*l = append(*l, v)
	return nil
}

// envName is the environment variable of a flag.
func envName(flagName string) string {
	return "FERUN_TESTNODE_" + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// byteSize parses an integer with an optional KiB, MiB, GiB or TiB suffix.
func byteSize(s string) (uint64, error) {
	units := []struct {
		suffix string
		shift  uint
	}{{"TiB", 40}, {"GiB", 30}, {"MiB", 20}, {"KiB", 10}}
	for _, u := range units {
		if v, ok := strings.CutSuffix(s, u.suffix); ok {
			n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return 0, err
			}
			return n << u.shift, nil
		}
	}
	return strconv.ParseUint(s, 10, 64)
}

// parse reads the flags and their environment variables into the node's
// configuration and the log level.
func parse(args []string, getenv func(string) string) (testnode.Config, slog.Level, error) {
	var level slog.Level
	host, _ := os.Hostname()
	if i := strings.IndexByte(host, '.'); i > 0 {
		host = host[:i]
	}
	fs := flag.NewFlagSet("ferun-testnode", flag.ContinueOnError)
	name := fs.String("name", strings.ToLower(host), "the name the node proposes at its join (a DNS label)")
	token := fs.String("join-token", "", "the join token, feos-join-<secret>.<pin>; not needed when --state-dir holds an identity")
	serverName := fs.String("server-name", "", "the DNS name of the control plane's certificate on the nodes' port (required)")
	trustDomain := fs.String("trust-domain", "feos.local", "the SPIFFE trust domain")
	stateDir := fs.String("state-dir", "", "keeps the node's identity and its pods' directories; empty: a temporary directory")
	var bootstrap addrs
	fs.Var(&bootstrap, "addr", "host:port of the control plane's nodes' port, repeatable (required)")
	objectStore := fs.String("object-store", "", "the name of an object store in this process's memory, so that pods hibernate (no other process shares it); empty: none")
	objectStoreDir := fs.String("object-store-dir", "", "a directory that holds an object store, shared by every test node that names it; empty: none")
	pendingDelay := fs.Duration("pending-delay", 2*time.Second, "how long a pod's bring-up takes")
	abortOnCancel := fs.Bool("abort-on-cancel", false, "mutating pod calls stop where today's feosd does when their request is cancelled")
	maxStreams := fs.Uint("max-streams", 1024, "the HTTP/2 stream limit of the node connections")
	cpus := fs.Uint("cpus", 16, "the CPU cores the node reports")
	memory := fs.String("memory", "64GiB", "the memory the node reports")
	disk := fs.String("disk", "1TiB", "the disk the node reports")
	podPrefix := fs.String("pod-prefix", "", "the pods' IPv6 prefix; empty: derived from the name")
	onMesh := fs.Bool("mesh", false, "put the node on the pods' mesh: ask JoinMesh with a key of its own, and serve MeshService")
	meshEndpoint := fs.String("mesh-endpoint", "", "the node's endpoint on the mesh, address:port; empty: 127.0.0.1 and a port from the name")
	fakeExec := fs.Bool("fake-exec", false, "every exec into a pod exits 0 without running anything (an environment's addon probes and provisioning)")
	checkGroups := fs.Bool("check-security-groups", true, "a pod that names a security group the node does not have fails, as on feosd")
	checkTables := fs.Bool("check-name-tables", true, "a pod that names a name table the node does not have fails, as on feosd")
	noNames := fs.Bool("no-name-service", false, "serve no NameService, as a feosd from before it: pods resolve no names of their environments")
	var exits list
	var portAddrs addrs
	fs.Var(&exits, "exit", "<object glob>=<code>[@<delay>]: the main container of an environment's pod whose ferun.object label matches exits with the code that long after each boot (default 2s; a negative code: no exit status), repeatable, the first match counts: job/*=0@3s, service/crasher=1")
	fakeProcesses := fs.Bool("fake-processes", false, "answer the control plane's background-process scripts (process_start, an environment's attached services) in memory without running anything")
	startLog := fs.Bool("start-log", false, "append a line to a container's log at each boot of its pod and at an exit of --exit")
	fs.Var(&portAddrs, "port-addr", "<port>=<host:port>: a port forward to a pod's port dials this address in place of 127.0.0.1:<port>, repeatable")
	logLevel := fs.String("log-level", "info", "debug, info, warn or error")

	if err := fs.Parse(args); err != nil {
		return testnode.Config{}, level, err
	}
	if fs.NArg() > 0 {
		return testnode.Config{}, level, fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	// The environment for the flags not given, so that a flag wins over its
	// variable, also a repeatable one.
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	var envErr error
	fs.VisitAll(func(f *flag.Flag) {
		if v := getenv(envName(f.Name)); v != "" && !given[f.Name] && envErr == nil {
			if err := fs.Set(f.Name, v); err != nil {
				envErr = fmt.Errorf("%s: %w", envName(f.Name), err)
			}
		}
	})
	if envErr != nil {
		return testnode.Config{}, level, envErr
	}
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		return testnode.Config{}, level, fmt.Errorf("--log-level: %w", err)
	}
	memBytes, err := byteSize(*memory)
	if err != nil {
		return testnode.Config{}, level, fmt.Errorf("--memory: %w", err)
	}
	diskBytes, err := byteSize(*disk)
	if err != nil {
		return testnode.Config{}, level, fmt.Errorf("--disk: %w", err)
	}
	if *maxStreams > 1<<31 || *cpus > 1<<16 {
		return testnode.Config{}, level, errors.New("--max-streams or --cpus out of range")
	}
	cfg := testnode.Config{
		Name:          *name,
		JoinToken:     *token,
		ServerName:    *serverName,
		TrustDomain:   *trustDomain,
		StateDir:      *stateDir,
		Addrs:         bootstrap,
		PendingDelay:  *pendingDelay,
		AbortOnCancel: *abortOnCancel,
		MaxStreams:    uint32(*maxStreams),
		CPUCores:      uint32(*cpus),
		MemoryBytes:   memBytes,
		DiskBytes:     diskBytes,
		PodPrefix:     *podPrefix,
		Mesh:          *onMesh,
		MeshEndpoint:  *meshEndpoint,

		FakeExec:            *fakeExec,
		CheckSecurityGroups: *checkGroups,
		CheckNameTables:     *checkTables,
		NoNameService:       *noNames,
		StartLog:            *startLog,
		FakeProcesses:       *fakeProcesses,
	}
	for _, e := range exits {
		r, err := testnode.ParseExitRule(e)
		if err != nil {
			return testnode.Config{}, level, fmt.Errorf("--exit: %w", err)
		}
		cfg.ContainerExits = append(cfg.ContainerExits, r)
	}
	for _, pa := range portAddrs {
		portText, addr, ok := strings.Cut(pa, "=")
		port, err := strconv.ParseUint(portText, 10, 16)
		if !ok || err != nil || port == 0 || addr == "" {
			return testnode.Config{}, level, fmt.Errorf("--port-addr %q: want <port>=<host:port>", pa)
		}
		if cfg.PortAddrs == nil {
			cfg.PortAddrs = map[uint32]string{}
		}
		cfg.PortAddrs[uint32(port)] = addr
	}
	switch {
	case *objectStore != "" && *objectStoreDir != "":
		return testnode.Config{}, level, errors.New("--object-store and --object-store-dir exclude each other")
	case *objectStore != "":
		cfg.ObjectStore = testnode.NewObjectStore(*objectStore)
	case *objectStoreDir != "":
		if cfg.ObjectStore, err = testnode.OpenObjectStore(*objectStoreDir); err != nil {
			return testnode.Config{}, level, err
		}
	}
	return cfg, level, nil
}

func run(args []string, getenv func(string) string) error {
	cfg, level, err := parse(args, getenv)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	cfg.Log = log
	n, err := testnode.New(cfg)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	usr1 := make(chan os.Signal, 1)
	signal.Notify(usr1, syscall.SIGUSR1)
	defer signal.Stop(usr1)
	go func() {
		for {
			select {
			case <-hup:
				log.Info("ferun-testnode: restarting gracefully")
				n.Restart(true)
			case <-usr1:
				b, err := json.Marshal(summary(n.Pods(), n.Volumes(), n.SecurityGroups(), n.NameTables()))
				if err != nil {
					log.Warn("ferun-testnode: state", "err", err)
					continue
				}
				log.Info("ferun-testnode: state", "state", string(b))
			case <-ctx.Done():
				return
			}
		}
	}()
	log.Info("ferun-testnode: starting", "name", cfg.Name, "addrs", strings.Join(cfg.Addrs, ","), "server-name", cfg.ServerName)
	return n.Run(ctx)
}

// state is what SIGUSR1 logs of a node: its pods, volumes, security
// groups and name tables, briefly.
type state struct {
	Pods    []podState    `json:"pods"`
	Volumes []volumeState `json:"volumes"`
	Groups  []groupState  `json:"groups"`
	Tables  []tableState  `json:"nameTables"`
}

type podState struct {
	ID      string   `json:"id"`
	State   string   `json:"state"`
	Object  string   `json:"object,omitempty"`
	Groups  []string `json:"groups,omitempty"`
	Tables  []string `json:"nameTables,omitempty"`
	Volumes []string `json:"volumes,omitempty"`
}

type volumeState struct {
	ID     string `json:"id"`
	State  string `json:"state"`
	Pod    string `json:"pod,omitempty"`
	Object string `json:"object,omitempty"`
}

type groupState struct {
	ID      string   `json:"id"`
	Version uint64   `json:"version"`
	Members []string `json:"members"`
	Rules   int      `json:"rules"`
}

// tableState is a name table: its names with their addresses.
type tableState struct {
	ID       string              `json:"id"`
	Version  uint64              `json:"version"`
	Names    map[string][]string `json:"names"`
	Includes []string            `json:"includes,omitempty"`
}

func summary(pods []*podv1.PodInfo, volumes []*volumev1.Volume, groups []*networkv1.SecurityGroup,
	tables []*networkv1.NameTable) state {
	out := state{Pods: []podState{}, Volumes: []volumeState{}, Groups: []groupState{}, Tables: []tableState{}}
	for _, p := range pods {
		ps := podState{ID: p.GetPodId(), State: strings.TrimPrefix(p.GetState().String(), "POD_STATE_"),
			Object: p.GetSpec().GetLabels()["ferun.object"], Groups: p.GetSpec().GetNetwork().GetSecurityGroups(),
			Tables: p.GetSpec().GetNetwork().GetNameTables()}
		for _, v := range p.GetSpec().GetVolumes() {
			ps.Volumes = append(ps.Volumes, v.GetVolumeId())
		}
		out.Pods = append(out.Pods, ps)
	}
	for _, v := range volumes {
		out.Volumes = append(out.Volumes, volumeState{ID: v.GetVolumeId(), State: strings.TrimPrefix(v.GetState().String(), "VOLUME_STATE_"),
			Pod: v.GetPodId(), Object: v.GetLabels()["ferun.object"]})
	}
	for _, g := range groups {
		out.Groups = append(out.Groups, groupState{ID: g.GetId(), Version: g.GetVersion(), Members: g.GetMembers(), Rules: len(g.GetRules())})
	}
	for _, t := range tables {
		ts := tableState{ID: t.GetId(), Version: t.GetVersion(), Names: map[string][]string{}, Includes: t.GetIncludes()}
		for _, e := range t.GetEntries() {
			ts.Names[e.GetName()] = e.GetAddresses()
		}
		out.Tables = append(out.Tables, ts)
	}
	return out
}
