// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { api } from '../api.js';
import { Icon } from '../Icon.jsx';
import { ErrBox } from '../components.jsx';
import { readItem, writeItem } from '../storage.js';
import { envPath } from '../routes.js';
import { mayBuild, useAccess } from '../people.js';
import { fmtValue, heavyEffect, statusState } from './envs.js';
import {
  SPEC_KINDS, addService, decodeDraft, deployBlock, draftKey, draftWords, encodeDraft, getObject, isDnsLabel, isSecretName,
  keyOfKind, kindOf, newEnvID, objectsOf, parseSpecText, proposeMessage, removeObject, renameObject, setObject, specText, uniqueName,
} from './draft.js';
import { referencesOf, referencesTo, refLine, refsFrom, removeWithRefs, secretNames } from './refs.js';
import { groupFindings, severityCounts, unitKey, worst } from './findings.js';
import { changesSince, rebase, resolve } from './rebase.js';
import { applyPins, imageView, pinSecret, secretPins } from './pins.js';
import { builtinCatalog, newAddon, readCatalog } from './catalog.js';
import { planner } from './planner.js';
import { AddonForm, FindingList, JobForm, RawForm, RouteForm, SecretForm, ServiceForm } from './BuilderForms.jsx';
import { GraphView, MARK, summary } from './GraphView.jsx';
import { changeList, diffMarks, removedOf } from './graph.js';
import '../styles/builder.css';

// The deployment builder (ADR 0032): an environment's "New deployment"
// and the page of a new environment. It edits a draft of the next
// deployment, a spec in the API's JSON and its base (the deployment it
// starts from), in forms and as JSON text (two views of the one draft),
// keeps it in this browser's storage, plans it 500 ms after every change,
// and deploys it with its base after a review; a refused base rebases
// object by object. A band above says what the draft is, how many objects
// it changes and that nothing changes until it is deployed. From 900 px
// the objects are a graph (GraphView.jsx) with the open object's form, or
// the review, beside it; below, a list of cards that open their form at
// full width. It writes deployments and secrets' values, nothing else.

const HISTORY = 100;

// ObjectList is the draft's objects by kind, each a card with its
// summary, its findings' worst severity and its references as lines.
export function ObjectList({ spec, groups, open, onOpen, onAdd, pins, catalog, marks }) {
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
              {key !== 'bindings' && onAdd && (
                key === 'addons'
                  ? <AddAddonButton catalog={catalog} onAdd={(type) => onAdd(key, type)} />
                  : <button className="ghost mini" onClick={() => onAdd(key)}><Icon name="plus" size={12} /> {one}</button>)}
            </div>
            {names.map((name) => {
              const u = unitKey(key, name);
              const fs = groups.objects.get(u) || [];
              const sev = worst(fs);
              const obj = key === 'secrets' ? null : getObject(spec, key, name);
              const lines = key === 'secrets' ? secretLines(referencesTo(spec, 'secrets', name, refs))
                : key === 'routes' ? [] : refsFrom(spec, key, name, refs).map(refLine);
              const pin = pinByName.get(name);
              const m = marks && marks.get(u);
              return (
                <button key={u} className={'b-card' + (open === u ? ' on' : '') + (sev ? ' ' + sev : '') + (m ? ' m-' + m : '')} onClick={() => onOpen(u)}>
                  <span className="b-cardhead">
                    <span className="mono strong">{name}</span>
                    {m && <span className={'g-mark m-' + m} title={MARK[m][1] + ' by this draft'}>{MARK[m][0]}</span>}
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

// secretLines are a secret card's lines: who uses it by a variable or a
// credential, one each; the registry logins folded into one line.
export function secretLines(refs) {
  const logins = [...new Set(refs.filter((r) => r.via === 'imageCredentials').map((r) => r.from.name))];
  const lines = refs.filter((r) => r.via !== 'imageCredentials').map((r) => `${r.from.name} · ${refLine(r)}`);
  if (logins.length) lines.push(`registry login of ${logins.join(', ')}`);
  return lines;
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

// useWide says whether the page is wide enough for the graph: wider than
// the 900 px below which the CSS lays out the list.
const WIDE = '(min-width: 901px)';
function useWide() {
  const q = () => (typeof window !== 'undefined' && window.matchMedia ? window.matchMedia(WIDE) : null);
  const [wide, setWide] = useState(() => { const m = q(); return m ? m.matches : false; });
  useEffect(() => {
    const m = q();
    if (!m) return undefined;
    const f = () => setWide(m.matches);
    m.addEventListener('change', f);
    return () => m.removeEventListener('change', f);
  }, []);
  return wide;
}

// ---------------------------------------------------------------- builder --

export function Builder({ tenant, env, st, navigate }) {
  const isNew = !env;
  // What the role allows (ADR 0031): a new environment needs write on the
  // tenant, a deployment and a secret's value write on the environment
  // (the tenant grant or the environment's, the higher). Below that the
  // builder shows the latest deployment's spec and changes nothing: no
  // plan (it needs write), no apply, no secret written, no draft stored.
  const access = useAccess(tenant);
  const readOnly = !mayBuild(access, isNew ? '' : (st && st.id) || env);
  const key = draftKey(tenant, env);
  const [draft, setDraftState] = useState(null);
  const draftRef = useRef(null);
  const [baseSpec, setBaseSpec] = useState(null);
  const [loadErr, setLoadErr] = useState('');
  const [storageOk, setStorageOk] = useState(true);
  const [view, setView] = useState('forms');
  const [open, setOpenState] = useState('');
  const [reviewing, setReviewing] = useState(false);
  const [hover, setHover] = useState('');
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
  const wide = useWide();
  const [focus, setFocus] = useState(true);
  // An object opened closes the review, which takes the same place.
  const setOpen = useCallback((u) => { setOpenState(u); if (u) setReviewing(false); }, []);

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
  const edit = useCallback((spec, opts) => {
    if (!readOnly) set({ ...draftRef.current, spec }, opts);
  }, [set, readOnly]);

  const fetchSpec = useCallback(async (n) => (n > 0 ? (await api.getDeployment(env, n)).spec || {} : {}), [env]);

  // loadLatest starts a draft from deployment n (0: empty).
  const loadLatest = useCallback(async (n) => {
    const spec = await fetchSpec(n);
    history.current = { past: [], future: [], at: 0 };
    setBaseSpec(spec);
    set({ spec, base: n, source: n > 0 ? `deployment ${n}` : 'empty', name: '' }, { record: false, store: false });
    writeItem('localStorage', key, '');
  }, [fetchSpec, set, key]);

  // rebaseOnto takes d's changes since its base onto deployment n.
  const rebaseOnto = useCallback(async (d, n) => {
    const [base, latestSpec] = await Promise.all([fetchSpec(d.base), fetchSpec(n)]);
    const r = rebase(base, d.spec, latestSpec);
    setBaseSpec(latestSpec);
    set({ ...d, spec: r.spec, base: n, source: `${d.source || 'a draft'}, rebased onto deployment ${n}` }, { record: true });
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
      if (readOnly) {
        const spec = await fetchSpec(latest);
        setBaseSpec(spec);
        set({ spec, base: latest, source: latest > 0 ? `deployment ${latest}` : 'empty', name: '' }, { record: false, store: false });
        return;
      }
      if (stored && stored.base === latest) {
        setBaseSpec(await fetchSpec(latest));
        set(stored, { record: false, store: false });
      } else if (stored && !isNew) await rebaseOnto(stored, latest);
      else if (stored) { setBaseSpec({}); set(stored, { record: false, store: false }); }
      else if (isNew) { setBaseSpec({}); set({ spec: {}, base: 0, source: 'empty', name: '' }, { record: false, store: false }); }
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
      (e) => {
        if (!live) return;
        setCatalog(builtinCatalog());
        setNotice(`The catalog of addons cannot be read (${e.message}): the forms use the list this UI knows.`);
      },
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
    if (draft && !readOnly) plannerRef.current.change({ spec: draft.spec, base: draft.base, env, isNew });
  }, [draft && draft.spec, draft && draft.base]); // eslint-disable-line react-hooks/exhaustive-deps

  // setSecret writes a value: at once through the secrets API, pinning
  // the version it made; for a new environment into memory, until the
  // first apply writes it.
  const setSecret = useCallback(async (name, value) => {
    if (readOnly) throw new Error('Your role does not allow this: it needs write on this environment.');
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
  }, [env, isNew, edit, refreshSecrets, readOnly]);

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
  // The draft's changes against its base, object by object: the marks
  // on the nodes and the band's count, at once; the plan's diff says
  // their effects.
  const changes = useMemo(() => (baseSpec && draft ? changeList(changesSince(baseSpec, spec)) : []), [baseSpec, spec]); // eslint-disable-line react-hooks/exhaustive-deps
  const marks = useMemo(() => diffMarks(changes), [changes]);
  const removed = useMemo(() => removedOf(changes, spec), [changes, spec]);
  const words = draftWords({
    isNew, readOnly, base: draft ? draft.base : 0, latest, deployed: st ? st.deployed : 0, state: statusState(st), changes: changes.length,
  });

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
  const discard = (n) => {
    setConflicts(null);
    setNotice('');
    setReviewing(false);
    setOpen('');
    if (isNew) set({ spec: {}, base: 0, source: 'empty', name: draftRef.current.name }, { record: true });
    else if (n === 0) set({ ...draftRef.current, spec: {}, source: 'empty' }, { record: true });
    else loadLatest(n).catch((e) => setLoadErr(e.message));
  };

  if (loadErr) return <ErrBox error={loadErr} />;
  if (!draft) return <p className="empty">Loading the draft…</p>;

  if (applied) {
    return <Applied applied={applied} tenant={tenant} env={env} st={st} navigate={navigate}
      onAgain={() => { setApplied(null); draftRef.current = null; setDraftState(null); loadLatest(applied.number).catch((e) => setLoadErr(e.message)); }} />;
  }

  const entry = { public: !!(st && st.public), hosts: (st && st.hosts) || [], env: isNew ? draft.name : env };
  const empty = !readOnly && objectsOf(spec, 'services').length === 0 && objectsOf(spec, 'jobs').length === 0;
  const editor = (
    <fieldset className="b-fieldset" disabled={readOnly}>
      <Editor unit={open} spec={spec} groups={groups} pins={pins} catalog={catalog} isNew={isNew}
        knownSecrets={knownSecrets} setSecret={setSecret} edit={edit} setOpen={setOpen}
        empty={empty}
        onFirst={(image) => { const r = addService(draftRef.current.spec, image); edit(r.spec); setOpen(unitKey('services', r.name)); }} />
    </fieldset>
  );
  const review = !readOnly && (
    <ReviewPanel isNew={isNew} draft={draft} plan={plan} planning={planning || stale} groups={groups} findings={findings}
      pinned={pinned} applyFindings={applyFindings} pins={pins} conflicts={conflicts ? conflicts.list.length : 0}
      changes={changes} next={words.next} onOpen={setOpen} onClose={() => setReviewing(false)}
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
  );
  const side = reviewing && review ? review : open ? <ObjectPanel unit={open} onClose={() => setOpen('')}>{editor}</ObjectPanel>
    : empty && view === 'forms' ? editor : null;
  const canReview = !readOnly;

  return (
    <div className={'builder' + (open || reviewing ? ' has-open' : '') + (wide ? ' wide' : '')}>
      <Band words={words} readOnly={readOnly} isNew={isNew} draft={draft} plan={plan} planning={planning || stale}
        changes={changes.length} view={view} setView={setView}
        undo={history.current.past.length ? undo : null} redo={history.current.future.length ? redo : null}
        onExport={exportDraft} onImport={(s) => { edit(s); setNotice('Imported: the review shows what it changes against the base.'); }}
        onError={setNotice} onDiscard={discard} latest={latest}
        onName={(name) => set({ ...draftRef.current, name }, { record: false })}
        onRebase={() => rebaseOnto(draftRef.current, latest).catch((e) => setNotice(e.message))}
        reviewing={reviewing} onReview={canReview ? () => { setReviewing((r) => !r); setOpenState(''); } : null} conflicts={!!conflicts} />
      {!storageOk && <p className="notice small">This browser keeps no storage for this page: the draft lives in memory and is gone with the page. Export it to keep it.</p>}
      {notice && <div className="message"><Icon name="alert" size={14} /><span>{notice}</span>
        <span className="spacer" /><button className="iconbtn" onClick={() => setNotice('')}><Icon name="x" size={12} /></button></div>}
      {conflicts && (
        <Conflicts conflicts={conflicts} onPick={(c, choice) => {
          edit(resolve(draftRef.current.spec, c, choice));
          const rest = conflicts.list.filter((x) => x !== c);
          setConflicts(rest.length ? { ...conflicts, list: rest } : null);
        }} />)}
      {secretsErr && <p className="notice small">The secrets cannot be read: {secretsErr}</p>}

      <div className={'b-gmain' + (side ? '' : ' solo')}>
        <div className="b-gcol">
          {view === 'json' && <JsonView spec={spec} onSpec={(s) => edit(s)} readOnly={readOnly} />}
          {view === 'forms' && wide && (
            <>
              <div className="b-gbar">
                <span className="muted small b-ghint">{readOnly ? 'Click an object to see it.' : <>Click an object to change it; drag a handle <span className="b-dot" /> onto another to connect them.</>}</span>
                <span className="spacer" />
                {!readOnly && <>
                  <button className="ghost mini" onClick={() => addObject('services')}><Icon name="plus" size={12} /> service</button>
                  <button className="ghost mini" onClick={() => addObject('jobs')}><Icon name="plus" size={12} /> job</button>
                  <AddAddonButton catalog={catalog} onAdd={(type) => addObject('addons', type)} />
                  <button className="ghost mini" onClick={() => addObject('routes')}><Icon name="plus" size={12} /> route</button>
                </>}
                <label className="b-focus small" title="dim what is not next to the open object">
                  <input type="checkbox" checked={focus} onChange={(e) => setFocus(e.target.checked)} /> focus</label>
              </div>
              <GraphView spec={spec} groups={groups} marks={marks} removed={removed} open={open} onOpen={setOpen}
                entry={entry} catalog={catalog} knownSecrets={knownSecrets} pins={pins} edit={readOnly ? null : edit} focus={focus}
                hover={hover} onHover={setHover} onAddSecret={() => addObject('secrets')} />
            </>)}
          {view === 'forms' && !wide && (
            <ObjectList spec={spec} groups={groups} open={open} pins={pins} catalog={catalog} marks={marks} onOpen={setOpen}
              onAdd={readOnly ? null : addObject} />)}
        </div>
        {side && <div className="b-side">
          {!wide && <button className="ghost mini b-back" onClick={() => { setOpen(''); setReviewing(false); }}><Icon name="arrowLeft" size={12} /> the objects</button>}
          {side}
        </div>}
      </div>
    </div>
  );
}

// Band says what the builder edits: the draft of which deployment, from
// which, how many changes, that nothing changes until it is deployed; and
// carries the draft's actions and the way to its review.
function Band({ words, readOnly, isNew, draft, plan, planning, changes, view, setView, undo, redo, onExport, onImport, onError,
  onDiscard, latest, onName, onRebase, reviewing, onReview, conflicts }) {
  const answer = plan && plan.answer;
  const errors = answer ? severityCounts(answer.findings || []).error : 0;
  return (
    <div className="b-band">
      <div className="b-bandtext">
        <div className="b-bandtitle">
          <Icon name="blocks" size={15} />
          <span className="strong">{words.title}</span>
          {words.from && <span className="muted">{words.from}</span>}
          {words.behind > 0 && (
            <span className="warntext small">· deployment {words.behind} is newer <button className="mini" onClick={onRebase}>Rebase onto {words.behind}</button></span>)}
        </div>
        <div className="b-bandsub small">
          {isNew && <NewEnvName name={draft.name} onChange={onName} />}
          {!readOnly && <span className={'b-count' + (changes ? ' some' : '')}>{words.count}</span>}
          {!readOnly && (planning ? <span className="muted">planning…</span>
            : plan && plan.error ? <span className="bad">the plan failed: {plan.error}</span>
              : errors > 0 ? <button className="b-link bad" onClick={onReview}>{errors} error{errors === 1 ? '' : 's'} in the plan</button>
                : answer ? <span className="muted"><Icon name="check" size={11} /> the plan accepts it</span> : null)}
          <span className="muted">{readOnly
            ? 'Your role does not allow changes here: it needs write on this environment. You see the latest deployment\'s spec.'
            : isNew ? 'Nothing exists until you deploy. The draft is kept in this browser.'
              : 'Nothing changes in the environment until you deploy. The draft is kept in this browser.'}</span>
        </div>
      </div>
      <div className="b-bandactions">
        {!readOnly && <button className="ghost mini" disabled={!undo} onClick={undo || undefined} title="undo"><Icon name="undo" size={12} /> Undo</button>}
        {!readOnly && <button className="ghost mini" disabled={!redo} onClick={redo || undefined} title="redo">Redo</button>}
        {!readOnly && <DiscardDraft isNew={isNew} latest={latest} changes={changes} onDiscard={onDiscard} />}
        {!readOnly && <ImportButton onSpec={onImport} onError={onError} />}
        <button className="ghost mini" onClick={onExport} title="the draft's spec as a JSON file">Export</button>
        <span className="b-seg">
          <button className={'ghost mini' + (view === 'forms' ? ' on' : '')} onClick={() => setView('forms')}><Icon name="blocks" size={12} /> Forms</button>
          <button className={'ghost mini' + (view === 'json' ? ' on' : '')} onClick={() => setView('json')}><Icon name="file" size={12} /> JSON</button>
        </span>
        {onReview && (
          <button className={'primary' + (reviewing ? ' on' : '')} onClick={onReview}
            title={conflicts ? 'decide the conflicts first' : 'the changes, their effects and the deploy'}>
            <Icon name="check" size={13} /> Review &amp; deploy{changes > 0 ? ` (${changes})` : ''}</button>)}
      </div>
    </div>
  );
}

function DiscardDraft({ isNew, latest, changes, onDiscard }) {
  const [open, setOpen] = useState(false);
  return (
    <span className="b-addaddon">
      <button className="ghost mini" onClick={() => setOpen((o) => !o)} title="start over">Discard draft</button>
      {open && (
        <span className="b-pop right b-discard">
          <span className="small">{changes > 0 ? `Discard ${changes} change${changes === 1 ? '' : 's'}?` : 'Start over?'}</span>
          {!isNew && latest > 0 && (
            <button className="mini danger" onClick={() => { setOpen(false); onDiscard(latest); }}>Start again from deployment {latest}</button>)}
          <button className="ghost mini" onClick={() => { setOpen(false); onDiscard(0); }}>Start from an empty spec</button>
          <button className="ghost mini" onClick={() => setOpen(false)}>Keep the draft</button>
        </span>)}
    </span>
  );
}

// ObjectPanel is the open object's form beside the graph, titled with
// what it is and where its changes go.
function ObjectPanel({ unit, onClose, children }) {
  const i = unit.indexOf('/');
  const key = unit.slice(0, i);
  const name = unit.slice(i + 1);
  const one = (kindOf(key) || { one: key }).one;
  const where = key === 'secrets'
    ? 'a value goes to the environment at once; the draft pins its version'
    : 'changes go into the draft';
  return (
    <div className="b-panel">
      <div className="b-panelhead">
        <span className="small"><span className="muted">{one}</span> <span className="mono strong">{name || 'new'}</span>
          <span className="muted"> — {where}</span></span>
        <span className="spacer" />
        <button className="iconbtn" title="close" onClick={onClose}><Icon name="x" size={13} /></button>
      </div>
      {children}
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
export function JsonView({ spec, onSpec, readOnly = false }) {
  const [text, setText] = useState(() => specText(spec));
  const [err, setErr] = useState('');
  const mine = useRef(null);
  useEffect(() => {
    if (spec !== mine.current) { setText(specText(spec)); setErr(''); }
  }, [spec]);
  return (
    <div className="b-json">
      <textarea className={'mono b-text' + (err ? ' bad-input' : '')} spellCheck={false} value={text} readOnly={readOnly}
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

// ReviewPanel is the review before a deployment: the changes object by
// object with their effects (the plan's diff against the base), the
// findings (errors block), the images that do not resolve, the secrets
// without a value, an acceptance of data loss or downtime, the message
// the builder proposes, and the deploy.
export function ReviewPanel({ isNew, draft, plan, planning, groups, findings = [], pinned, applyFindings, pins, conflicts = 0,
  changes = [], next = 0, onApply, onOpen = () => {}, onClose }) {
  const answer = plan && plan.answer;
  const diff = answer && answer.diff ? answer.diff : [];
  const heavy = diff.filter((d) => heavyEffect(d.effect));
  const [ack, setAck] = useState(false);
  const [message, setMessage] = useState('');
  const [touched, setTouched] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');
  const proposed = proposeMessage(isNew ? changes.map((c) => ({ kind: c.kind, name: c.name, change: c.change })) : diff, draft.spec);
  useEffect(() => { setAck(false); }, [heavy.length]);
  const all = applyFindings || (answer && answer.findings) || findings;
  const counts = severityCounts(all);
  const unresolved = pinned ? pinned.unresolved : [];
  const missing = (pins || []).filter((p) => p.missing).map((p) => p.name);
  const block = deployBlock({ isNew, name: draft.name, planning, answer, diff, unresolved, heavy: heavy.length, ack, conflicts, busy });
  const open = (kind, name) => {
    const key = keyOfKind(kind);
    if (key && key !== 'routes') onOpen(unitKey(key, name));
    else if (key === 'routes') onOpen(unitKey('routes', name));
  };
  return (
    <div className="b-review">
      <div className="b-panelhead">
        <span className="strong">Review deployment {next}</span>
        <span className="muted small">{isNew ? 'of a new environment' : `against deployment ${draft.base || '(none)'}`}</span>
        <span className="spacer" />
        {onClose && <button className="iconbtn" title="close" onClick={onClose}><Icon name="x" size={13} /></button>}
      </div>
      <div className="b-planhead">
        {planning && <span className="muted small">planning…</span>}
        {!planning && answer && (answer.valid
          ? <span className="badge ok">the plan accepts it</span>
          : <span className="badge bad">{counts.error} error{counts.error === 1 ? '' : 's'}</span>)}
        {!planning && answer && counts.warning > 0 && <span className="badge warn">{counts.warning} warning{counts.warning === 1 ? '' : 's'}</span>}
        {plan && plan.error && <span className="bad small">{plan.error}</span>}
      </div>
      {all.length > 0 && (
        <div className="b-revsect">
          <div className="sect">findings{counts.error > 0 ? ' · errors block the deploy' : ''}</div>
          <FindingList list={all} />
        </div>)}
      {missing.length > 0 && !isNew && (
        <p className="bad small">Secrets without a value: {missing.join(', ')}. Open each in the secrets list to set one.</p>)}
      {pinned === null && answer && (
        <p className="panel-note">This ferund answers no image pins with a plan; it pins the images at deploy.</p>)}
      {unresolved.length > 0 && (
        <p className="bad small">These images do not resolve, so the builder does not deploy: {unresolved.join(', ')}</p>)}
      <div className="b-revsect">
        <div className="sect">changes{isNew ? ' · everything is added with deployment 1' : ''}</div>
        {isNew
          ? (changes.length === 0
            ? <p className="empty small">Nothing in the draft yet.</p>
            : <ul className="b-changes">{changes.map((c) => (
              <li key={c.unit} className="b-change"><span className={'g-mark m-' + c.change}>{MARK[c.change][0]}</span>
                <button className="b-link" onClick={() => open(c.kind, c.name)}>{c.kind} <span className="mono">{c.name}</span></button>
                <span className="spacer" /><span className="muted small">added</span></li>))}</ul>)
          : diff.length === 0
            ? <p className="empty small">{answer ? 'Nothing changes against the base.' : 'Not planned yet.'}</p>
            : <ul className="b-changes">{diff.map((d) => <ChangeRow key={`${d.kind}/${d.name}`} d={d} onOpen={open} />)}</ul>}
      </div>
      <div className="b-apply">
        {heavy.length > 0 && (
          <label className="b-ack">
            <input type="checkbox" checked={ack} onChange={(e) => setAck(e.target.checked)} />
            <span>I accept: {heavy.map((d) => `${d.kind} ${d.name} ${d.effect}`).join('; ')}</span>
          </label>)}
        <label className="b-field">
          <span className="b-label">message</span>
          <input placeholder={proposed || 'what this deployment does'} value={touched ? message : proposed}
            onChange={(e) => { setTouched(true); setMessage(e.target.value); }} />
        </label>
        <div className="b-row">
          <button className="primary" disabled={!!block} onClick={() => onApply(touched ? message : proposed, setBusy, setErr)}>
            <Icon name="check" size={13} /> {isNew ? `Create ${draft.name || 'the environment'} with deployment 1` : `Deploy as deployment ${next}`}</button>
          {block && <span className="muted small">{block}</span>}
        </div>
        {err && <p className="bad small">{err}</p>}
      </div>
    </div>
  );
}

// ChangeRow is one object's change in the review: what, its effect, and
// its fields from → to.
function ChangeRow({ d, onOpen }) {
  const heavy = heavyEffect(d.effect);
  const tone = heavy ? 'bad' : d.effect === 'in place' ? 'ok' : 'warn';
  return (
    <li className="b-change">
      <div className="b-changehead">
        <span className={'g-mark m-' + d.change} title={MARK[d.change] ? MARK[d.change][1] : d.change}>{MARK[d.change] ? MARK[d.change][0] : '~'}</span>
        <button className="b-link" onClick={() => onOpen(d.kind, d.name)}>{d.kind} <span className="mono">{d.name}</span></button>
        <span className="muted small">{d.change === 'add' ? 'added' : d.change === 'remove' ? 'removed' : 'changed'}</span>
        <span className="spacer" />
        {d.effect && <span className={'badge ' + tone}>{d.effect}</span>}
      </div>
      {(d.fields || []).length > 0 && (
        <div className="b-fields">
          {d.fields.map((f) => (
            <div key={f.path} className="b-fieldchange small">
              <span className="mono muted">{f.path}</span>
              <span className="mono b-from" title={fmtValue(f.from)}>{fmtValue(f.from)}</span>
              <span className="muted">→</span>
              <span className="mono b-to" title={fmtValue(f.to)}>{fmtValue(f.to)}</span>
            </div>))}
        </div>)}
    </li>
  );
}

function Applied({ applied, tenant, env, st, navigate, onAgain }) {
  const rollout = st && st.rollout && st.rollout.deployment === applied.number ? st.rollout : null;
  return (
    <div className="b-applied">
      <h3><Icon name="check" size={15} /> Deployment {applied.number} made</h3>
      <p>
        {rollout ? <>Its rollout: <span className="mono">{rollout.state}</span>{rollout.reason ? ` · ${rollout.reason}` : ''}</>
          : <>It waits for its rollout ({applied.state}).</>}
      </p>
      {(applied.diff || []).length > 0 && <ul className="b-changes">{applied.diff.map((d) => (
        <ChangeRow key={`${d.kind}/${d.name}`} d={d} onOpen={() => navigate(envPath(tenant, env, 'deployments', applied.number))} />))}</ul>}
      <div className="b-row">
        <button onClick={() => navigate(envPath(tenant, env))}>The overview</button>
        <button onClick={() => navigate(envPath(tenant, env, 'deployments', applied.number))}>Deployment {applied.number}</button>
        <button className="primary" onClick={onAgain}><Icon name="plus" size={13} /> New deployment</button>
      </div>
    </div>
  );
}
