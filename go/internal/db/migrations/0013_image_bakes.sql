-- SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
-- SPDX-License-Identifier: Apache-2.0

-- Baked layers of images (ADR 0025): a disk layer on a guest's blank data
-- volume that holds an image pulled into the guest's image store, sealed
-- before any container ran, and in the object store. A create looks its
-- image up only after the image's registry said yes to it, and its node
-- starts the pod's disk on the newest layer made for the guest it boots.
-- The key is the image's source and manifest digest
-- (registry/repository@sha256:…), never a digest alone, with the guest's
-- version and its blank data volume's layer; a second bake of the same
-- (two nodes at once, or after the store lost a layer) is a row of its own,
-- and a node passes over a layer the store does not have. Additive: an
-- instance of the previous version neither reads nor writes it.
CREATE TABLE image_bakes (
  image text COLLATE "C" NOT NULL,
  guest_version text COLLATE "C" NOT NULL,
  l0 text COLLATE "C" NOT NULL,
  layer text COLLATE "C" NOT NULL,
  node text NOT NULL,
  baked_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (image, guest_version, l0, layer),
  CHECK (image ~ '@sha256:[0-9a-f]{64}$'),
  CHECK (guest_version <> '' AND l0 <> '' AND layer <> '')
);

CREATE INDEX image_bakes_newest ON image_bakes (image, baked_at DESC);
