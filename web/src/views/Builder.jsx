// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { api } from '../api.js';
import { Icon } from '../Icon.jsx';
import { ErrBox } from '../components.jsx';
import { readItem, writeItem } from '../storage.js';
import { envPath } from '../routes.js';
import { DiffView } from './Environments.jsx';
import { heavyEffect } from './envs.js';
import {
  SPEC_KINDS, addService, decodeDraft, newEnvID, draftKey, encodeDraft, getObject, isDnsLabel, isObject, isSecretName,
  objectsOf, parseSpecText, proposeMessage, removeObject, renameObject, setObject, specText, uniqueName,
} from './draft.js';
import { referencesOf, referencesTo, refLine, refsFrom, removeWithRefs, secretNames } from './refs.js';
import { groupFindings, severityCounts, unitKey, worst } from './findings.js';
import { rebase, resolve } from './rebase.js';
import { applyPins, imageView, pinSecret, secretPins } from './pins.js';
import { builtinCatalog, newAddon, readCatalog } from './catalog.js';
import { planner } from './planner.js';
import { AddonForm, FindingList, JobForm, RawForm, RouteForm, SecretForm, ServiceForm } from './BuilderForms.jsx';

// The deployment builder (ADR 0032), step 1 without the graph: an
// environment's tab Builder and the page of a new environment. It edits
// a draft, a spec in the API's JSON and its base, in forms and as JSON
// text (two views of the one draft), keeps it in this browser's storage,
// plans it 500 ms after every change, shows the findings at the objects
// and fields their paths name and the diff against the base, and applies
// it with its base; a refused base rebases object by object. It writes
// deployments and secrets' values, nothing else. Below 900 px the list of
// objects is cards that open their form at full width.

const HISTORY = 100;

// ObjectList is the draft's objects by kind, each a card with its
// summary, its findings' worst severity and its references as lines.
export function ObjectList({ spec, groups, open, onOpen, onAdd, pins, catalog }) {
  const refs = useMemo(() => referencesOf(spec), [spec]);
  const pinByName = new Map((pins || []).map((p) => [p.name, p]));
  return (
    <div className="b-list">
      {SPEC_KINDS.map(({ key, title, one }) => {
        const names = key === 'secrets' ? secretNames(spec) : objectsOf(spec, key).map(([n]) => n);
        if (key === 'bindings' && names.length === 0) return null;
        return (
          <div key={key} className="b-kind">
            <div className="b-kindhead">
              <span className="sect">{title} <span className="count">· {names.length}</span></span>
              <span className="spacer" />
              {key !== 'bindings' && (
                key === 'addons'
                  ? <AddAddonButton catalog={catalog} onAdd={(type) => onAdd(key, type)} />
                  : <button className="ghost mini" onClick={() => onAdd(key)}><Icon name="plus" size={12} /> {one}</button>)}
            </div>
            {names.map((name) => {
              const u = unitKey(key, name);
              const fs = groups.objects.get(u) || [];
              const sev = worst(fs);
              const obj = key === 'secrets' ? null : getObject(spec, key, name);
              const lines = key === 'secrets'
                ? referencesTo(spec, 'secrets', name, refs).map((r) => `${r.from.name} · ${refLine(r)}`)
                : key === 'routes' ? [] : refsFrom(spec, key, name, refs).map(refLine);
              const pin = pinByName.get(name);
              return (
                <button key={u} className={'b-card' + (open === u ? ' on' : '') + (sev ? ' ' + sev : '')} onClick={() => onOpen(u)}>
                  <span className="b-cardhead">
                    <span className="mono strong">{name}</span>
                    {sev && <span className={'badge ' + (sev === 'error' ? 'bad' : sev === 'warning' ? 'warn' : '')}>{fs.length} {sev === 'info' ? 'note' : sev}{fs.length > 1 ? 's' : ''}</span>}
                    {pin && pin.missing && <span className="badge bad">missing</span>}
                    {pin && pin.newer && <span className="badge warn">v{pin.latest} newer</span>}
                  </span>
                  <span className="muted small b-summary">{summary(key, obj, pin)}</span>
                  {lines.map((l, i) => <span key={i} className="mono small b-refline">{l}</span>)}
                </button>
              );
            })}
            {names.length === 0 && <p className="empty small b-none">none</p>}
          </div>
        );
      })}
    </div>
  );
}

function AddAddonButton({ catalog, onAdd }) {
  const [open, setOpen] = useState(false);
  return (
    <span className="b-addaddon">
      <button className="ghost mini" onClick={() => setOpen((o) => !o)}><Icon name="plus" size={12} /> addon</button>
      {open && (
        <span className="b-pop">
          {(catalog ? catalog.types : []).map((t) => (
            <button key={t.type} className="ghost mini" onClick={() => { setOpen(false); onAdd(t.type); }}>{t.type}</button>))}
        </span>)}
    </span>
  );
}

// summary is a card's line under its name.
function summary(key, o, pin) {
  if (key === 'secrets') {
    if (!pin) return '';
    const p = pin.pinned ? `pinned ${pin.pinned}` : 'latest when applied';
    return pin.latest ? `${p} · latest ${pin.latest}` : p;
  }
  if (!isObject(o)) return '';
  switch (key) {
    case 'services': {
      if (o.runtime === 'attached') return `attached · sandbox ${o.sandbox || '—'}`;
      const v = imageView(o);
      const ports = (Array.isArray(o.ports) ? o.ports : []).map((p) => p && `${p.name}:${p.port}`).join(' ');
      return [v.written || 'no image yet', v.digest ? 'pinned' : '', ports].filter(Boolean).join(' · ');
    }
    case 'jobs': return [imageView(o).written || 'no image yet', o.on === 'manual' ? 'when asked' : 'with deployments'].join(' · ');
    case 'addons': return [o.type, o.version, o.plan].filter(Boolean).join(' · ');
    case 'routes': return `${o.path || '?'} → ${o.service || '?'}${o.port !== undefined ? ':' + o.port : ''}`;
    case 'bindings': return o.upstream || '';
    default: return '';
  }
}

// ---------------------------------------------------------------- builder --

export function Builder({ tenant, env, st, navigate }) {
  const isNew = !env;
  const key = draftKey(tenant, env);
  const [draft, setDraftState] = useState(null);
  const draftRef = useRef(null);
  const [loadErr, setLoadErr] = useState('');
  const [storageOk, setStorageOk] = useState(true);
  const [view, setView] = useState('forms');
  const [open, setOpen] = useState('');
  const history = useRef({ past: [], future: [], at: 0 });
  const [, setHist] = useState(0);
  const [plan, setPlan] = useState(null);
  const [planning, setPlanning] = useState(false);
  const [secrets, setSecrets] = useState(null);
  const [secretsErr, setSecretsErr] = useState('');
  const pending = useRef(new Map());
  const [, bumpPending] = useState(0);
  const [catalog, setCatalog] = useState(null);
  const [conflicts, setConflicts] = useState(null);
  const [notice, setNotice] = useState('');
  const [applied, setApplied] = useState(null);

  // set replaces the draft and stores it; record pushes the draft before
  // onto the undo history (edits within a second are one step).
  const set = useCallback((next, { record = true, store = true } = {}) => {
    const prev = draftRef.current;
    if (record && prev) {
      const h = history.current;
      const now = Date.now();
      if (now - h.at > 1000 || h.past.length === 0) {
        h.past.push(prev.spec);
        if (h.past.length > HISTORY) h.past.shift();
      }
      h.at = now;
      h.future = [];
      setHist((x) => x + 1);
    }
    draftRef.current = next;
    setDraftState(next);
    if (store) setStorageOk(writeItem('localStorage', key, encodeDraft(next)));
  }, [key]);
  const edit = useCallback((spec, opts) => set({ ...draftRef.current, spec }, opts), [set]);

  const fetchSpec = useCallback(async (n) => (n > 0 ? (await api.getDeployment(env, n)).spec || {} : {}), [env]);

  // loadLatest starts a draft from deployment n (0: empty).
  const loadLatest = useCallback(async (n) => {
    const spec = await fetchSpec(n);
    history.current = { past: [], future: [], at: 0 };
    set({ spec, base: n, source: n > 0 ? `deployment ${n}` : 'empty', name: '' }, { record: false, store: false });
    writeItem('localStorage', key, '');
  }, [fetchSpec, set, key]);

  // rebaseOnto takes d's changes since its base onto deployment n.
  const rebaseOnto = useCallback(async (d, n) => {
    const [baseSpec, latestSpec] = await Promise.all([fetchSpec(d.base), fetchSpec(n)]);
    const r = rebase(baseSpec, d.spec, latestSpec);
    set({ ...d, spec: r.spec, base: n }, { record: true });
    setConflicts(r.conflicts.length ? { list: r.conflicts, from: d.base, to: n } : null);
    setNotice(`Rebased from deployment ${d.base || 'none'} onto deployment ${n}: ${r.applied.length} change${r.applied.length === 1 ? '' : 's'} kept`
      + (r.conflicts.length ? `, ${r.conflicts.length} conflict${r.conflicts.length === 1 ? '' : 's'} to decide.` : '.'));
  }, [fetchSpec, set]);

  // The draft when the builder opens: the stored one (rebased when its
  // base is no longer the latest deployment), else the latest
  // deployment's spec, else an empty spec.
  const ready = isNew || !!st;
  useEffect(() => {
    if (!ready || draftRef.current) return;
    const stored = decodeDraft(readItem('localStorage', key));
    const latest = isNew ? 0 : st.deployment;
    (async () => {
      if (stored && stored.base === latest) set(stored, { record: false, store: false });
      else if (stored && !isNew) await rebaseOnto(stored, latest);
      else if (stored) set(stored, { record: false, store: false });
      else if (isNew) set({ spec: {}, base: 0, source: 'empty', name: '' }, { record: false, store: false });
      else await loadLatest(latest);
    })().catch((e) => setLoadErr(e.message));
  }, [ready]); // eslint-disable-line react-hooks/exhaustive-deps

  // The secrets (names and versions) and the catalog.
  const refreshSecrets = useCallback(async () => {
    if (isNew) return;
    try {
      setSecrets((await api.listSecrets(env)).secrets || []);
      setSecretsErr('');
    } catch (e) {
      setSecretsErr(e.message);
    }
  }, [env, isNew]);
  useEffect(() => { refreshSecrets(); }, [refreshSecrets]);
  useEffect(() => {
    let live = true;
    api.addonCatalog().then(
      (a) => { if (live) setCatalog(readCatalog(a) || builtinCatalog()); },
      () => { if (live) setCatalog(builtinCatalog()); },
    );
    return () => { live = false; };
  }, []);

  // Plan on every change.
  const plannerRef = useRef(null);
  if (!plannerRef.current) {
    plannerRef.current = planner({
      run: (input) => (input.isNew ? api.planSpec(input.spec) : api.planEnvironment(input.env, input.spec, input.base)),
      onResult: (answer, input) => setPlan({ answer, spec: input.spec, base: input.base }),
      onError: (e, input) => setPlan({ error: e.message, spec: input.spec, base: input.base }),
      onBusy: setPlanning,
    });
  }
  useEffect(() => () => plannerRef.current.stop(), []);
  useEffect(() => {
    if (draft) plannerRef.current.change({ spec: draft.spec, base: draft.base, env, isNew });
  }, [draft && draft.spec, draft && draft.base]); // eslint-disable-line react-hooks/exhaustive-deps

  // setSecret writes a value: at once through the secrets API, pinning
  // the version it made; for a new environment into memory, until the
  // first apply writes it.
  const setSecret = useCallback(async (name, value) => {
    if (!isSecretName(name)) throw new Error(`${name} is not a secret name: a-z, 0-9, '.', '_' and '-'`);
    if (isNew) {
      pending.current.set(name, value);
      bumpPending((x) => x + 1);
      const d = draftRef.current;
      edit(pinSecret(d.spec, name, 0));
      return 0;
    }
    const v = await api.putSecret(env, name, value);
    edit(pinSecret(draftRef.current.spec, name, v.version));
    refreshSecrets();
    return v.version;
  }, [env, isNew, edit, refreshSecrets]);

  // ----------------------------------------------------------- derived --
  const spec = draft ? draft.spec : {};
  const answer = plan && plan.answer;
  const [applyFindings, setApplyFindings] = useState(null);
  useEffect(() => setApplyFindings(null), [answer]);
  const findings = applyFindings || (answer ? answer.findings || [] : []);
  const groups = useMemo(() => groupFindings(findings, spec), [findings, spec]);
  const names = useMemo(() => secretNames(spec), [spec]);
  const pins = useMemo(() => secretPins(spec, names, secrets || [], new Set(pending.current.keys())),
    [spec, names, secrets, pending.current.size]); // eslint-disable-line react-hooks/exhaustive-deps
  const knownSecrets = (secrets || []).map((s) => s.name).concat([...pending.current.keys()]);
  const stale = plan && draft && plan.spec !== draft.spec;
  const pinned = answer && 'images' in answer ? applyPins(spec, answer.images) : null;
  const latest = isNew ? 0 : (answer && typeof answer.current === 'number' ? Math.max(answer.current, st ? st.deployment : 0) : (st ? st.deployment : 0));
  const behind = draft && !isNew && latest > draft.base;

  // ------------------------------------------------------------ editing --
  const undo = () => {
    const h = history.current;
    if (!h.past.length) return;
    h.future.push(draftRef.current.spec);
    h.at = 0;
    set({ ...draftRef.current, spec: h.past.pop() }, { record: false });
    setHist((x) => x + 1);
  };
  const redo = () => {
    const h = history.current;
    if (!h.future.length) return;
    h.past.push(draftRef.current.spec);
    h.at = 0;
    set({ ...draftRef.current, spec: h.future.pop() }, { record: false });
    setHist((x) => x + 1);
  };

  const addObject = (k, type) => {
    const s = draftRef.current.spec;
    if (k === 'services') {
      const r = addService(s);
      edit(r.spec);
      setOpen(unitKey('services', r.name));
    } else if (k === 'jobs') {
      const n = uniqueName(s, 'jobs', 'migrate');
      edit(setObject(s, 'jobs', n, { image: '', on: 'deploy' }));
      setOpen(unitKey('jobs', n));
    } else if (k === 'addons') {
      const bp = catalog && catalog.byType.get(type);
      const n = uniqueName(s, 'addons', type);
      edit(setObject(s, 'addons', n, bp ? newAddon(bp) : { type }));
      setOpen(unitKey('addons', n));
    } else if (k === 'routes') {
      const [svc, so] = objectsOf(s, 'services')[0] || [];
      const port = so && Array.isArray(so.ports) && so.ports[0] ? so.ports[0].name || so.ports[0].port : undefined;
      const n = uniqueName(s, 'routes', objectsOf(s, 'routes').length ? 'route' : 'root');
      const r = { path: objectsOf(s, 'routes').length ? `/${n}` : '/', service: svc || '' };
      if (port !== undefined) r.port = port;
      edit(setObject(s, 'routes', n, r));
      setOpen(unitKey('routes', n));
    } else if (k === 'secrets') {
      setOpen('secrets/');
    }
  };

  const exportDraft = () => download(`${env || draft.name || 'new-environment'}.json`, specText(spec));

  if (loadErr) return <ErrBox error={loadErr} />;
  if (!draft) return <p className="empty">Loading the draft…</p>;

  if (applied) {
    return <Applied applied={applied} tenant={tenant} env={env} st={st} navigate={navigate}
      onAgain={() => { setApplied(null); draftRef.current = null; setDraftState(null); loadLatest(applied.number).catch((e) => setLoadErr(e.message)); }} />;
  }

  const openUnit = open;
  return (
    <div className={'builder' + (openUnit ? ' has-open' : '')}>
      <div className="b-toolbar">
        {isNew && <NewEnvName name={draft.name} onChange={(name) => set({ ...draftRef.current, name }, { record: false })} />}
        <span className="muted small">
          {isNew ? 'a new environment' : <>from <span className="mono">{draft.source || '—'}</span> · base <span className="mono">{draft.base || 'none'}</span></>}
        </span>
        <span className="spacer" />
        <span className="b-seg">
          <button className={'ghost mini' + (view === 'forms' ? ' on' : '')} onClick={() => setView('forms')}><Icon name="blocks" size={12} /> Forms</button>
          <button className={'ghost mini' + (view === 'json' ? ' on' : '')} onClick={() => setView('json')}><Icon name="file" size={12} /> JSON</button>
        </span>
        <button className="ghost mini" disabled={!history.current.past.length} onClick={undo} title="undo">Undo</button>
        <button className="ghost mini" disabled={!history.current.future.length} onClick={redo} title="redo">Redo</button>
        <button className="ghost mini" onClick={exportDraft} title="the draft's spec as a file">Export</button>
        <ImportButton onSpec={(s) => { edit(s); setNotice('Imported: the plan shows what it changes against the base.'); }} onError={setNotice} />
        <StartOver isNew={isNew} latest={latest} onStart={(n) => {
          setConflicts(null);
          setNotice('');
          if (isNew) set({ spec: {}, base: 0, source: 'empty', name: draftRef.current.name }, { record: true });
          else if (n === 0) set({ ...draftRef.current, spec: {}, source: 'empty' }, { record: true });
          else loadLatest(n).catch((e) => setLoadErr(e.message));
        }} />
      </div>
      {!storageOk && <p className="notice small">This browser keeps no storage for this page: the draft lives in memory and is gone with the page. Export it to keep it.</p>}
      {notice && <div className="message"><Icon name="alert" size={14} /><span>{notice}</span>
        <span className="spacer" /><button className="iconbtn" onClick={() => setNotice('')}><Icon name="x" size={12} /></button></div>}
      {behind && !conflicts && (
        <div className="message"><Icon name="alert" size={14} />
          <span>Deployment {latest} is newer than this draft's base {draft.base || '(none)'}.</span>
          <button className="mini" onClick={() => rebaseOnto(draftRef.current, latest).catch((e) => setNotice(e.message))}>Rebase onto {latest}</button>
        </div>)}
      {conflicts && (
        <Conflicts conflicts={conflicts} onPick={(c, choice) => {
          edit(resolve(draftRef.current.spec, c, choice));
          const rest = conflicts.list.filter((x) => x !== c);
          setConflicts(rest.length ? { ...conflicts, list: rest } : null);
        }} />)}
      {secretsErr && <p className="notice small">The secrets cannot be read: {secretsErr}</p>}

      {view === 'json'
        ? <JsonView spec={spec} onSpec={(s) => edit(s)} />
        : (
          <div className="b-main">
            <ObjectList spec={spec} groups={groups} open={openUnit} pins={pins} catalog={catalog} onOpen={setOpen} onAdd={addObject} />
            <div className="b-editor">
              <button className="ghost mini b-back" onClick={() => setOpen('')}><Icon name="arrowLeft" size={12} /> the objects</button>
              <Editor unit={openUnit} spec={spec} groups={groups} pins={pins} catalog={catalog} isNew={isNew}
                knownSecrets={knownSecrets} setSecret={setSecret} edit={edit} setOpen={setOpen}
                empty={objectsOf(spec, 'services').length === 0 && objectsOf(spec, 'jobs').length === 0}
                onFirst={(image) => { const r = addService(draftRef.current.spec, image); edit(r.spec); setOpen(unitKey('services', r.name)); }} />
            </div>
          </div>)}

      <PlanPanel isNew={isNew} draft={draft} plan={plan} planning={planning || stale} groups={groups} pinned={pinned}
        applyFindings={applyFindings} pins={pins}
        onApply={async (message, setBusy, setErr) => {
          setBusy(true);
          setErr('');
          try {
            const res = await applyDraft({ draftRef, set, edit, isNew, env, answer, message, pending, bumpPending });
            if (res.unchanged) {
              setNotice(`Nothing changes: the draft equals deployment ${res.number}.`);
            } else {
              writeItem('localStorage', key, '');
              if (isNew) navigate(envPath(tenant, draftRef.current.name));
              else setApplied(res);
            }
          } catch (e) {
            if (e.status === 409 && e.body && e.body.code === 'base_mismatch') {
              await rebaseOnto(draftRef.current, e.body.current);
            } else if (e.status === 422 && e.body && Array.isArray(e.body.findings)) {
              setApplyFindings(e.body.findings);
              setErr(e.message);
            } else {
              setErr(e.message);
            }
          } finally {
            setBusy(false);
          }
        }} />
    </div>
  );
}

// applyDraft writes the plan's pins into the draft and applies it with
// its base. A new environment is created first and its secrets written
// next; a step that fails leaves what the earlier ones made, and the next
// apply continues from there.
async function applyDraft({ draftRef, set, edit, isNew, env, answer, message, pending, bumpPending }) {
  let d = draftRef.current;
  if (answer && 'images' in answer) {
    const r = applyPins(d.spec, answer.images);
    if (r.unresolved.length) throw new Error(`these images do not resolve: ${r.unresolved.join(', ')}`);
    if (r.spec !== d.spec) { edit(r.spec, { record: false }); d = draftRef.current; }
  }
  let target = env;
  if (isNew) {
    target = d.name;
    if (!isDnsLabel(target)) throw new Error('name the environment first: a DNS label');
    if (!d.id) {
      set({ ...d, id: newEnvID() }, { record: false });
      d = draftRef.current;
    }
    try {
      await api.createEnvironment(target, d.id);
    } catch (e) {
      if (e.status === 409 && e.body && e.body.code === 'name_taken') {
        throw new Error(`the tenant has an environment ${target} already: choose another name`);
      }
      throw e;
    }
    for (const [name, value] of [...pending.current]) {
      const v = await api.putSecret(target, name, value);
      pending.current.delete(name);
      bumpPending((x) => x + 1);
      edit(pinSecret(draftRef.current.spec, name, v.version), { record: false });
    }
    d = draftRef.current;
  }
  return api.applyDeployment(target, { spec: d.spec, base: d.base, message });
}

function download(filename, text) {
  const blob = new Blob([text], { type: 'application/json' });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}

function ImportButton({ onSpec, onError }) {
  const ref = useRef(null);
  return (
    <>
      <button className="ghost mini" onClick={() => ref.current && ref.current.click()} title="replace the draft's spec with a file's">Import</button>
      <input ref={ref} type="file" accept=".json,application/json" hidden onChange={async (e) => {
        const file = e.target.files && e.target.files[0];
        e.target.value = '';
        if (!file) return;
        const r = parseSpecText(await file.text());
        if (r.error) onError(`${file.name}: ${r.error}`);
        else onSpec(r.spec);
      }} />
    </>
  );
}

function StartOver({ isNew, latest, onStart }) {
  const [open, setOpen] = useState(false);
  return (
    <span className="b-addaddon">
      <button className="ghost mini" onClick={() => setOpen((o) => !o)}>Start over</button>
      {open && (
        <span className="b-pop right">
          {!isNew && latest > 0 && <button className="ghost mini" onClick={() => { setOpen(false); onStart(latest); }}>from deployment {latest}</button>}
          <button className="ghost mini" onClick={() => { setOpen(false); onStart(0); }}>empty</button>
        </span>)}
    </span>
  );
}

function NewEnvName({ name, onChange }) {
  const ok = isDnsLabel(name);
  return (
    <span className="b-envname">
      <span className="muted small">name</span>
      <input className={'mono' + (name && !ok ? ' bad-input' : '')} placeholder="my-app" value={name || ''}
        onChange={(e) => onChange(e.target.value.trim())} />
      {name && !ok && <span className="bad small">a DNS label: a-z, 0-9 and -</span>}
    </span>
  );
}

// Editor is the open object's form, or what an empty draft offers.
function Editor({ unit, spec, groups, pins, catalog, isNew, knownSecrets, setSecret, edit, setOpen, empty, onFirst }) {
  const [confirm, setConfirm] = useState(null);
  useEffect(() => setConfirm(null), [unit]);
  if (!unit) {
    if (empty) return <FirstService onAdd={onFirst} />;
    return <p className="empty">Open an object to edit it, or add one.</p>;
  }
  if (unit === 'secrets/') return <NewSecret isNew={isNew} spec={spec} setSecret={setSecret} onDone={(n) => setOpen(unitKey('secrets', n))} />;
  const i = unit.indexOf('/');
  const key = unit.slice(0, i);
  const name = unit.slice(i + 1);
  const ctx = { spec, catalog, findings: groups.objects.get(unit) || [], setSecret, isNew, knownSecrets };
  const onChange = (o) => edit(setObject(spec, key, name, o));
  const onRename = (to) => {
    const taken = objectsOf(spec, key).some(([n]) => n === to)
      || ((key === 'services' || key === 'addons') && objectsOf(spec, key === 'services' ? 'addons' : 'services').some(([n]) => n === to));
    if (taken) return;
    edit(renameObject(spec, key, name, to));
    setOpen(unitKey(key, to));
  };
  const onRemove = () => {
    const refs = referencesTo(spec, key, name).filter((r) => !(r.from.key === key && r.from.name === name));
    if (refs.length === 0) { edit(key === 'secrets' ? removeObject(spec, key, name) : removeObject(spec, key, name)); setOpen(''); return; }
    setConfirm(refs);
  };
  const confirmBox = confirm && (
    <div className="message"><Icon name="alert" size={14} />
      <span>{confirm.length} reference{confirm.length === 1 ? '' : 's'} name {name}: {confirm.map((r) => `${r.from.name} · ${refLine(r)}`).join('; ')}.</span>
      <button className="mini danger" onClick={() => { edit(removeWithRefs(spec, key, name)); setOpen(''); }}>Remove them too</button>
      <button className="mini" onClick={() => { edit(removeObject(spec, key, name)); setOpen(''); }}>Keep them</button>
      <button className="ghost mini" onClick={() => setConfirm(null)}>Cancel</button>
    </div>
  );
  if (key === 'secrets') {
    const pin = pins.find((p) => p.name === name) || { name, pinned: 0, latest: 0, missing: true };
    return (
      <>{confirmBox}
        <SecretForm name={name} pin={pin} refs={referencesTo(spec, 'secrets', name)} ctx={ctx}
          onPin={(v) => edit(pinSecret(spec, name, v))} onRemove={onRemove} /></>);
  }
  const obj = getObject(spec, key, name);
  if (obj === undefined) return <p className="empty">No {key.slice(0, -1)} {name} in the draft.</p>;
  const props = { name, obj, ctx, onChange, onRename, onRemove };
  return (
    <>
      {confirmBox}
      {key === 'services' && <ServiceForm {...props} />}
      {key === 'jobs' && <JobForm {...props} />}
      {key === 'addons' && <AddonForm {...props} />}
      {key === 'routes' && <RouteForm {...props} />}
      {key === 'bindings' && <RawForm kind="binding" {...props} />}
    </>
  );
}

// FirstService is an empty environment's first step: an image and a port
// named http, with the route / to it (ADR 0032).
function FirstService({ onAdd }) {
  const [image, setImage] = useState('');
  return (
    <div className="b-first">
      <h3>Add a service</h3>
      <p className="muted small">An image from any registry; the builder gives it a port named http and the route <span className="mono">/</span>.
        Plan, apply.</p>
      <div className="b-row">
        <input className="mono grow" placeholder="ghcr.io/acme/web:1.0" value={image} onChange={(e) => setImage(e.target.value)}
          onKeyDown={(e) => { if (e.key === 'Enter' && image) onAdd(image); }} />
        <button className="primary" disabled={!image} onClick={() => onAdd(image)}><Icon name="plus" size={13} /> Add a service</button>
      </div>
    </div>
  );
}

function NewSecret({ isNew, spec, setSecret, onDone }) {
  const [name, setName] = useState('');
  const [value, setValue] = useState('');
  const [err, setErr] = useState('');
  const [busy, setBusy] = useState(false);
  const exists = secretNames(spec).includes(name);
  return (
    <div className="b-form">
      <div className="b-objhead"><span className="badge acc">secret</span><span className="strong">a new secret</span></div>
      <p className="muted small">{isNew
        ? 'Its value stays in this page until the first apply, which writes it before the deployment.'
        : 'Its value goes to the environment at once and never into the draft, this browser\'s storage or an export.'}</p>
      <div className="b-row wrap">
        <input className="mono" placeholder="name" value={name} onChange={(e) => setName(e.target.value.trim())} />
        <input type="password" className="grow" autoComplete="new-password" placeholder="the value" value={value}
          onChange={(e) => setValue(e.target.value)} />
        <button className="primary mini" disabled={busy || !value || !isSecretName(name) || exists} onClick={async () => {
          setBusy(true);
          setErr('');
          try {
            await setSecret(name, value);
            onDone(name);
          } catch (e) {
            setErr(e.message);
          } finally {
            setBusy(false);
          }
        }}>Set</button>
      </div>
      {name && !isSecretName(name) && <p className="bad small">a-z, 0-9, '.', '_' and '-'; "addon." is the platform's</p>}
      {exists && <p className="bad small">The draft knows {name} already: open it to set a new version.</p>}
      {err && <p className="bad small">{err}</p>}
    </div>
  );
}

// JsonView is the draft as JSON: an edit is parsed on each change; text
// that does not parse is marked, and the draft keeps the last one that
// did.
export function JsonView({ spec, onSpec }) {
  const [text, setText] = useState(() => specText(spec));
  const [err, setErr] = useState('');
  const mine = useRef(null);
  useEffect(() => {
    if (spec !== mine.current) { setText(specText(spec)); setErr(''); }
  }, [spec]);
  return (
    <div className="b-json">
      <textarea className={'mono b-text' + (err ? ' bad-input' : '')} spellCheck={false} value={text}
        onChange={(e) => {
          setText(e.target.value);
          const r = parseSpecText(e.target.value);
          if (r.error) { setErr(r.error); return; }
          setErr('');
          mine.current = r.spec;
          onSpec(r.spec);
        }} />
      {err
        ? <p className="bad small">{err}: the forms and the plan keep the last text that parsed.</p>
        : <p className="muted small">The draft as the API takes it. Edits here are edits of the forms, and back.</p>}
    </div>
  );
}

function Conflicts({ conflicts, onPick }) {
  return (
    <div className="b-conflicts">
      <div className="sect">conflicts · changed in the draft and since deployment {conflicts.from || '(none)'}</div>
      {conflicts.list.map((c) => (
        <div key={c.unit} className="b-conflict">
          <div className="mono strong">{c.unit}</div>
          <div className="b-conflict-sides">
            <div><div className="muted small">this draft</div><pre className="specpre">{c.mine === undefined ? '(removed)' : JSON.stringify(c.mine, null, 2)}</pre>
              <button className="mini" onClick={() => onPick(c, 'mine')}>Keep this draft's</button></div>
            <div><div className="muted small">deployment {conflicts.to}</div><pre className="specpre">{c.theirs === undefined ? '(removed)' : JSON.stringify(c.theirs, null, 2)}</pre>
              <button className="mini" onClick={() => onPick(c, 'theirs')}>Keep deployment {conflicts.to}'s</button></div>
          </div>
        </div>
      ))}
    </div>
  );
}

// PlanPanel is the plan's answer: its verdict, the findings without an
// object, the diff against the base with each change's effect, and the
// apply with a message the builder proposes.
export function PlanPanel({ isNew, draft, plan, planning, groups, pinned, applyFindings, pins, onApply }) {
  const answer = plan && plan.answer;
  const diff = answer && answer.diff ? answer.diff : [];
  const heavy = diff.filter((d) => heavyEffect(d.effect));
  const [ack, setAck] = useState(false);
  const [message, setMessage] = useState('');
  const [touched, setTouched] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');
  const proposed = proposeMessage(diff, draft.spec);
  useEffect(() => { setAck(false); }, [heavy.length]);
  const counts = severityCounts(applyFindings || (answer && answer.findings) || []);
  const unresolved = pinned ? pinned.unresolved : [];
  const missing = (pins || []).filter((p) => p.missing).map((p) => p.name);
  const nothing = !isNew && answer && answer.valid && diff.length === 0;
  const blocked = busy || planning || !answer || !answer.valid || unresolved.length > 0 || (heavy.length > 0 && !ack)
    || nothing || (isNew && !isDnsLabel(draft.name));
  return (
    <div className="b-plan">
      <div className="b-planhead">
        <h3>Plan</h3>
        {planning && <span className="muted small">planning…</span>}
        {!planning && answer && (answer.valid
          ? <span className="badge ok">valid</span>
          : <span className="badge bad">{counts.error} error{counts.error === 1 ? '' : 's'}</span>)}
        {!planning && answer && counts.warning > 0 && <span className="badge warn">{counts.warning} warning{counts.warning === 1 ? '' : 's'}</span>}
        {plan && plan.error && <span className="bad small">{plan.error}</span>}
        <span className="spacer" />
        {!isNew && answer && <span className="muted small">against deployment {answer.base || '(none)'}</span>}
      </div>
      <FindingList list={groups.global} />
      {missing.length > 0 && !isNew && (
        <p className="bad small">Missing secrets: {missing.join(', ')}. Open each to set a value.</p>)}
      {pinned === null && answer && (
        <p className="panel-note">This ferund answers no image pins with a plan; it pins the images at apply.</p>)}
      {unresolved.length > 0 && (
        <p className="bad small">These images do not resolve, so the builder does not apply: {unresolved.join(', ')}</p>)}
      {isNew
        ? <p className="muted small">A new environment: everything in the draft is added with deployment 1.</p>
        : <DiffView diff={diff} empty={answer ? 'No changes against the base.' : 'Not planned yet.'} />}
      <div className="b-apply">
        {heavy.length > 0 && (
          <label className="b-ack">
            <input type="checkbox" checked={ack} onChange={(e) => setAck(e.target.checked)} />
            <span>I accept: {heavy.map((d) => `${d.kind}/${d.name} ${d.effect}`).join('; ')}</span>
          </label>)}
        <div className="b-row">
          <input className="grow" placeholder={proposed || 'a message for the deployment'} value={touched ? message : proposed}
            onChange={(e) => { setTouched(true); setMessage(e.target.value); }} />
          <button className="primary" disabled={blocked}
            onClick={() => onApply(touched ? message : proposed, setBusy, setErr)}>
            <Icon name="check" size={13} /> {isNew ? 'Create and apply' : 'Apply'}</button>
        </div>
        {err && <p className="bad small">{err}</p>}
        {nothing && <p className="muted small">Nothing to apply: the draft equals its base.</p>}
      </div>
    </div>
  );
}

function Applied({ applied, tenant, env, st, navigate, onAgain }) {
  const rollout = st && st.rollout && st.rollout.deployment === applied.number ? st.rollout : null;
  return (
    <div className="b-applied">
      <h3><Icon name="check" size={15} /> Deployment {applied.number} applied</h3>
      <p>
        {rollout ? <>Its rollout: <span className="mono">{rollout.state}</span>{rollout.reason ? ` · ${rollout.reason}` : ''}</>
          : <>It waits for its rollout ({applied.state}).</>}
      </p>
      <DiffView diff={applied.diff} />
      <div className="b-row">
        <button onClick={() => navigate(envPath(tenant, env))}>The overview</button>
        <button onClick={() => navigate(envPath(tenant, env, 'deployments', applied.number))}>Deployment {applied.number}</button>
        <button className="primary" onClick={onAgain}>Edit again</button>
      </div>
    </div>
  );
}
