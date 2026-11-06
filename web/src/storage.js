// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Browser storage for the UI's few remembered things. Storage may be
// missing or refuse (a private window, blocked site data), so every access
// is guarded: where it fails, the value is kept in memory instead, and
// lives as long as the page.
//
//   localStorage   ferun-theme     dark or light
//                  ferun-tenants   {tenant: token}, the tenant tokens this
//                                  browser minted or was given
//                  ferun-draft:<tenant>/<env>
//                                  the deployment builder's draft of an
//                                  environment, "<tenant>/" a new one's
//                                  (views/draft.js): a spec and its base,
//                                  never a secret's value
//   sessionStorage ferun-admin     "user:password" of a Basic sign-in, for
//                                  the life of the tab only

export const THEME_KEY = 'ferun-theme';
export const TENANTS_KEY = 'ferun-tenants';
export const ADMIN_KEY = 'ferun-admin';

// memory holds what storage refused, by "<kind>/<key>".
const memory = new Map();

function area(kind) {
  try {
    return globalThis[kind] || null;
  } catch {
    return null;
  }
}

// readItem returns the stored string, or '' when there is none.
export function readItem(kind, key) {
  const k = `${kind}/${key}`;
  if (memory.has(k)) return memory.get(k);
  try {
    return area(kind)?.getItem(key) ?? '';
  } catch {
    return '';
  }
}

// writeItem stores value, or removes the key when value is empty; it
// returns false when storage refused and the value lives in memory.
export function writeItem(kind, key, value) {
  const k = `${kind}/${key}`;
  try {
    const s = area(kind);
    if (!s) throw new Error('no storage');
    if (value) s.setItem(key, value);
    else s.removeItem(key);
    memory.delete(k);
    return true;
  } catch {
    memory.set(k, value || '');
    return false;
  }
}
