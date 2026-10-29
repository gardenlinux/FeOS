// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/seal"
	"github.com/gardenlinux/feos/go/internal/store"
)

// A sandbox's image credentials (ADR 0025): the question before its create
// is asked with them, and its create carries the guest's pull token, asked
// for with them; a sandbox without them asks anonymously and sends none.
// Credentials sealed for another sandbox do not open, and an instance
// without a key does not start a sandbox that has them.
func TestSandboxImageCredentials(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	e := h.eng()
	key := sha256.Sum256([]byte(t.Name()))
	sl, err := seal.New(key[:])
	if err != nil {
		t.Fatal(err)
	}
	e.sealer = sl
	var mu sync.Mutex
	var asked []model.RegistryCredentials
	var creates []*podv1.CreatePodRequest
	e.checkImage = func(_ context.Context, _ string, auth model.RegistryCredentials) error {
		mu.Lock()
		defer mu.Unlock()
		asked = append(asked, auth)
		return nil
	}
	e.pullToken = func(_ context.Context, _ string, auth model.RegistryCredentials) (string, error) {
		if auth.Anonymous() {
			return "", nil
		}
		return "token-of-" + auth.Username, nil
	}
	h.node("n1").Script().SetHook(podv1.PodService_CreatePod_FullMethodName, func(_ context.Context, _ string, req any) error {
		if r, ok := req.(*podv1.CreatePodRequest); ok {
			mu.Lock()
			creates = append(creates, proto.Clone(r).(*podv1.CreatePodRequest))
			mu.Unlock()
		}
		return nil
	})
	alice := model.RegistryCredentials{Username: "alice", Password: "s3cret"}
	keep := func(id podid.ID, aad podid.ID) {
		t.Helper()
		plain, err := json.Marshal(alice)
		if err != nil {
			t.Fatal(err)
		}
		sealed, err := sl.Seal(plain, store.ImageCredentialsAAD(aad))
		if err != nil {
			t.Fatal(err)
		}
		if err := h.st.SetImageCredentials(bg, id, sealed); err != nil {
			t.Fatal(err)
		}
	}
	last := func() (model.RegistryCredentials, string) {
		mu.Lock()
		defer mu.Unlock()
		return asked[len(asked)-1], creates[len(creates)-1].GetPullToken()
	}

	withCreds := h.sandbox(512)
	keep(withCreds.ID, withCreds.ID)
	h.ensure(withCreds.ID)
	if auth, token := last(); auth != alice || token != "token-of-alice" {
		t.Fatalf("asked with %+v, pull token %q", auth, token)
	}

	plain := h.sandbox(512)
	h.ensure(plain.ID)
	if auth, token := last(); !auth.Anonymous() || token != "" {
		t.Fatalf("anonymous sandbox asked with %+v, pull token %q", auth, token)
	}

	ensureErr := func(id podid.ID) error {
		ctx, cancel := context.WithTimeout(bg, 10*time.Second)
		defer cancel()
		_, err := e.Ensure(ctx, id)
		return err
	}
	stolen := h.sandbox(512)
	keep(stolen.ID, withCreds.ID)
	wantErr(t, ensureErr(stolen.ID), nil, "open the sandbox's image credentials")

	e.sealer = nil
	locked := h.sandbox(512)
	keep(locked.ID, locked.ID)
	wantErr(t, ensureErr(locked.ID), nil, errNoSealer.Error())
}
