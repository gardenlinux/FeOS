-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- A sandbox's credentials for its image's registry (ADR 0025), sealed
-- under --secret-key-file's key with the AAD "sandbox_image_credentials|
-- <sandbox id>": the engine asks the registry with them before every create
-- of the sandbox's pod and gets the guest's pull token with them. A row of
-- its own, so that the sandbox's row never carries them; it goes with the
-- sandbox. Additive: an instance of the previous version neither reads nor
-- writes it.
CREATE TABLE sandbox_image_credentials (
  sandbox_id text COLLATE "C" PRIMARY KEY REFERENCES sandboxes(id) ON DELETE CASCADE,
  sealed bytea NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now()
);
