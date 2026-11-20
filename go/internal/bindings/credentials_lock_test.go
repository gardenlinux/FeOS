// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package bindings

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/store"
)

// lockWait is how long a call must stay blocked on a credential's lock
// for a test to count it as waiting.
const lockWait = 300 * time.Millisecond

// A sandbox that names a credential waits for the credential's lock, and
// then finds it as it is: one a delete under the lock took is 422
// no_credential, not a name of nothing.
func TestSandboxCredentialsWaitForTheCredentialsLock(t *testing.T) {
	r := newRig(t)
	if code, raw := r.tenantCall("PUT", credPath("box", "erp"), credentialBody("Bearer box-token", 0)); code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, raw)
	}
	done := make(chan error, 1)
	err := r.st.InTx(bg, func(tx *store.Store) error {
		if err := tx.LockEnvCredential(bg, r.box, "erp"); err != nil {
			return err
		}
		go func() { done <- r.svc.SetSandboxCredentials(bg, "acme", r.box, r.sb1, []string{"erp"}) }()
		select {
		case err := <-done:
			return fmt.Errorf("SetSandboxCredentials did not wait for the lock: %v", err)
		case <-time.After(lockWait):
		}
		_, err := tx.DeleteEnvCredential(bg, r.box, "erp")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if code, c := apiCode(<-done); code != http.StatusUnprocessableEntity || c != codeNoCredential {
		t.Fatalf("after the delete: %d %s, want 422 %s", code, c, codeNoCredential)
	}
	if names, err := r.st.SandboxCredentialNames(bg, r.sb1); err != nil || len(names) != 0 {
		t.Fatalf("the sandbox names %v %v", names, err)
	}
}

// A delete waits for the credential's lock and reads its uses under it:
// a sandbox that came to name it meanwhile makes it 409.
func TestDeleteCredentialWaitsForTheCredentialsLock(t *testing.T) {
	r := newRig(t)
	if code, raw := r.tenantCall("PUT", credPath("box", "erp"), credentialBody("Bearer box-token", 0)); code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, raw)
	}
	type answer struct {
		code int
		raw  string
	}
	done := make(chan answer, 1)
	err := r.st.InTx(bg, func(tx *store.Store) error {
		if err := tx.LockEnvCredential(bg, r.box, "erp"); err != nil {
			return err
		}
		go func() {
			code, raw := r.tenantCall("DELETE", credPath("box", "erp"), "")
			done <- answer{code, raw}
		}()
		select {
		case a := <-done:
			return fmt.Errorf("the delete did not wait for the lock: %d %s", a.code, a.raw)
		case <-time.After(lockWait):
		}
		return tx.SetSandboxCredentialNames(bg, r.sb1, []string{"erp"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if a := <-done; a.code != http.StatusConflict {
		t.Fatalf("the delete of a credential a sandbox came to name: %d %s", a.code, a.raw)
	}
	if _, err := r.st.EnvCredential(bg, r.box, "erp", 0); err != nil {
		t.Fatalf("the credential is gone: %v", err)
	}
}
