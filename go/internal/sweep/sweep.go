// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package sweep is the leader's sweep of the object store (ADR 0065): it
// names what is live, every sandbox row's checkpoint head and every layer
// of the image catalog, and has one node per object store's disk daemon
// delete the rest once it is older than the daemon's grace (ADR 0011).
package sweep

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/config"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/leader"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
)

// LoopName is the sweep's leader loop.
const LoopName = "object-store-sweep"

// checkEvery is how often the loop looks whether a sweep is due: a new
// leader sweeps within it once a node is connected.
const checkEvery = 10 * time.Minute

// budget is the sweep's soft limit on the node; a sweep that runs out
// stops after its current pass, and the next one goes on.
const budget = 20 * time.Minute

// LiveSource adds to the live set what the sandbox rows and the catalog
// do not name: the backups of ADR 0064.
type LiveSource interface {
	LiveSet(ctx context.Context) (heads, layers []string, err error)
}

// Conns reaches the nodes (nodelink.Server).
type Conns interface {
	Conn(ctx context.Context, node string) (grpc.ClientConnInterface, error)
	Connected(node string) bool
}

// Config is the sweep's: its mode (config.Sweep*) and how often it runs.
type Config struct {
	Mode  string
	Every time.Duration
	Log   *slog.Logger
}

// Sweeper runs the sweep.
type Sweeper struct {
	cfg   Config
	db    *db.DB
	st    *store.Store
	conns Conns
	extra LiveSource
	log   *slog.Logger
	// liveSet reads the rows' live set of an object store (tests replace
	// it).
	liveSet func(ctx context.Context, objectStore string) (store.SweepLive, error)

	mu   sync.Mutex
	last map[string]time.Time // object store → last sweep on this instance
}

// New makes the sweeper; extra may be nil.
func New(cfg Config, d *db.DB, conns Conns, extra LiveSource) *Sweeper {
	s := &Sweeper{cfg: cfg, db: d, conns: conns, extra: extra,
		log: cfg.Log.With("component", "sweep"), last: map[string]time.Time{}}
	if d != nil {
		s.st = store.New(d)
	}
	s.liveSet = s.readLiveSet
	return s
}

// readLiveSet reads the rows' live set in one transaction.
func (s *Sweeper) readLiveSet(ctx context.Context, objectStore string) (store.SweepLive, error) {
	var live store.SweepLive
	err := s.st.InTx(ctx, func(tx *store.Store) error {
		var err error
		live, err = tx.SweepLiveSet(ctx, objectStore)
		return err
	})
	return live, err
}

// Loops returns the sweep's leader loop; none when it is off.
func (s *Sweeper) Loops() []leader.Loop {
	if s.cfg.Mode == config.SweepOff {
		return nil
	}
	return []leader.Loop{{Name: LoopName, Interval: checkEvery, Run: s.run}}
}

// run sweeps every object store whose last sweep on this instance is
// older than Every, through one of its ready nodes this instance is
// connected to.
func (s *Sweeper) run(ctx context.Context) error {
	nodes, err := s.st.ListNodes(ctx)
	if err != nil {
		return fmt.Errorf("sweep: nodes: %w", err)
	}
	byStore := map[string][]string{}
	for _, n := range nodes {
		if n.ObjectStoreID != "" && n.Status == model.NodeReady && s.conns.Connected(n.Name) {
			byStore[n.ObjectStoreID] = append(byStore[n.ObjectStoreID], n.Name)
		}
	}
	var errs []error
	for _, id := range slices.Sorted(maps.Keys(byStore)) {
		if !s.due(id) {
			continue
		}
		if err := s.sweepStore(ctx, id, byStore[id]); err != nil {
			errs = append(errs, fmt.Errorf("sweep of object store %s: %w", id, err))
			continue
		}
		s.mu.Lock()
		s.last[id] = time.Now()
		s.mu.Unlock()
	}
	return errors.Join(errs...)
}

func (s *Sweeper) due(store string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	last, ok := s.last[store]
	return !ok || time.Since(last) >= s.cfg.Every
}

// sweepStore reads the live set in one transaction and has the first of
// nodes that takes the call sweep the store. An empty live set sweeps
// nothing. A node that does not know the call yet is passed over.
func (s *Sweeper) sweepStore(ctx context.Context, objectStore string, nodes []string) error {
	live, err := s.liveSet(ctx, objectStore)
	if err != nil {
		return err
	}
	if s.extra != nil {
		heads, layers, err := s.extra.LiveSet(ctx)
		if err != nil {
			return fmt.Errorf("live set of the backups: %w", err)
		}
		live.Heads = append(live.Heads, heads...)
		live.Layers = append(live.Layers, layers...)
	}
	if len(live.Heads) == 0 {
		s.log.Warn("sweep skipped: the live set names no head", "objectStore", objectStore)
		return nil
	}
	dry := s.cfg.Mode != config.SweepOn
	for _, node := range nodes {
		cctx, cancel := context.WithTimeout(ctx, budget+5*time.Minute)
		rep, err := s.call(cctx, node, live, dry)
		cancel()
		if status.Code(err) == codes.Unimplemented {
			s.log.Info("sweep: the node's feosd does not sweep yet", "node", node, "objectStore", objectStore)
			continue
		}
		if err != nil {
			return fmt.Errorf("node %s: %w", node, err)
		}
		s.log.Info("object store swept", "objectStore", objectStore, "node", node, "dryRun", dry,
			"liveHeads", len(live.Heads), "liveLayers", len(live.Layers),
			"snapshotObjectsSeen", rep.GetSnapshotObjectsSeen(), "snapshotObjectsDeleted", rep.GetSnapshotObjectsDeleted(),
			"layersSeen", rep.GetLayersSeen(), "layersDeleted", rep.GetLayersDeleted(),
			"chunksSeen", rep.GetChunksSeen(), "chunksDeleted", rep.GetChunksDeleted(),
			"bytesDeleted", rep.GetBytesDeleted(), "keysDeleted", rep.GetKeysDeleted(),
			"budgetExhausted", rep.GetBudgetExhausted(), "sampleKeys", sample(rep.GetSampleKeys(), 20))
		return nil
	}
	return nil
}

func (s *Sweeper) call(ctx context.Context, node string, live store.SweepLive, dry bool) (*podv1.SweepObjectStoreResponse, error) {
	cc, err := s.conns.Conn(ctx, node)
	if err != nil {
		return nil, err
	}
	return podv1.NewPodServiceClient(cc).SweepObjectStore(ctx, &podv1.SweepObjectStoreRequest{
		LiveHeads: live.Heads, LiveLayers: live.Layers, DryRun: dry, BudgetSeconds: uint64(budget / time.Second)})
}

// sample is the first n of keys, for the log.
func sample(keys []string, n int) []string {
	return keys[:min(n, len(keys))]
}
