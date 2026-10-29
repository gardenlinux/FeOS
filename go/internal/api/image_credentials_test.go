// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/seal"
	"github.com/gardenlinux/feos/go/internal/store"
)

// A sandbox's image credentials (ADR 0025): taken at its create, sealed
// for it alone and never returned, replaced and removed by their route;
// refused half given, and without a key to seal them.
func TestSandboxImageCredentials(t *testing.T) {
	t.Parallel()
	key := sha256.Sum256([]byte(t.Name()))
	sl, err := seal.New(key[:])
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, envOptions{sealer: sl})
	alice := model.RegistryCredentials{Username: "alice", Password: "s3cret"}
	w := e.do("POST", "/api/v1/sandboxes", bearerOf(e.tenantToken),
		map[string]any{"image": "ghcr.io/acme/app:1", "imageCredentials": alice})
	expect(t, w, http.StatusCreated, "")
	if bytes.Contains(w.Body.Bytes(), []byte("s3cret")) || bytes.Contains(w.Body.Bytes(), []byte("imageCredentials")) {
		t.Fatalf("the answer shows the credentials: %s", w.Body)
	}
	var created struct {
		Status struct {
			ID string `json:"sandboxId"`
		} `json:"status"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	id, err := podid.Parse(created.Status.ID)
	if err != nil {
		t.Fatalf("sandbox ID %q: %v", created.Status.ID, err)
	}
	opened := func() model.RegistryCredentials {
		t.Helper()
		sealed, err := e.st.ImageCredentials(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if sealed == nil {
			return model.RegistryCredentials{}
		}
		if bytes.Contains(sealed, []byte("s3cret")) {
			t.Fatal("kept in the clear")
		}
		plain, err := sl.Open(sealed, store.ImageCredentialsAAD(id))
		if err != nil {
			t.Fatal(err)
		}
		var c model.RegistryCredentials
		if err := json.Unmarshal(plain, &c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	if got := opened(); got != alice {
		t.Fatalf("kept %+v", got)
	}
	w = e.do("GET", "/api/v1/sandboxes/"+id.String(), bearerOf(e.tenantToken), nil)
	expect(t, w, http.StatusOK, "")
	if bytes.Contains(w.Body.Bytes(), []byte("s3cret")) || bytes.Contains(w.Body.Bytes(), []byte("alice")) {
		t.Fatalf("a read shows the credentials: %s", w.Body)
	}

	bob := model.RegistryCredentials{Username: "bob", Password: "rotated"}
	expect(t, e.do("PUT", "/api/v1/sandboxes/"+id.String()+"/image-credentials", bearerOf(e.tenantToken), bob),
		http.StatusNoContent, "")
	if got := opened(); got != bob {
		t.Fatalf("after the PUT: %+v", got)
	}
	expect(t, e.do("PUT", "/api/v1/sandboxes/"+id.String()+"/image-credentials", bearerOf(e.tenantToken),
		map[string]string{"username": "bob"}), http.StatusBadRequest, errBody("imageCredentials: image credentials need a username and a password"))
	expect(t, e.do("DELETE", "/api/v1/sandboxes/"+id.String()+"/image-credentials", bearerOf(e.tenantToken), nil),
		http.StatusNoContent, "")
	if got := opened(); !got.Anonymous() {
		t.Fatalf("after the DELETE: %+v", got)
	}
	expect(t, e.do("POST", "/api/v1/sandboxes", bearerOf(e.tenantToken),
		map[string]any{"imageCredentials": map[string]string{"password": "x"}}), http.StatusBadRequest, errBody("imageCredentials: image credentials need a username and a password"))

	// Without a key, credentials are refused, and a create without them
	// works as before.
	plain := newEnv(t, envOptions{})
	expect(t, plain.do("POST", "/api/v1/sandboxes", bearerOf(plain.tenantToken),
		map[string]any{"imageCredentials": alice}), http.StatusServiceUnavailable,
		errBody("image credentials need --secret-key-file on the control plane"))
	expect(t, plain.do("POST", "/api/v1/sandboxes", bearerOf(plain.tenantToken), map[string]any{}), http.StatusCreated, "")
}
