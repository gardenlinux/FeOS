// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package addons

import (
	"fmt"
	"slices"
	"strconv"

	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
)

// nats is a NATS server from the official alpine image, with JetStream's
// store on the volume. Its command replaces the image's, which would load
// a config with a cluster listener and a built-in route password: one
// user "nats" with the generated password, clients on [::]:4222 (one
// dual-stack socket), the unauthenticated monitor on loopback only
// (through a config file written at start: the flags have no monitor
// host), where the probe reads its healthz. The server runs as root; the
// image has no other user.
//
// Option "jetstream" (default true): without it the server keeps nothing
// and the addon has no volume.
type nats struct{}

func init() { _ = Register(nats{}) }

const (
	natsPort  = 4222
	natsMount = "/data"
	natsUser  = "nats"
	// natsServe writes the monitor's config and starts the server, the
	// password from the environment (the variable is quoted, so any
	// value stays one word; exec makes the server the container's
	// process).
	natsServe = `printf 'http: "127.0.0.1:8222"\n' > /tmp/nats.conf && ` +
		`exec nats-server -c /tmp/nats.conf -a :: -p 4222 --user nats --pass "$NATS_PASSWORD"`
	natsJetStream = " -js -sd " + natsMount
)

func (nats) Type() string        { return "nats" }
func (nats) Versions() []string  { return []string{"2"} }
func (nats) DefaultPlan() string { return "small" }

func (nats) Plans() map[string]Plan {
	return map[string]Plan{
		"dev":   {VCPUs: 1, MemoryMib: 128, VolumeMib: 1024},
		"small": {VCPUs: 1, MemoryMib: 512, VolumeMib: 10240},
	}
}

// natsJetStreamOption is nats's option, which jetstream reads.
var natsJetStreamOption = Option{Name: "jetstream", Kind: OptionBoolean, Default: natsJetStreamDefault,
	Description: "JetStream, with its store on the addon's volume. Without it the server keeps nothing and " +
		"the addon has no volume; turning it off later no longer serves the streams on the volume."}

// natsJetStreamDefault is the jetstream option's default.
const natsJetStreamDefault = true

func (nats) OutputKeys() []string { return []string{"host", "port", "user", "password", "url"} }

func (nats) OutputFields() []string { return nil }

func (nats) Options() []Option { return []Option{natsJetStreamOption} }

func (n nats) jetstream(a Addon) (bool, []spec.Finding) {
	fs := unknownOptions(n, a.Options)
	js, jfs := boolOption(a.Options, natsJetStreamOption.Name, natsJetStreamDefault)
	return js, append(fs, jfs...)
}

func (n nats) Validate(prev *Addon, next Addon) []spec.Finding {
	prev = sameType(prev, n.Type())
	js, jfs := n.jetstream(next)
	fs := checkVersionPlan(n, prev, next, js)
	if prev != nil {
		if was, _ := n.jetstream(*prev); was && !js {
			fs = append(fs, warning("options.jetstream", "JetStream is turned off: the server is recreated "+
				"without it and no longer serves the streams on its volume", "keep jetstream true to keep them"))
		}
	}
	return append(fs, jfs...)
}

func (nats) Credentials(Addon) []string { return []string{passwordKey} }

func (n nats) Render(a Addon, _ Context) (Instance, error) {
	if !slices.Contains(n.Versions(), a.Version) {
		return Instance{}, fmt.Errorf("nats: no version %q", a.Version)
	}
	if name, _, ok := planOf(n, a); !ok {
		return Instance{}, fmt.Errorf("nats: no plan %q", name)
	}
	js, fs := n.jetstream(a)
	if err := firstError(n.Type(), fs); err != nil {
		return Instance{}, err
	}
	// JetStream sizes its limits from the pod's memory and the volume's
	// free space; no flags needed.
	serve, mount := natsServe, ""
	if js {
		serve, mount = natsServe+natsJetStream, natsMount
	}
	return Instance{
		Image:   "nats:" + a.Version + "-alpine",
		Command: []string{"sh", "-c", serve},
		Env: map[string]model.TemplateValue{
			"NATS_PASSWORD": model.CredentialValue(passwordKey),
		},
		Port:  natsPort,
		Mount: mount,
		// busybox wget fails on an HTTP error, so a 503 is not ready.
		Probe: []string{"wget", "-qO-", "http://127.0.0.1:8222/healthz"},
	}, nil
}

// Outputs: host, port, user ("nats"), password and url
// nats://nats:<password>@[addr]:4222. No fields.
func (n nats) Outputs(_ Addon, c Context, ref Ref) ([]model.TemplatePart, error) {
	if err := checkRef(n.Type(), ref, n.OutputKeys(), n.OutputFields()...); err != nil {
		return nil, err
	}
	switch ref.Key {
	case "host":
		return literal(host(c)), nil
	case "port":
		return literal(strconv.Itoa(natsPort)), nil
	case "user":
		return literal(natsUser), nil
	case "password":
		return credential(passwordKey), nil
	}
	return serverURL("nats", natsUser, passwordKey, c, natsPort, ""), nil
}

func (nats) Provision(Addon, Context) []Step { return nil }
