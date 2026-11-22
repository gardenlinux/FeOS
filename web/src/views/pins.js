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
import { httpSecretNames } from './refs.js';

// isDigestRef says whether an image is a digest reference, as ferund
// says it (an "@sha256:" in it): the plan answers no pin for one, and
// the builder leaves its image and imageAsWritten alone.
export function isDigestRef(image) {
  return String(image || '').includes('@sha256:');
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

// expectationOf is what a pin says its code expects of the secret (ADR
// 0058): {type, mode, forwardedHost}, each only when the pin writes it;
// {} for none.
export function expectationOf(ref) {
  const out = {};
  if (!isObject(ref)) return out;
  if (ref.type === 'value' || ref.type === 'http') out.type = ref.type;
  if (ref.mode === 'name' || ref.mode === 'intercept') out.mode = ref.mode;
  if (typeof ref.forwardedHost === 'boolean') out.forwardedHost = ref.forwardedHost;
  return out;
}

// expectationWords say an expectation for the secret's row: "a value",
// "an HTTP secret in the intercept mode", "… in the name mode, the host
// forwarded"; '' for none.
export function expectationWords(e) {
  if (!e || Object.keys(e).length === 0) return '';
  if (e.type === 'value') return 'a value';
  let s = 'an HTTP secret';
  if (e.mode) s += ` in the ${e.mode} mode`;
  if (typeof e.forwardedHost === 'boolean') s += e.forwardedHost ? ', the host forwarded' : ', the host not forwarded';
  return s;
}

// newSecretQuery is the query of the new HTTP secret's page that offers
// the secret as the spec expects it: its name, mode and forwarded host.
export function newSecretQuery(name, e) {
  const q = new URLSearchParams();
  q.set('name', name);
  if (e && e.mode) q.set('mode', e.mode);
  if (e && typeof e.forwardedHost === 'boolean') q.set('forwardedHost', e.forwardedHost ? '1' : '0');
  return '?' + q.toString();
}

// offeredSecret is what the new HTTP secret's page takes from its query
// (newSecretQuery): {name, mode, forwardedHost}, each only when given and
// well formed; the intercept mode forwards no host.
export function offeredSecret(search) {
  const q = new URLSearchParams(search || '');
  const out = {};
  const name = q.get('name');
  if (name && /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/.test(name)) out.name = name;
  const mode = q.get('mode');
  if (mode === 'name' || mode === 'intercept') out.mode = mode;
  const fh = q.get('forwardedHost');
  if ((fh === '1' || fh === '0') && out.mode !== 'intercept') out.forwardedHost = fh === '1';
  return out;
}

// secretPins are the secrets a draft knows with their pins against the
// environment's list (GET …/secrets, both kinds): {name, type (http or
// value, ADR 0055: the list's, else the pin's written type, the draft's
// reference with a key), pinned (0: the latest at apply), latest (0: the
// environment has none), newer, missing, expect (what the spec's code
// expects of it, ADR 0058: expectationOf)}. pending names those whose
// values wait in memory (a new environment).
export function secretPins(spec, names, list, pending = new Set()) {
  const latest = new Map((list || []).map((s) => [s.name, s.version]));
  const types = new Map((list || []).map((s) => [s.name, s.type === 'http' ? 'http' : 'value']));
  const http = new Set(httpSecretNames(spec));
  return names.map((name) => {
    const ref = spec && isObject(spec.secrets) && isObject(spec.secrets[name]) ? spec.secrets[name] : null;
    const pinned = ref && Number.isInteger(ref.version) ? ref.version : 0;
    const l = latest.get(name) || 0;
    const expect = expectationOf(ref);
    const type = types.get(name) || expect.type || (http.has(name) ? 'http' : 'value');
    return { name, type, pinned, latest: l, newer: pinned > 0 && l > pinned, missing: !l && !pending.has(name), expect };
  });
}

// pinSecret pins secret name to version (0: the latest at apply). The pin
// keeps what the code expects of the secret, its type, mode and forwarded
// host (ADR 0058), and leaves its upstream, which is the version's (the
// apply writes it, and the mode, beside the version).
export function pinSecret(spec, name, version) {
  const ref = spec && isObject(spec.secrets) && isObject(spec.secrets[name]) ? spec.secrets[name] : {};
  const out = version > 0 ? { version } : {};
  return setObject(spec, 'secrets', name, { ...out, ...expectationOf(ref) });
}
