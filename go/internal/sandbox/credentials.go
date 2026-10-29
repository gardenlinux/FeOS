// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
)

// A sandbox's image credentials (ADR 0025) come with its create and are
// kept sealed beside its row (sandbox_image_credentials). The engine asks
// the image's registry with them before every create of the sandbox's
// pod, resolves its tag with them, and gets the guest's pull token with
// them; the credentials never reach a node, and the token only feosd's
// memory and the guest's pull.

// errNoSealer is a sandbox with image credentials on an instance without
// --secret-key-file.
var errNoSealer = errors.New("the sandbox has image credentials, and this instance has no --secret-key-file to open them")

// registryAuth is what the row's creates bring for its image: an
// environment's instance its template's login, its secrets at their
// latest versions; a sandbox its sealed credentials, opened; none when it
// has none.
func (e *Engine) registryAuth(ctx context.Context, row store.Sandbox) (model.RegistryCredentials, error) {
	if t := row.PodTemplate; t != nil {
		if t.ImageCredentials == nil {
			return model.RegistryCredentials{}, nil
		}
		resolve := e.secretResolver()
		if resolve == nil {
			return model.RegistryCredentials{}, errNoResolver
		}
		auth, err := t.ImageCredentials.Resolve(func(name string) (string, error) {
			return resolve(ctx, row.EnvironmentID, name, 0)
		})
		if err != nil {
			return model.RegistryCredentials{}, fmt.Errorf("the image credentials: %w", err)
		}
		return auth, nil
	}
	sealed, err := e.st.ImageCredentials(ctx, row.ID)
	if err != nil || sealed == nil {
		return model.RegistryCredentials{}, err
	}
	if e.sealer == nil {
		return model.RegistryCredentials{}, errNoSealer
	}
	plain, err := e.sealer.Open(sealed, store.ImageCredentialsAAD(row.ID))
	if err != nil {
		return model.RegistryCredentials{}, fmt.Errorf("open the sandbox's image credentials: %w", err)
	}
	var auth model.RegistryCredentials
	if err := json.Unmarshal(plain, &auth); err != nil {
		return model.RegistryCredentials{}, fmt.Errorf("the sandbox's image credentials: %w", err)
	}
	return auth, nil
}

// startToken is the guest's pull token for a start of the row's pod on its
// disk, which has the image unless its first pull did not end: asked for
// when the sandbox has credentials, and none when that fails, since the
// start may need none.
func (e *Engine) startToken(ctx context.Context, row store.Sandbox) string {
	auth, err := e.registryAuth(ctx, row)
	if err != nil || auth.Anonymous() || row.ImageRef == "" {
		return ""
	}
	token, err := e.imagePullToken()(ctx, row.ImageRef, auth)
	if err != nil {
		e.rl.warn("pull-token", "no pull token for a start", "sandbox", row.ID, "err", err)
		return ""
	}
	return token
}
