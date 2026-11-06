// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Pins (ADR 0032, "Only what the person changed changes"): a stored spec
// has every image pinned to a digest, with imageAsWritten, and every
// referenced secret to a version; a draft keeps them. A plan answers the
// pins it resolved, images: {"<as written>": "<registry>/<repo>@sha256:…"},
// and the builder writes them into the draft before it applies, so that
// what the plan showed is what runs. A ferund that does not answer images
// yet pins at apply, as before. Pure.

import { isObject, objectsOf, setObject } from './draft.js';

const DIGEST = /@sha256:[0-9a-f]{64}$/;

export function isDigestRef(image) {
  return DIGEST.test(String(image || ''));
}

// digestOf is an image reference's "sha256:…", '' without one.
export function digestOf(image) {
  const m = /@(sha256:[0-9a-f]{64})$/.exec(String(image || ''));
  return m ? m[1] : '';
}

// imageView is how the form shows an object's image: as written, and its
// digest when it is pinned.
export function imageView(o) {
  if (!isObject(o)) return { written: '', digest: '' };
  const image = String(o.image || '');
  if (isDigestRef(image)) return { written: o.imageAsWritten || image, digest: digestOf(image) };
  return { written: image, digest: '' };
}

// setImage is o with the image as the person wrote it: the pin goes, and
// the next plan resolves it.
export function setImage(o, written) {
  const out = { ...o, image: written };
  delete out.imageAsWritten;
  if (!written) delete out.image;
  return out;
}

// checkNewer puts a pinned image's as written back ("Check for a newer
// image"), which the plan resolves again.
export function checkNewer(o) {
  if (!isObject(o) || !isDigestRef(o.image) || !o.imageAsWritten) return o;
  return setImage(o, o.imageAsWritten);
}

// imageObjects are the objects that run an image: image services and
// jobs.
export function imageObjects(spec) {
  const out = [];
  for (const key of ['services', 'jobs']) {
    for (const [name, o] of objectsOf(spec, key)) {
      if (!isObject(o) || (key === 'services' && o.runtime === 'attached') || !o.image) continue;
      out.push({ key, name, image: String(o.image) });
    }
  }
  return out;
}

// unpinned are the images spec names as written, not by digest, sorted.
export function unpinned(spec) {
  return [...new Set(imageObjects(spec).filter((x) => !isDigestRef(x.image)).map((x) => x.image))].sort();
}

// applyPins writes a plan's images into spec: {spec, unresolved,
// supported}. supported is false when the plan answered no images (an
// older ferund), and spec stays as it is; unresolved are the images as
// written the plan did not pin, which keep apply from running.
export function applyPins(spec, images) {
  if (!isObject(images)) return { spec, unresolved: [], supported: false };
  let out = spec;
  const unresolved = new Set();
  for (const { key, name, image } of imageObjects(spec)) {
    if (isDigestRef(image)) continue;
    const pin = images[image];
    if (typeof pin === 'string' && isDigestRef(pin)) {
      const o = spec[key][name];
      out = setObject(out, key, name, { ...o, image: pin, imageAsWritten: image });
    } else {
      unresolved.add(image);
    }
  }
  return { spec: out, unresolved: [...unresolved].sort(), supported: true };
}

// secretPins are the secrets a draft knows with their pins against the
// environment's list (GET …/secrets): {name, pinned (0: the latest at
// apply), latest (0: the environment has none), newer, missing}.
// pending names those whose values wait in memory (a new environment).
export function secretPins(spec, names, list, pending = new Set()) {
  const latest = new Map((list || []).map((s) => [s.name, s.version]));
  return names.map((name) => {
    const ref = spec && isObject(spec.secrets) && isObject(spec.secrets[name]) ? spec.secrets[name] : null;
    const pinned = ref && Number.isInteger(ref.version) ? ref.version : 0;
    const l = latest.get(name) || 0;
    return { name, pinned, latest: l, newer: pinned > 0 && l > pinned, missing: !l && !pending.has(name) };
  });
}

// pinSecret pins secret name to version (0: the latest at apply).
export function pinSecret(spec, name, version) {
  return setObject(spec, 'secrets', name, version > 0 ? { version } : {});
}
