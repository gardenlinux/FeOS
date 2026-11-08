// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The pods of rows with a pod template (ADR 0018 §2.3): an environment's
// instances, the tenants' jobs' pods and the sandboxes. Their spec is built from the
// template at the create, with the template's secrets unsealed by the
// SecretResolver; the database holds no secret value.

// errNoResolver is a template that names a secret while the engine has
// no SecretResolver.
var errNoResolver = errors.New("no secret resolver: the environments are not set up on this instance")

// podSpec is the spec of the sandbox's pod, built from its pod template
// when it has one. Identical on every retry and takeover as long as the
// secrets it names keep their values (a version's value never changes),
// because feosd compares the normalised spec, labels included, to make a
// create idempotent. A secret that does not resolve fails it: the create
// sends nothing then.
func (e *Engine) podSpec(ctx context.Context, sb store.Sandbox) (*podv1.PodSpec, error) {
	return buildPodSpec(ctx, sb, e.secretResolver(), e.cfg.RegistryEgress)
}

// buildPodSpec is podSpec with the resolver given.
func buildPodSpec(ctx context.Context, sb store.Sandbox, resolve SecretResolver, registryEgress map[string][]string) (*podv1.PodSpec, error) {
	labels := map[string]string{
		LabelSandbox:     sb.ID.String(),
		LabelTenant:      sb.Owner,
		LabelIncarnation: sb.Incarnation.String(),
	}
	spec := &podv1.PodSpec{
		Resources: &podv1.Resources{Vcpus: uint32(sb.VCPUs), MemoryMib: uint64(sb.MemoryMiB)},
		Network:   &podv1.PodNetwork{},
		Labels:    labels,
	}
	t := sb.PodTemplate
	if t == nil {
		spec.Containers = []*podv1.ContainerSpec{{Name: mainContainer, ImageRef: sb.ImageRef, Command: []string{"sleep", "infinity"}}}
		return spec, nil
	}
	main := &podv1.ContainerSpec{Name: mainContainer, ImageRef: sb.ImageRef}
	if len(t.Command) > 0 {
		main.Command = append([]string(nil), t.Command...)
	}
	if len(t.Env) > 0 {
		env, err := resolveEnv(ctx, sb, t.Env, resolve)
		if err != nil {
			return nil, err
		}
		main.Env = env
	}
	for _, m := range t.Mounts {
		main.VolumeMounts = append(main.VolumeMounts, &podv1.VolumeMount{Volume: m.Volume, Path: m.Path, ReadOnly: m.ReadOnly})
	}
	for _, v := range t.Volumes {
		spec.Volumes = append(spec.Volumes, &podv1.PodVolume{Name: v.Name, VolumeId: v.VolumeID.String()})
	}
	if len(t.SecurityGroups) > 0 {
		spec.Network.SecurityGroups = append([]string(nil), t.SecurityGroups...)
	}
	if len(t.NameTables) > 0 {
		spec.Network.NameTables = append([]string(nil), t.NameTables...)
	}
	// Its egress by name (ADR 0034, revised): the resolver answers only
	// these upstream names.
	if t.RestrictEgressNames {
		spec.Network.RestrictEgressNames = true
		spec.Network.EgressDomains = egressDomains(t.EgressDomains, sb.ImageRef, registryEgress)
	}
	// The sandbox labels win: a template cannot make its pod another
	// sandbox's, tenant's or incarnation's.
	for k, v := range t.Labels {
		if _, ok := labels[k]; !ok {
			labels[k] = v
		}
	}
	spec.Containers = []*podv1.ContainerSpec{main}
	return spec, nil
}

// secretKey names one version of a secret.
type secretKey struct {
	name    string
	version int
}

// resolveEnv renders a template's environment, each secret version
// unsealed once.
func resolveEnv(ctx context.Context, row store.Sandbox, env map[string]model.TemplateValue, resolve SecretResolver) (map[string]string, error) {
	r := &envResolver{ctx: ctx, row: row, resolve: resolve, seen: map[secretKey]string{}}
	out := make(map[string]string, len(env))
	for name, v := range env {
		s, err := r.value(v)
		if err != nil {
			return nil, fmt.Errorf("env %s: %w", name, err)
		}
		out[name] = s
	}
	return out, nil
}

// envResolver renders the values of one template.
type envResolver struct {
	ctx     context.Context
	row     store.Sandbox
	resolve SecretResolver
	seen    map[secretKey]string
}

// value renders one value: a literal, a secret (escaped), or its parts
// joined.
func (r *envResolver) value(v model.TemplateValue) (string, error) {
	switch {
	case v.Parts != nil:
		var b strings.Builder
		for i, p := range v.Parts {
			s, err := r.part(p)
			if err != nil {
				return "", fmt.Errorf("part %d: %w", i, err)
			}
			b.WriteString(s)
		}
		return b.String(), nil
	case v.Secret != "":
		return r.secret(v.Secret, v.Version, v.Escape)
	}
	return v.Value, nil
}

func (r *envResolver) part(p model.TemplatePart) (string, error) {
	switch {
	case p.Credential != "":
		// A blueprint's credential is mapped to a system secret before a
		// template is stored.
		return "", fmt.Errorf("an unmapped credential %q", p.Credential)
	case p.Secret != "":
		return r.secret(p.Secret, p.Version, p.Escape)
	}
	return p.Value, nil
}

// secret unseals one version of a secret of the row's environment and
// escapes it.
func (r *envResolver) secret(name string, version int, escape string) (string, error) {
	k := secretKey{name, version}
	s, ok := r.seen[k]
	if !ok {
		if r.resolve == nil {
			return "", fmt.Errorf("secret %s version %d: %w", name, version, errNoResolver)
		}
		if r.row.IsOwn() {
			return "", fmt.Errorf("secret %s version %d: the sandbox is no environment's or tenant job's", name, version)
		}
		var err error
		if s, err = r.resolve(r.ctx, r.row, name, version); err != nil {
			return "", fmt.Errorf("secret %s version %d of %s: %w", name, version, secretOwner(r.row), err)
		}
		r.seen[k] = s
	}
	switch escape {
	case model.EscapeNone:
		return s, nil
	case model.EscapeURL:
		return escapeUserinfo(s), nil
	}
	return "", fmt.Errorf("secret %s: unknown escape %q", name, escape)
}

// secretOwner names what a row's secrets belong to in an error.
func secretOwner(row store.Sandbox) string {
	if row.TenantJob {
		return "job " + row.ID.String()
	}
	return "environment " + row.EnvironmentID.String()
}

// escapeUserinfo escapes s for the user or the password of a URL's
// userinfo (RFC 3986 §3.2.1): the unreserved characters and the
// sub-delims stay, every other byte is percent-encoded in upper-case hex,
// ":" too (it would end a user), and so are "@", "/", "?", "#", "[", "]",
// "%" and anything outside ASCII (UTF-8 bytes one by one).
func escapeUserinfo(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if userinfoKeeps(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&15])
	}
	return b.String()
}

// userinfoKeeps reports whether c stands as it is in a userinfo part:
// unreserved (ALPHA, DIGIT, "-", ".", "_", "~") or a sub-delim ("!", "$",
// "&", "'", "(", ")", "*", "+", ",", ";", "=").
func userinfoKeeps(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		return true
	}
	return strings.IndexByte("-._~!$&'()*+,;=", c) >= 0
}

// egressDomains are a pod's allowed names and the hosts its image is
// pulled from: the image is declared, so its registry is egress it
// declares (ADR 0034, revised), and the guest pulls over the pod's
// network until the node pulls (ADR 0022, ADR 0025). Sorted, without
// repeats.
func egressDomains(domains []string, imageRef string, registryEgress map[string][]string) []string {
	out := append([]string(nil), domains...)
	out = append(out, registryHosts(imageRef)...)
	out = append(out, registryEgress[registryHost(imageRef)]...)
	slices.Sort(out)
	return slices.Compact(out)
}

// registryHosts are the names a pull of ref talks to: the registry, and
// for the two public registries the hosts their blobs and tokens come
// from. An image without a registry host is Docker Hub's.
func registryHosts(ref string) []string {
	host := registryHost(ref)
	switch host {
	case "docker.io", "index.docker.io", "registry-1.docker.io":
		// Its blobs come from CDNs that change: docker.com's and
		// Cloudflare R2's.
		return []string{"*.docker.com", "*.docker.io", "*.r2.cloudflarestorage.com", "docker.io"}
	case "ghcr.io":
		return []string{"*.githubusercontent.com", "ghcr.io"}
	case "", "localhost":
		return nil
	}
	return []string{host}
}

// registryHost is the host of ref's registry, without a port; Docker
// Hub's for an image without one.
func registryHost(ref string) string {
	host := "docker.io"
	if i := strings.IndexByte(ref, '/'); i > 0 {
		if first := ref[:i]; strings.ContainsAny(first, ".:") || first == "localhost" {
			host = first
		}
	}
	if h, _, ok := strings.Cut(host, ":"); ok {
		host = h
	}
	return strings.ToLower(host)
}
