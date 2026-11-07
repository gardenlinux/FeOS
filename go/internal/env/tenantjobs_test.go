// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/env/ctl"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
)

// A job's create gets its defaults, and a 400 for what it gets wrong.
func TestTenantJobSpec(t *testing.T) {
	rs, err := tenantJobSpec(CreateTenantJobRequest{Environment: "builds", Image: "builder:1", ResultPath: "/run/out/digest"})
	if err != nil || rs.VCPUs != spec.DefaultVCPUs || rs.MemoryMiB != spec.DefaultMemoryMib ||
		rs.Deadline != 10*time.Minute || rs.ResultPath != "/run/out/digest" || !slices.Equal(rs.SecurityGroups, []string{"default"}) {
		t.Fatalf("defaults %+v %v", rs, err)
	}
	if rs, err := tenantJobSpec(CreateTenantJobRequest{Environment: "builds", Image: "b", Vcpus: 2, MemoryMib: 2048, Deadline: "20m"}); err != nil ||
		rs.VCPUs != 2 || rs.MemoryMiB != 2048 || rs.Deadline != 20*time.Minute {
		t.Fatalf("sized %+v %v", rs, err)
	}
	for _, d := range []int{0, 1, 400} {
		if rs, err := tenantJobSpec(CreateTenantJobRequest{Environment: "builds", Image: "b", LogRetentionDays: d}); err != nil || rs.LogRetentionDays != d {
			t.Fatalf("logRetentionDays %d: %+v %v", d, rs, err)
		}
	}
	groups := func(names ...string) *[]string { return &names }
	if rs, err := tenantJobSpec(CreateTenantJobRequest{Environment: "builds", Image: "b",
		SecurityGroups: groups("web", "db")}); err != nil || !slices.Equal(rs.SecurityGroups, []string{"db", "web"}) {
		t.Fatalf("groups %+v %v", rs, err)
	}
	for _, bad := range []CreateTenantJobRequest{
		{},
		{Image: "b"},
		{Environment: "e", Image: "b", SecurityGroups: groups()},
		{Environment: "e", Image: "b", SecurityGroups: groups("a", "b", "c", "d", "e", "f")},
		{Environment: "e", Image: "b", SecurityGroups: groups("Web")},
		{Environment: "e", Image: "b", SecurityGroups: groups("web", "web")},
		{Environment: "e", Image: "b", Env: map[string]string{"A-B": "x"}},
		{Environment: "e", Image: "b", Secrets: map[string]string{"imageCredentials.username": "x"}},
		{Environment: "e", Image: "b", Env: map[string]string{"T": "x"}, Secrets: map[string]string{"T": "y"}},
		{Environment: "e", Image: "b", ImageCredentials: &model.RegistryCredentials{Username: "u"}},
		{Environment: "e", Image: "b", Vcpus: 99},
		{Environment: "e", Image: "b", MemoryMib: 1},
		{Environment: "e", Image: "b", Deadline: "3h"},
		{Environment: "e", Image: "b", Deadline: "soon"},
		{Environment: "e", Image: "b", ResultPath: "out"},
		{Environment: "e", Image: "b", ResultPath: "/dev/termination-log"},
		{Environment: "e", Image: "b", ResultPath: "/run/../etc/x"},
		{Environment: "e", Image: "b", Labels: map[string]string{"": "x"}},
		{Environment: "e", Image: "b", LogRetentionDays: -1},
		{Environment: "e", Image: "b", LogRetentionDays: 401},
	} {
		_, err := tenantJobSpec(bad)
		var ae *apierr.Error
		if !errors.As(err, &ae) || ae.Status != http.StatusBadRequest {
			t.Errorf("%+v: %v, want a 400", bad, err)
		}
	}
}

// A create's hash for its key leaves out the values of its secrets and
// image credentials, which a caller mints anew for a repeat, and keeps
// everything else.
func TestTenantJobRequestHash(t *testing.T) {
	a := CreateTenantJobRequest{Image: "b", Env: map[string]string{"C": "1"}, Secrets: map[string]string{"T": "one"},
		ImageCredentials: &model.RegistryCredentials{Username: "u", Password: "p1"}}
	b := a
	b.Secrets = map[string]string{"T": "two"}
	b.ImageCredentials = &model.RegistryCredentials{Username: "u", Password: "p2"}
	if requestHash(a) != requestHash(b) {
		t.Error("new secret values change the hash")
	}
	for _, c := range []CreateTenantJobRequest{
		{Image: "b", Env: map[string]string{"C": "2"}, Secrets: a.Secrets, ImageCredentials: a.ImageCredentials},
		{Image: "b", Env: a.Env, Secrets: map[string]string{"U": "one"}, ImageCredentials: a.ImageCredentials},
		{Image: "b", Env: a.Env, Secrets: a.Secrets},
	} {
		if requestHash(c) == requestHash(a) {
			t.Errorf("%+v hashes as %+v", c, a)
		}
	}
}

// The controller's answers of the jobs map to their statuses.
func TestTenantJobErrors(t *testing.T) {
	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{ctl.ErrJobKeyConflict, http.StatusUnprocessableEntity, CodeJobKeyConflict},
		{fmt.Errorf("image x: denied: %w", ctl.ErrJobImage), http.StatusUnprocessableEntity, ""},
		{ctl.ErrNoObject, http.StatusNotFound, ""},
		{errors.New("secrets need --secret-key-file"), http.StatusServiceUnavailable, ""},
		{errors.New("the tenant is being deleted"), http.StatusConflict, ""},
	} {
		var ae *apierr.Error
		if err := tenantJobError(c.err); !errors.As(err, &ae) || ae.Status != c.status || ae.Code != c.code {
			t.Errorf("%v: %v", c.err, err)
		}
	}
	if tenantJobError(nil) != nil {
		t.Error("nil is an error")
	}
}
