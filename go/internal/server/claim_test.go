// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// A fixed --instance-id, as a StatefulSet gives, restarted after it was
// killed during its stop: the new run waits until the old row is older
// than the TTL, then runs live (not stopping) and releases the op the
// killed run left, which nothing would finish otherwise. A second process
// under the same ID while the first runs is refused.
func TestFixedInstanceIDRestart(t *testing.T) {
	url := dbtest.NewDatabase(t)
	caDir := testCA(t)
	cfg := testConfig(url, caDir)
	cfg.InstanceID = "ferund-0"
	i, err := New(context.Background(), cfg, dbtest.Logger(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st := i.Store
	// The killed run: its row stopping and written 13 s ago, an op of its
	// own in flight.
	if _, _, err := st.CreateTenant(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertNode(ctx, "n1"); err != nil {
		t.Fatal(err)
	}
	sb, _, err := st.CreateSandbox(ctx, store.NewSandbox{ID: podid.Mint(), Owner: "acme", Image: "alpine", VCPUs: 2, MemoryMiB: 512,
		EnvironmentID: dbtest.Environment(t, st.Q(), "acme", "box")})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO instances (id, started_at, last_seen, stopping)
			VALUES ('ferund-0', now() - interval '1 hour', now() - interval '13 seconds', now() - interval '20 seconds')`,
		`UPDATE sandboxes SET node = 'n1', client_state = 'running', op = 'pause', op_id = gen_random_uuid(),
			op_owner = 'ferund-0', op_target = 'paused', op_deadline = now() + interval '1 hour' WHERE id = '` + sb.ID.String() + `'`,
	} {
		if _, err := i.DB.Pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	t0 := time.Now()
	started(t, i)
	waitServing(t, i)
	if took := time.Since(t0); took < time.Second {
		t.Fatalf("served after %v, before the old row aged past the TTL", took)
	}
	in, err := st.GetInstance(ctx, "ferund-0")
	if err != nil || !in.Live || !in.Stopping.IsZero() || time.Since(in.StartedAt) > time.Minute {
		t.Fatalf("the row of the new run: %+v %v", in, err)
	}
	r, err := st.GetSandbox(ctx, sb.ID)
	if err != nil || r.Op != "" || r.OpOwner != "" || r.ClientState != model.StateUnknown {
		t.Fatalf("the killed run's op: %+v %v", r, err)
	}

	// A second process under the same ID is refused while the first runs,
	// and its listeners are released.
	j, err := New(context.Background(), cfg, dbtest.Logger(t))
	if err != nil {
		t.Fatal(err)
	}
	httpAddr := j.HTTPAddr()
	done := make(chan error, 1)
	go func() { done <- j.Run(context.Background()) }()
	select {
	case err = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("a second process under the same ID was not refused")
	}
	var inUse *store.InstanceInUseError
	if !errors.As(err, &inUse) || !strings.Contains(err.Error(), "another process runs under this --instance-id") {
		t.Fatalf("second process: %v", err)
	}
	ln, err := net.Listen("tcp", httpAddr)
	if err != nil {
		t.Fatalf("the refused instance still holds %s: %v", httpAddr, err)
	}
	ln.Close()
	if in2, err := st.GetInstance(ctx, "ferund-0"); err != nil || !in2.Live || !in2.StartedAt.Equal(in.StartedAt) {
		t.Fatalf("the refused start touched the first run's row: %+v %v", in2, err)
	}

	// A start that is stopped while it waits for the row returns nil.
	k, err := New(context.Background(), cfg, dbtest.Logger(t))
	if err != nil {
		t.Fatal(err)
	}
	kctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := k.Run(kctx); err != nil {
		t.Fatalf("a start stopped while waiting: %v", err)
	}
}
