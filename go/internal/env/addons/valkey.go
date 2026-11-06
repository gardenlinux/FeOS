// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package addons

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
)

// valkey is Valkey (the Redis fork) from the official alpine image, with
// the append-only file on the volume. The image's own command runs: its
// entrypoint chowns /data, drops to the valkey user and appends
// $VALKEY_EXTRA_FLAGS unquoted (word-split, so the generated hex password
// stays one word). The defaults bind every address, IPv6 included, with
// protected mode off; the password protects it.
//
// Option "maxmemoryPolicy": what happens at the memory limit (70% of the
// plan's memory), noeviction by default.
type valkey struct{}

func init() { _ = Register(valkey{}) }

const (
	valkeyPort  = 6379
	valkeyMount = "/data"
	// valkeyUser is the ACL user requirepass sets: a URL needs it, since
	// clients send an empty user name as one.
	valkeyUser          = "default"
	valkeyDefaultPolicy = "noeviction"
)

// valkeyPolicies are the maxmemory-policy values of valkey 7.2 and 8; any
// other stops the server at start.
var valkeyPolicies = []string{
	"volatile-lru", "volatile-lfu", "volatile-random", "volatile-ttl",
	"allkeys-lru", "allkeys-lfu", "allkeys-random", "noeviction",
}

func (valkey) Type() string        { return "valkey" }
func (valkey) Versions() []string  { return []string{"7.2", "8"} }
func (valkey) DefaultPlan() string { return "small" }

func (valkey) Plans() map[string]Plan {
	return map[string]Plan{
		"dev":    {VCPUs: 1, MemoryMib: 128, VolumeMib: 256},
		"small":  {VCPUs: 1, MemoryMib: 512, VolumeMib: 2048},
		"medium": {VCPUs: 1, MemoryMib: 2048, VolumeMib: 8192},
		"large":  {VCPUs: 2, MemoryMib: 8192, VolumeMib: 32768},
	}
}

// valkeyMaxmemoryPolicy is valkey's option, which policy reads.
var valkeyMaxmemoryPolicy = Option{Name: "maxmemoryPolicy", Kind: OptionOneOf, Default: valkeyDefaultPolicy,
	Values: valkeyPolicies,
	Description: "What valkey does at its memory limit (70% of the plan's memory): noeviction refuses writes, " +
		"the others evict keys (allkeys-*: any key, volatile-*: keys with a TTL)."}

func (valkey) OutputKeys() []string { return []string{"host", "port", "user", "password", "url"} }

func (valkey) OutputFields() []string { return nil }

func (valkey) Options() []Option { return []Option{valkeyMaxmemoryPolicy} }

func (v valkey) policy(a Addon) (string, []spec.Finding) {
	opt := valkeyMaxmemoryPolicy
	fs := unknownOptions(v, a.Options)
	policy, pfs := stringOption(a.Options, opt.Name, valkeyDefaultPolicy)
	fs = append(fs, pfs...)
	if len(pfs) == 0 && !slices.Contains(opt.Values, policy) {
		fs = append(fs, errorf("options."+opt.Name, fmt.Sprintf("valkey has no maxmemory policy %q", policy),
			"use one of "+strings.Join(opt.Values, ", ")))
	}
	return policy, fs
}

func (v valkey) Validate(prev *Addon, next Addon) []spec.Finding {
	prev = sameType(prev, v.Type())
	fs := checkVersionPlan(v, prev, next, true)
	if prev != nil {
		from, to := slices.Index(v.Versions(), prev.Version), slices.Index(v.Versions(), next.Version)
		switch {
		case from < 0 || to < 0 || from == to:
		case to < from:
			fs = append(fs, errorf("version",
				fmt.Sprintf("valkey cannot go back from %s to %s: a newer version's data files are not "+
					"promised to load in an older one", prev.Version, next.Version),
				"keep version "+prev.Version))
		default:
			fs = append(fs, info("version",
				fmt.Sprintf("valkey goes from %s to %s on the same volume, which keeps the data; going back "+
					"later is refused", prev.Version, next.Version), ""))
		}
	}
	_, pfs := v.policy(next)
	return append(fs, pfs...)
}

func (valkey) Credentials(Addon) []string { return []string{passwordKey} }

func (v valkey) Render(a Addon, _ Context) (Instance, error) {
	if !slices.Contains(v.Versions(), a.Version) {
		return Instance{}, fmt.Errorf("valkey: no version %q", a.Version)
	}
	name, plan, ok := planOf(v, a)
	if !ok {
		return Instance{}, fmt.Errorf("valkey: no plan %q", name)
	}
	policy, fs := v.policy(a)
	if err := firstError(v.Type(), fs); err != nil {
		return Instance{}, err
	}
	// The rest of the plan's memory is the kernel's, the guest's and an
	// AOF rewrite's fork.
	maxmemory := plan.MemoryMib * 7 / 10
	return Instance{
		Image: "valkey/valkey:" + a.Version + "-alpine",
		Env: map[string]model.TemplateValue{
			"VALKEY_EXTRA_FLAGS": {Parts: []model.TemplatePart{
				{Value: "--appendonly yes --requirepass "},
				{Credential: passwordKey},
				{Value: " --maxmemory " + strconv.Itoa(maxmemory) + "mb --maxmemory-policy " + policy},
			}},
			// For the probe, which runs with the container's env.
			"VALKEY_PASSWORD": model.CredentialValue(passwordKey),
		},
		Port:  valkeyPort,
		Mount: valkeyMount,
		// valkey-cli exits 0 on a NOAUTH answer too, hence the grep;
		// REDISCLI_AUTH is the only variable it reads a password from.
		Probe: []string{"sh", "-c", `REDISCLI_AUTH="$VALKEY_PASSWORD" valkey-cli ping | grep -qx PONG`},
	}, nil
}

// Outputs: host, port, user ("default"), password and url
// redis://default:<password>@[addr]:6379/0. No fields.
func (v valkey) Outputs(_ Addon, c Context, ref Ref) ([]model.TemplatePart, error) {
	if err := checkRef(v.Type(), ref, v.OutputKeys(), v.OutputFields()...); err != nil {
		return nil, err
	}
	switch ref.Key {
	case "host":
		return literal(host(c)), nil
	case "port":
		return literal(strconv.Itoa(valkeyPort)), nil
	case "user":
		return literal(valkeyUser), nil
	case "password":
		return credential(passwordKey), nil
	}
	return serverURL("redis", valkeyUser, passwordKey, c, valkeyPort, "/0"), nil
}

func (valkey) Provision(Addon, Context) []Step { return nil }
