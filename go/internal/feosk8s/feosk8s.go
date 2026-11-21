// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package feosk8s is the controller feos-k8s (ADR 0020): it keeps the FeOS
// nodes of a Kubernetes cluster and FeRun in step. A node that matches the
// node selector gets a join token for its name, which FeRun pins to it; a
// guard pod under a PodDisruptionBudget that allows no disruption holds a
// cordoned node's drain until FeRun has drained the node; an uncordon
// undrains it; and a FeRun node whose Kubernetes node is gone is removed.
//
// The controller polls: every pass reads the nodes, the guards, the join
// secret and FeRun's nodes, and acts on the difference. A pass is
// idempotent, and what must outlive a restart lives in Kubernetes (the
// node annotation feos.fecode.dev/drain, the guards, the secret's keys) or
// in FeRun (its nodes and join tokens).
package feosk8s

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gardenlinux/feos/go/internal/kube"
)

// The names the controller marks its objects with.
const (
	// AnnotationDrain on a node says that the controller asked FeRun to
	// drain it for a cordon, so that an uncordon undrains it, after a
	// restart too. A drain an operator began in FeRun has no annotation
	// and is left alone.
	AnnotationDrain = "feos.fecode.dev/drain"
	drainRequested  = "requested"

	// LabelApp and GuardApp select the guard pods; the static
	// PodDisruptionBudget selects them by these.
	LabelApp = "app.kubernetes.io/name"
	GuardApp = "feos-guard"
	// LabelNode names a guard's node.
	LabelNode = "feos.fecode.dev/node"

	guardPrefix = "feos-guard-"
)

// Config is the controller's configuration.
type Config struct {
	// NodeSelector selects the Kubernetes nodes that run FeOS.
	NodeSelector string
	// NodeNamePrefix: only FeRun nodes whose name begins with it are
	// removed, so that the nodes of other clusters stay. Required.
	NodeNamePrefix string
	// Namespace holds the guards and the join secret.
	Namespace string
	// JoinSecret is the secret with a join token per node name.
	JoinSecret string
	// GuardImage is the guards' image; GuardPriorityClass their priority.
	GuardImage         string
	GuardPriorityClass string
	// Interval is the time between passes.
	Interval time.Duration
	// JoinTokenTTL is a join token's lifetime; an unused token that
	// expired is replaced.
	JoinTokenTTL time.Duration
	// RemoveAfter is how long a FeRun node is seen down without a
	// Kubernetes node before it is removed.
	RemoveAfter time.Duration
}

// Controller is the controller.
type Controller struct {
	cfg   Config
	kube  *kube.Client
	ferun *FeRun
	log   *slog.Logger
	now   func() time.Time

	// The loop's own memory, which a restart may lose: what was logged
	// last per node and topic (so that a state that lasts is logged once),
	// and since when a FeRun node is seen gone.
	noted     map[string]string
	goneSince map[string]time.Time

	lastPass atomic.Int64 // the end of the last pass, Unix ns
}

// New returns a controller.
func New(cfg Config, k *kube.Client, f *FeRun, log *slog.Logger) (*Controller, error) {
	switch {
	case cfg.NodeNamePrefix == "":
		return nil, errors.New("the node name prefix is required")
	case cfg.Namespace == "" || cfg.JoinSecret == "" || cfg.GuardImage == "":
		return nil, errors.New("the namespace, the join secret and the guard image are required")
	case cfg.Interval <= 0 || cfg.JoinTokenTTL <= 0 || cfg.RemoveAfter < 0:
		return nil, errors.New("the interval and the join tokens' lifetime must be positive")
	}
	return &Controller{cfg: cfg, kube: k, ferun: f, log: log, now: time.Now,
		noted: map[string]string{}, goneSince: map[string]time.Time{}}, nil
}

// Run reconciles every interval until ctx ends.
func (c *Controller) Run(ctx context.Context) error {
	t := time.NewTicker(c.cfg.Interval)
	defer t.Stop()
	for {
		if err := c.Reconcile(ctx); err != nil && ctx.Err() == nil {
			c.log.Warn("reconcile", "err", err)
		}
		c.lastPass.Store(time.Now().UnixNano())
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Healthy reports whether a pass ended within maxAge.
func (c *Controller) Healthy(maxAge time.Duration) bool {
	last := c.lastPass.Load()
	return last != 0 && time.Since(time.Unix(0, last)) <= maxAge
}

// pass is what one pass read. Its maps are by FeRun name (FeRunName).
type pass struct {
	all    map[string]bool      // every Kubernetes node
	feos   map[string]kube.Node // the nodes of the selector
	clash  map[string]bool      // names that two nodes of the selector share
	ferun  map[string]FeRunNode
	guards map[string]kube.Pod // by their node

	secretExists bool
	secretData   map[string][]byte

	tokens       []JoinToken
	tokensLoaded bool
}

// Reconcile runs one pass. It acts on every node it can and returns the
// errors of those it could not.
func (c *Controller) Reconcile(ctx context.Context) error {
	p, err := c.read(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, name := range slices.Sorted(maps.Keys(p.feos)) {
		n := p.feos[name]
		if !isNodeName(name) {
			c.note(name, "name", "node name is no DNS label; FeOS cannot run on it", slog.LevelWarn)
			continue
		}
		if err := c.join(ctx, p, name); err != nil {
			errs = append(errs, err)
		}
		if err := c.guard(ctx, p, n); err != nil {
			errs = append(errs, err)
		}
	}
	for key := range c.noted {
		if node, _, _ := strings.Cut(key, "/"); p.feos[node].Metadata.Name == "" && !p.clash[node] {
			delete(c.noted, key)
		}
	}
	if err := c.cleanup(ctx, p); err != nil {
		errs = append(errs, err)
	}
	if err := c.remove(ctx, p); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// read reads what a pass acts on.
func (c *Controller) read(ctx context.Context) (*pass, error) {
	all, err := c.kube.ListNodes(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("list the nodes: %w", err)
	}
	feos, err := c.kube.ListNodes(ctx, c.cfg.NodeSelector)
	if err != nil {
		return nil, fmt.Errorf("list the FeOS nodes: %w", err)
	}
	fnodes, err := c.ferun.ListNodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list FeRun's nodes: %w", err)
	}
	guards, err := c.kube.ListPods(ctx, c.cfg.Namespace, LabelApp+"="+GuardApp)
	if err != nil {
		return nil, fmt.Errorf("list the guards: %w", err)
	}
	p := &pass{all: map[string]bool{}, feos: map[string]kube.Node{}, clash: map[string]bool{},
		ferun: map[string]FeRunNode{}, guards: map[string]kube.Pod{}, secretData: map[string][]byte{}}
	for _, n := range all {
		p.all[FeRunName(n.Metadata.Name)] = true
	}
	for _, n := range feos {
		name := FeRunName(n.Metadata.Name)
		if o, ok := p.feos[name]; ok {
			c.note(name, "name", "nodes "+o.Metadata.Name+" and "+n.Metadata.Name+" have one FeRun name; FeOS runs on neither", slog.LevelWarn)
			p.clash[name] = true
			continue
		}
		p.feos[name] = n
	}
	for name := range p.clash {
		delete(p.feos, name)
	}
	for _, n := range fnodes {
		p.ferun[n.Node] = n
	}
	for _, g := range guards {
		if g.Spec.NodeName != "" {
			p.guards[FeRunName(g.Spec.NodeName)] = g
		}
	}
	s, err := c.kube.GetSecret(ctx, c.cfg.Namespace, c.cfg.JoinSecret)
	switch {
	case kube.IsNotFound(err):
	case err != nil:
		return nil, fmt.Errorf("read the join secret: %w", err)
	default:
		p.secretExists = true
		for k, v := range s.Data {
			if len(v) > 0 {
				p.secretData[k] = v
			}
		}
	}
	return p, nil
}

// joinTokens lists FeRun's join tokens, once per pass.
func (c *Controller) joinTokens(ctx context.Context, p *pass) ([]JoinToken, error) {
	if p.tokensLoaded {
		return p.tokens, nil
	}
	ts, err := c.ferun.ListJoinTokens(ctx)
	if err != nil {
		return nil, fmt.Errorf("list the join tokens: %w", err)
	}
	p.tokens, p.tokensLoaded = ts, true
	return ts, nil
}

// liveToken reports whether FeRun has a join token pinned to node that is
// neither used nor expired.
func liveToken(tokens []JoinToken, node string, now time.Time) bool {
	for _, t := range tokens {
		if t.Node != node || t.UsedAt != "" {
			continue
		}
		if exp, err := time.Parse(time.RFC3339, t.ExpiresAt); err == nil && exp.After(now) {
			return true
		}
	}
	return false
}

// join gives a node that FeRun does not know, or knows as removed, a join
// token for its name in the join secret, and takes the key away once the
// node is ready. A token that expired unused is replaced. A node whose
// identity forked is left to an operator: two holders used its identity.
func (c *Controller) join(ctx context.Context, p *pass, name string) error {
	f, known := p.ferun[name]
	_, hasKey := p.secretData[name]
	switch {
	case !known || (f.Status == nodeRemoved && !f.IdentityForked):
		tokens, err := c.joinTokens(ctx, p)
		if err != nil {
			return err
		}
		if hasKey && liveToken(tokens, name, c.now()) {
			return nil
		}
		t, err := c.ferun.CreateJoinToken(ctx, name, c.cfg.JoinTokenTTL)
		if err != nil {
			return fmt.Errorf("make a join token for %s: %w", name, err)
		}
		if err := c.putKey(ctx, p, name, []byte(t.Token)); err != nil {
			if rerr := c.ferun.RevokeJoinToken(ctx, t.ID); rerr != nil {
				c.log.Warn("revoke a join token not handed out", "node", name, "id", t.ID, "err", rerr)
			}
			return fmt.Errorf("put the join token of %s into secret %s: %w", name, c.cfg.JoinSecret, err)
		}
		p.secretData[name] = []byte(t.Token)
		p.tokens = append(p.tokens, JoinToken{ID: t.ID, Node: name, ExpiresAt: t.ExpiresAt})
		c.log.Info("join token made", "node", name, "id", t.ID, "expiresAt", t.ExpiresAt, "replaced", hasKey)
	case f.Status == nodeReady && hasKey:
		if err := c.kube.PatchSecretData(ctx, c.cfg.Namespace, c.cfg.JoinSecret, map[string][]byte{name: nil}); err != nil {
			return fmt.Errorf("remove the join token of %s from secret %s: %w", name, c.cfg.JoinSecret, err)
		}
		delete(p.secretData, name)
		c.log.Info("node joined; its join token is removed", "node", name)
	}
	return nil
}

// putKey sets a key of the join secret, creating the secret if need be.
func (c *Controller) putKey(ctx context.Context, p *pass, key string, value []byte) error {
	if !p.secretExists {
		err := c.kube.CreateSecret(ctx, kube.Secret{APIVersion: "v1", Kind: "Secret", Type: "Opaque",
			Metadata: kube.ObjectMeta{Name: c.cfg.JoinSecret, Namespace: c.cfg.Namespace},
			Data:     map[string][]byte{key: value}})
		if err == nil {
			p.secretExists = true
			return nil
		}
		if !kube.IsAlreadyExists(err) {
			return err
		}
		p.secretExists = true
	}
	return c.kube.PatchSecretData(ctx, c.cfg.Namespace, c.cfg.JoinSecret, map[string][]byte{key: value})
}

// guard keeps a schedulable node's guard, and drains a cordoned node in
// FeRun, deleting its guard once FeRun has drained it, or does not know it,
// or has it down or removed: then nothing on the node waits for a drain.
// An uncordon undrains a drain the controller began.
func (c *Controller) guard(ctx context.Context, p *pass, n kube.Node) error {
	node, name := n.Metadata.Name, FeRunName(n.Metadata.Name)
	f, known := p.ferun[name]
	annotated := n.Metadata.Annotations[AnnotationDrain] == drainRequested
	g, hasGuard := p.guards[name]

	if !n.Spec.Unschedulable {
		if annotated {
			if known && f.Draining {
				if _, err := c.ferun.Undrain(ctx, name); err != nil && !isFeRunStatus(err, http.StatusNotFound) {
					return fmt.Errorf("undrain %s in FeRun: %w", name, err)
				}
				c.log.Info("node uncordoned; undrained in FeRun", "node", name)
			}
			if err := c.kube.PatchNodeAnnotations(ctx, node, map[string]*string{AnnotationDrain: nil}); err != nil {
				return fmt.Errorf("remove the drain annotation of %s: %w", name, err)
			}
		}
		delete(c.noted, name+"/drain")
		return c.ensureGuard(ctx, node, g, hasGuard)
	}

	release, why := false, ""
	switch {
	case !known:
		release, why = true, "FeRun does not know the node"
	case f.Status == nodeDown || f.Status == nodeRemoved:
		release, why = true, "the node is "+f.Status+" in FeRun"
	}
	if !release && !f.Draining {
		if !annotated {
			v := drainRequested
			if err := c.kube.PatchNodeAnnotations(ctx, node, map[string]*string{AnnotationDrain: &v}); err != nil {
				return fmt.Errorf("annotate %s for its drain: %w", name, err)
			}
		}
		nf, err := c.ferun.Drain(ctx, name)
		switch {
		case isFeRunStatus(err, http.StatusConflict) || isFeRunStatus(err, http.StatusNotFound):
			// Down or removed meanwhile, or unknown.
			release, why = true, err.Error()
		case err != nil:
			return fmt.Errorf("drain %s in FeRun: %w", name, err)
		default:
			f = nf
			c.log.Info("node cordoned; draining in FeRun", "node", name)
		}
	}
	if !release {
		switch {
		case f.Draining && f.StatusDescription == descDrained:
			release, why = true, "drained in FeRun"
		case strings.HasPrefix(f.StatusDescription, descCannotDrain):
			c.note(name, "drain", f.StatusDescription+"; the guard stays", slog.LevelWarn)
		}
	}
	if !release || !hasGuard || g.Metadata.DeletionTimestamp != "" {
		return nil
	}
	if err := c.kube.DeletePod(ctx, c.cfg.Namespace, g.Metadata.Name); err != nil {
		return fmt.Errorf("delete the guard of %s: %w", name, err)
	}
	c.log.Info("guard deleted; the node's drain goes on", "node", name, "reason", why)
	return nil
}

// ensureGuard creates a node's guard when it has none, and replaces one
// that ended.
func (c *Controller) ensureGuard(ctx context.Context, name string, g kube.Pod, has bool) error {
	if has {
		if g.Metadata.DeletionTimestamp != "" {
			return nil // it goes; the next pass makes a new one
		}
		if g.Status.Phase == kube.PodFailed || g.Status.Phase == kube.PodSucceeded {
			if err := c.kube.DeletePod(ctx, c.cfg.Namespace, g.Metadata.Name); err != nil {
				return fmt.Errorf("delete the ended guard of %s: %w", name, err)
			}
			c.log.Info("guard ended; deleted", "node", name, "phase", g.Status.Phase)
		}
		return nil
	}
	err := c.kube.CreatePod(ctx, c.guardPod(name))
	if kube.IsAlreadyExists(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("create the guard of %s: %w", name, err)
	}
	c.log.Info("guard created", "node", name)
	return nil
}

// GuardName is the name of a node's guard.
func GuardName(node string) string { return guardPrefix + node }

// guardPod is a node's guard: a pause container bound to the node, which
// tolerates every taint, so that only a drain's eviction (which the
// PodDisruptionBudget refuses) or the controller ends it.
func (c *Controller) guardPod(node string) kube.Pod {
	zero := int64(0)
	no, yes := false, true
	uid := int64(65535)
	return kube.Pod{
		APIVersion: "v1", Kind: "Pod",
		Metadata: kube.ObjectMeta{
			Name: GuardName(node), Namespace: c.cfg.Namespace,
			Labels: map[string]string{LabelApp: GuardApp, LabelNode: node},
		},
		Spec: kube.PodSpec{
			NodeName:                      node,
			PriorityClassName:             c.cfg.GuardPriorityClass,
			TerminationGracePeriodSeconds: &zero,
			AutomountServiceAccountToken:  &no,
			EnableServiceLinks:            &no,
			Tolerations:                   []kube.Toleration{{Operator: "Exists"}},
			Containers: []kube.Container{{
				Name:  "guard",
				Image: c.cfg.GuardImage,
				Resources: kube.ResourceRequirements{
					Requests: map[string]string{"cpu": "1m", "memory": "4Mi"},
				},
				SecurityContext: &kube.SecurityContext{
					RunAsNonRoot: &yes, RunAsUser: &uid, AllowPrivilegeEscalation: &no,
					ReadOnlyRootFilesystem: &yes, Capabilities: &kube.Capabilities{Drop: []string{"ALL"}},
				},
			}},
		},
	}
}

// cleanup deletes the guards of nodes that are gone or no longer FeOS
// nodes, and the join secret's keys of nodes that are gone.
func (c *Controller) cleanup(ctx context.Context, p *pass) error {
	var errs []error
	for _, node := range slices.Sorted(maps.Keys(p.guards)) {
		g := p.guards[node]
		if _, ok := p.feos[node]; ok || g.Metadata.DeletionTimestamp != "" {
			continue
		}
		if err := c.kube.DeletePod(ctx, c.cfg.Namespace, g.Metadata.Name); err != nil {
			errs = append(errs, fmt.Errorf("delete the guard of %s: %w", node, err))
			continue
		}
		c.log.Info("guard of a node that is gone or no FeOS node deleted", "node", node)
	}
	gone := map[string][]byte{}
	for key := range p.secretData {
		if !p.all[key] {
			gone[key] = nil
		}
	}
	if len(gone) > 0 {
		if err := c.kube.PatchSecretData(ctx, c.cfg.Namespace, c.cfg.JoinSecret, gone); err != nil {
			errs = append(errs, fmt.Errorf("remove the join tokens of nodes that are gone: %w", err))
		} else {
			for key := range gone {
				delete(p.secretData, key)
			}
			c.log.Info("join tokens of nodes that are gone removed", "nodes", slices.Sorted(maps.Keys(gone)))
		}
	}
	return errors.Join(errs...)
}

// remove removes from FeRun the nodes of this cluster (NodeNamePrefix)
// whose Kubernetes node is gone and that FeRun has down, or joined and
// never reporting, for longer than RemoveAfter. They are removed by force:
// what FeRun still has on them is on a machine that is gone.
func (c *Controller) remove(ctx context.Context, p *pass) error {
	now := c.now()
	for name := range c.goneSince {
		if _, ok := p.ferun[name]; !ok {
			delete(c.goneSince, name)
		}
	}
	var errs []error
	for _, name := range slices.Sorted(maps.Keys(p.ferun)) {
		f := p.ferun[name]
		if !strings.HasPrefix(name, c.cfg.NodeNamePrefix) || p.all[name] ||
			(f.Status != nodeDown && f.Status != nodeJoined) {
			delete(c.goneSince, name)
			continue
		}
		since, ok := c.goneSince[name]
		if !ok {
			since = now
			c.goneSince[name] = now
		}
		if now.Sub(since) < c.cfg.RemoveAfter {
			continue
		}
		if err := c.ferun.RemoveNode(ctx, name); err != nil && !isFeRunStatus(err, http.StatusNotFound) {
			errs = append(errs, fmt.Errorf("remove %s from FeRun: %w", name, err))
			continue
		}
		delete(c.goneSince, name)
		c.log.Info("node removed from FeRun: its Kubernetes node is gone", "node", name, "status", f.Status)
	}
	return errors.Join(errs...)
}

// note logs msg for a node and topic when it differs from what was logged
// last for them.
func (c *Controller) note(node, topic, msg string, level slog.Level) {
	key := node + "/" + topic
	if c.noted[key] == msg {
		return
	}
	c.noted[key] = msg
	c.log.Log(context.Background(), level, msg, "node", node)
}

// FeRunName is a Kubernetes node's name in FeRun: its first DNS label,
// which is all of it on Gardener, and "ip-10-0-1-2" of EKS's
// "ip-10-0-1-2.eu-west-1.compute.internal". The join secret's keys are
// these names; feos-node's run cuts its node's name the same way.
func FeRunName(node string) string {
	name, _, _ := strings.Cut(node, ".")
	return name
}

// isNodeName reports whether name is a DNS label, as FeRun's node names
// are: 1 to 63 lowercase letters, digits and "-", beginning and ending
// with a letter or a digit.
func isNodeName(name string) bool {
	if len(name) == 0 || len(name) > 63 || name[0] == '-' || name[len(name)-1] == '-' {
		return false
	}
	for i := 0; i < len(name); i++ {
		ch := name[i]
		if (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') && ch != '-' {
			return false
		}
	}
	return true
}
