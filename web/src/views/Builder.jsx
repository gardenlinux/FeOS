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
  addService, decodeDraft, deployBlock, draftKey, draftWords, encodeDraft, getObject, isDnsLabel, isObject, isSecretName,
  keyOfKind, newEnvID, objectsOf, parseSpecText, proposeMessage, removeObject, renameObject, setObject, specText, uniqueName,
} from './draft.js';
import { refLine, referencesTo, removeWithRefs, secretNames } from './refs.js';
import { groupFindings, severityCounts, unitKey, worst } from './findings.js';
import { changesSince, rebase, resolve } from './rebase.js';
import { applyPins, pinSecret, secretPins } from './pins.js';
import { builtinCatalog, newAddon, readCatalog } from './catalog.js';
import { planner } from './planner.js';
import {
  AddonForm, BindingForm, FindingList, JobForm, NewBinding, NewSecret, RouteForm, SecretForm, ServiceForm,
} from './BuilderForms.jsx';
import { edgesByCaller, egressOfAnswer, egressWords, fromWords, portWords, refOf } from './reach.js';
import { GROUPS, MARK, itemOfUnit, listGroups, shortImage, summaryOf, unitOfItem } from './fields.js';
import '../styles/builder.css';

// The deployment builder (ADR 0032): an environment's "New deployment"
// and the page of a new environment. It edits a draft of the next
// deployment, a spec in the API's JSON and its base (the deployment it
// starts from), in forms and as JSON text (two views of the one draft),
// keeps it in this browser's storage, plans it 500 ms after every change,
// and deploys it with its base after a review; a refused base rebases
// object by object. A band above says what the draft is, how many objects
// it changes and that nothing changes until it is deployed. Below it the
// draft's objects as a list, by kind, and the selected object's form (or
// the review, or an overview of the draft) beside it; on a narrow screen
// the list first, an object's form on its own. The address names the
// selected object (…/builder/<kind>/<name>). It writes deployments and
// secrets' values, nothing else.

const HISTORY = 100;

// ObjectNav is the draft's objects by kind: each row its name, a line
// (image tag, addon type and plan, route path → service, binding's
// credential), its change against the base (+ ~ −) and its findings.
export function ObjectNav({ spec, base, groups, open, onOpen, onAdd, pins, catalog, marks }) {
  const list = useMemo(() => listGroups(spec, base || {}, marks), [spec, base, marks]);
  const pinByName = new Map((pins || []).map((p) => [p.name, p]));
  return (
    <nav className="b-nav" aria-label="the draft's objects">
      <button className={'b-navtop' + (open === '' ? ' on' : '')} onClick={() => onOpen('')}>
        <Icon name="layers" size={13} /> Overview</button>
      {list.map((g) => (
        <div key={g.key} className="b-group">
          <div className="b-grouphead">
            <span className="b-grouptitle">{g.title}</span>
            <span className="count">{g.rows.filter((r) => !r.removed).length}</span>
            <span className="spacer" />
            {onAdd && (g.key === 'addons'
              ? <AddAddonButton catalog={catalog} onAdd={(type) => onAdd('addons', type)} />
              : <button className="ghost mini b-add" title={`add a ${g.one}`} onClick={() => onAdd(g.key)}><Icon name="plus" size={11} /> Add</button>)}
          </div>
          {g.rows.map((r) => {
            const fs = groups.objects.get(r.unit) || [];
            const c = severityCounts(fs);
            const pin = pinByName.get(r.name);
            const obj = r.key === 'secrets' ? null : getObject(spec, r.key, r.name);
            const line = r.removed ? 'removed by this draft' : summaryOf(r.key, r.key === 'secrets' ? null : obj, r.key === 'secrets' ? pin : null);
            const sev = worst(fs);
            return (
              <button key={r.unit} className={'b-row-obj' + (open === r.unit ? ' on' : '') + (r.removed ? ' gone' : '') + (sev ? ' sev-' + sev : '')}
                onClick={() => onOpen(r.unit)} aria-current={open === r.unit ? 'true' : undefined}>
                <span className="b-objline">
                  <span className={'b-mark' + (r.mark ? ' m-' + r.mark : '')} title={r.mark ? MARK[r.mark][1] + ' by this draft' : undefined}>
                    {r.mark ? MARK[r.mark][0] : ''}</span>
                  <span className="mono b-objname">{r.name}</span>
                  <span className="spacer" />
                  {c.error > 0 && <span className="b-fcount sev-error" title={`${c.error} error${c.error === 1 ? '' : 's'} in the plan`}>{c.error}</span>}
                  {c.warning > 0 && <span className="b-fcount sev-warning" title={`${c.warning} warning${c.warning === 1 ? '' : 's'}`}>{c.warning}</span>}
                  {r.key === 'secrets' && pin && pin.missing && <span className="b-fcount sev-error" title="no value yet">!</span>}
                </span>
                <span className={'b-objsum small' + (r.key === 'routes' || r.key === 'services' || r.key === 'jobs' ? ' mono' : '')}>{line}</span>
              </button>
            );
          })}
          {g.rows.length === 0 && <p className="b-none muted small">none</p>}
        </div>
      ))}
    </nav>
  );
}

function AddAddonButton({ catalog, onAdd }) {
  const [open, setOpen] = useState(false);
  return (
    <span className="b-addaddon">
      <button className="ghost mini b-add" title="add an addon from the catalog" aria-expanded={open} onClick={() => setOpen((o) => !o)}>
        <Icon name="plus" size={11} /> Add</button>
      {open && (
        <span className="b-pop right b-catalog">
          <span className="muted small">from the catalog</span>
          {(catalog ? catalog.types : []).map((t) => (
            <button key={t.type} className="ghost b-catitem" onClick={() => { setOpen(false); onAdd(t.type); }}>
              <span className="strong">{t.type}</span>
              <span className="muted small">{[t.defaultVersion && `version ${t.defaultVersion}`, t.plans.length && `${t.plans.length} plans`].filter(Boolean).join(' · ')}</span>
            </button>))}
          {!catalog && <span className="muted small">reading the catalog…</span>}
        </span>)}
    </span>
  );
}

// ---------------------------------------------------------------- builder --

export function Builder({ tenant, env, st, navigate, item = '' }) {
  const isNew = !env;
  // What the role allows (ADR 0031, ADR 0035): a new environment needs
  // write on the tenant, a deployment, a secret's value and an attached
  // service write on the environment (the tenant grant or the
  // environment's, the higher). Below that the builder shows the latest
  // deployment's spec and changes nothing: no plan (it needs write), no
  // apply, no secret written, no draft stored.
  const access = useAccess(tenant);
  const readOnly = !mayBuild(access, isNew ? '' : (st && st.id) || env);
  const editAttached = !readOnly;
  const key = draftKey(tenant, env);
  const [draft, setDraftState] = useState(null);
  const draftRef = useRef(null);
  const [baseSpec, setBaseSpec] = useState(null);
  const [loadErr, setLoadErr] = useState('');
  const [storageOk, setStorageOk] = useState(true);
  const [view, setView] = useState('forms');
  const [open, setOpenState] = useState(() => unitOfItem(item));
  const openRef = useRef(open);
  openRef.current = open;
  const [reviewing, setReviewing] = useState(false);
  const history = useRef({ past: [], future: [], at: 0 });
  const [, setHist] = useState(0);
  const [plan, setPlan] = useState(null);
  const [planning, setPlanning] = useState(false);
  const [secrets, setSecrets] = useState(null);
  const [secretsErr, setSecretsErr] = useState('');
  const [credentials, setCredentials] = useState(null);
  const [credentialsErr, setCredentialsErr] = useState('');
  const pending = useRef(new Map());
  const [, bumpPending] = useState(0);
  const [catalog, setCatalog] = useState(null);
  // The environment's sandboxes: where its attached services may run.
  const [sandboxes, setSandboxes] = useState(null);
  const [conflicts, setConflicts] = useState(null);
  const [notice, setNotice] = useState('');
  const [applied, setApplied] = useState(null);

  // setOpen selects an object (or the overview: ''): its form replaces
  // the review, and the address names it, so that a link opens it.
  const setOpen = useCallback((u) => {
    setOpenState(u);
    setReviewing(false);
    if (!isNew && navigate) {
      const to = envPath(tenant, env, 'builder', itemOfUnit(u));
      if (typeof location === 'undefined' || to !== location.pathname) navigate(to);
    }
  }, [isNew, navigate, tenant, env]);
  // The address changed (back, a link): select what it names.
  useEffect(() => {
    if (itemOfUnit(openRef.current) !== String(item || '')) { setOpenState(unitOfItem(item)); setReviewing(false); }
  }, [item]);

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
  // editSpec applies fn to the draft's latest spec: a form's change across
  // objects never writes over another that came first.
  const editSpec = useCallback((fn) => edit(fn(draftRef.current.spec)), [edit]);

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

  // The secrets (names and versions), the tenant's credentials and the
  // catalog.
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
    if (isNew) { setSandboxes([]); return undefined; }
    let live = true;
    api.listSandboxes(env).then((r) => { if (live) setSandboxes(r.sandboxes || []); }, () => { if (live) setSandboxes(null); });
    return () => { live = false; };
  }, [env, isNew]);
  useEffect(() => {
    let live = true;
    api.listCredentials().then(
      (r) => { if (live) setCredentials((r && r.credentials) || []); },
      (e) => { if (live) { setCredentials([]); setCredentialsErr(e.message); } },
    );
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
      edit(pinSecret(draftRef.current.spec, name, 0));
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
  // The draft's changes against its base, object by object: the list's
  // marks and the band's count, at once; the plan's diff says their
  // effects.
  const changes = useMemo(() => (baseSpec && draft ? changesSince(baseSpec, spec) : []), [baseSpec, spec]); // eslint-disable-line react-hooks/exhaustive-deps
  const marks = useMemo(() => new Map(changes.map((c) => [c.unit, c.change])), [changes]);
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
      const r = addService(s, '', 8080);
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
    } else if (k === 'bindings' || k === 'secrets') {
      setOpen(`${k}/`);
    }
  };

  const exportDraft = () => download(`${env || draft.name || 'new-environment'}.json`, specText(spec));
  const discard = (n) => {
    setConflicts(null);
    setNotice('');
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

  const empty = !readOnly && objectsOf(spec, 'services').length === 0 && objectsOf(spec, 'jobs').length === 0;
  const ctx = {
    spec, catalog, setSecret, isNew, knownSecrets, pins, credentials, credentialsErr, editSpec, editAttached, tenant, navigate,
    sandboxes,
  };
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
  const paneFirst = !!open || reviewing || empty;
  const pane = reviewing && review ? review : (
    <fieldset className="b-fieldset" disabled={readOnly}>
      <Editor unit={open} spec={spec} base={baseSpec || {}} groups={groups} pins={pins} ctx={ctx} marks={marks} edit={edit}
        draftRef={draftRef} setOpen={setOpen} empty={empty} st={st} isNew={isNew} readOnly={readOnly} words={words}
        onFirst={(image, port) => { const r = addService(draftRef.current.spec, image, port); edit(r.spec); setOpen(unitKey('services', r.name)); }} />
    </fieldset>
  );

  return (
    <div className={'builder' + (paneFirst ? ' pane-first' : '')}>
      <Band words={words} readOnly={readOnly} isNew={isNew} draft={draft} plan={plan} planning={planning || stale}
        changes={changes.length} view={view} setView={setView}
        undo={history.current.past.length ? undo : null} redo={history.current.future.length ? redo : null}
        onExport={exportDraft} onImport={(s) => { edit(s); setNotice('Imported: the review shows what it changes against the base.'); }}
        onError={setNotice} onDiscard={discard} latest={latest}
        onName={(name) => set({ ...draftRef.current, name }, { record: false })}
        onRebase={() => rebaseOnto(draftRef.current, latest).catch((e) => setNotice(e.message))}
        reviewing={reviewing} onReview={readOnly ? null : () => { setView('forms'); setReviewing((r) => !r); }} conflicts={!!conflicts} />
      {!storageOk && <p className="notice small">This browser keeps no storage for this page: the draft lives in memory and is gone with the page. Export it to keep it.</p>}
      {notice && <div className="message"><Icon name="alert" size={14} /><span>{notice}</span>
        <span className="spacer" /><button className="iconbtn" title="dismiss" onClick={() => setNotice('')}><Icon name="x" size={12} /></button></div>}
      {conflicts && (
        <Conflicts conflicts={conflicts} onPick={(c, choice) => {
          edit(resolve(draftRef.current.spec, c, choice));
          const rest = conflicts.list.filter((x) => x !== c);
          setConflicts(rest.length ? { ...conflicts, list: rest } : null);
        }} />)}
      {secretsErr && <p className="notice small">The secrets cannot be read: {secretsErr}</p>}
      {groups.global.length > 0 && view === 'forms' && (
        <div className="b-global"><span className="b-label">the plan, about the draft as a whole</span><FindingList list={groups.global} /></div>)}

      {view === 'json'
        ? <JsonView spec={spec} onSpec={(s) => edit(s)} readOnly={readOnly} />
        : (
          <div className="b-split">
            <ObjectNav spec={spec} base={baseSpec} groups={groups} open={reviewing ? '\u0000' : open} pins={pins} catalog={catalog} marks={marks}
              onOpen={setOpen} onAdd={readOnly ? null : addObject} />
            <div className="b-pane">
              <button className="ghost mini b-back" onClick={() => setOpen('')}><Icon name="arrowLeft" size={12} /> All objects</button>
              {pane}
            </div>
          </div>)}
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
          <button className={'ghost mini' + (view === 'forms' ? ' on' : '')} aria-pressed={view === 'forms'} onClick={() => setView('forms')}>Forms</button>
          <button className={'ghost mini' + (view === 'json' ? ' on' : '')} aria-pressed={view === 'json'} onClick={() => setView('json')}>JSON</button>
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
      <input className={'mono' + (name && !ok ? ' bad-input' : '')} placeholder="my-app" value={name || ''} aria-label="environment name"
        onChange={(e) => onChange(e.target.value.trim())} />
      {name && !ok && <span className="bad small">a DNS label: a-z, 0-9 and -</span>}
    </span>
  );
}

// Editor is the selected object's form; nothing selected, the draft's
// overview, or an empty draft's first step.
function Editor({ unit, spec, base, groups, pins, ctx, marks, edit, draftRef, setOpen, empty, st, isNew, readOnly, words, onFirst }) {
  const [confirm, setConfirm] = useState(null);
  useEffect(() => setConfirm(null), [unit]);
  if (!unit) {
    if (empty) return <FirstService onAdd={onFirst} isNew={isNew} />;
    return <Overview spec={spec} st={st} isNew={isNew} words={words} groups={groups} pins={pins} onOpen={setOpen} />;
  }
  const latest = () => draftRef.current.spec;
  if (unit === 'secrets/') {
    return <NewSecret ctx={{ ...ctx, findings: [] }} onDone={(n) => setOpen(unitKey('secrets', n))} />;
  }
  if (unit === 'bindings/') {
    return <NewBinding ctx={{ ...ctx, findings: [] }} onAdd={(r) => { edit(r.spec); setOpen(unitKey('bindings', r.name)); }} />;
  }
  const i = unit.indexOf('/');
  const key = unit.slice(0, i);
  const name = unit.slice(i + 1);
  const fctx = { ...ctx, findings: groups.objects.get(unit) || [] };
  const onChange = (o) => edit(setObject(latest(), key, name, o));
  const onRename = (to) => {
    const s = latest();
    const taken = objectsOf(s, key).some(([n]) => n === to)
      || ((key === 'services' || key === 'addons') && objectsOf(s, key === 'services' ? 'addons' : 'services').some(([n]) => n === to));
    if (taken) return;
    edit(renameObject(s, key, name, to));
    setOpen(unitKey(key, to));
  };
  const onRemove = readOnly ? null : () => {
    const refs = referencesTo(latest(), key, name).filter((r) => !(r.from.key === key && r.from.name === name));
    if (refs.length === 0) { edit(removeObject(latest(), key, name)); setOpen(''); return; }
    setConfirm(refs);
  };
  const confirmBox = confirm && (
    <div className="message b-confirm"><Icon name="alert" size={14} />
      <span>{confirm.length} reference{confirm.length === 1 ? '' : 's'} name {name}: {confirm.map((r) => `${r.from.name} · ${refLine(r)}`).join('; ')}.</span>
      <span className="b-row wrap">
        <button className="mini danger" onClick={() => { edit(removeWithRefs(latest(), key, name)); setOpen(''); }}>Remove them too</button>
        <button className="mini" onClick={() => { edit(removeObject(latest(), key, name)); setOpen(''); }}>Keep them (the plan names them)</button>
        <button className="ghost mini" onClick={() => setConfirm(null)}>Cancel</button>
      </span>
    </div>
  );
  if (key === 'secrets') {
    const known = secretNames(spec).includes(name);
    if (!known) return <Removed unit={unit} kind="secret" name={name} base={base} edit={edit} latest={latest} />;
    const pin = pins.find((p) => p.name === name) || { name, pinned: 0, latest: 0, missing: true };
    return (
      <>
        <SecretForm name={name} pin={pin} refs={referencesTo(spec, 'secrets', name)} ctx={fctx}
          onPin={(v) => edit(pinSecret(latest(), name, v))} onRemove={onRemove} />
        {confirmBox}
      </>);
  }
  const obj = getObject(spec, key, name);
  if (obj === undefined) {
    return <Removed unit={unit} kind={GROUPS.find((g) => g.key === key)?.one || key} name={name} base={base} edit={edit} latest={latest} />;
  }
  const props = { name, obj, ctx: fctx, onChange, onRename: readOnly ? null : onRename, onRemove };
  return (
    <>
      {key === 'services' && <ServiceForm {...props} />}
      {key === 'jobs' && <JobForm {...props} />}
      {key === 'addons' && <AddonForm {...props} added={marks.get(unit) === 'add'} />}
      {key === 'routes' && <RouteForm {...props} />}
      {key === 'bindings' && <BindingForm {...props} />}
      {confirmBox}
    </>
  );
}

// Removed is an object of the base the draft removes: what it was, and
// the way back.
function Removed({ unit, kind, name, base, edit, latest }) {
  const i = unit.indexOf('/');
  const key = unit.slice(0, i);
  const was = getObject(base, key, name);
  if (was === undefined) return <p className="empty">No {kind} {name} in the draft.</p>;
  return (
    <div className="b-form">
      <header className="b-formhead"><div className="b-formtitle"><span className="kind">{kind}</span><h2 className="mono">{name}</h2>
        <span className="badge bad">removed by this draft</span></div></header>
      <section className="b-sect"><div className="b-sectbody">
        <p className="small">The next deployment removes it. As the base has it:</p>
        <pre className="specpre b-raw">{JSON.stringify(was, null, 2)}</pre>
        <span><button onClick={() => edit(setObject(latest(), key, name, was))}><Icon name="undo" size={13} /> Restore it</button></span>
      </div></section>
    </div>
  );
}

// Overview is the draft with nothing selected: what the environment runs
// after the deployment (the groups with counts, the services and addons),
// and how it is reached (its hosts, the routes).
function Overview({ spec, st, isNew, words, groups, pins, onOpen }) {
  const services = objectsOf(spec, 'services');
  const addons = objectsOf(spec, 'addons');
  const routes = objectsOf(spec, 'routes').sort(([, a], [, b]) => String((a && a.path) || '').localeCompare(String((b && b.path) || '')));
  const counts = GROUPS.map((g) => [g, g.key === 'secrets' ? secretNames(spec).length : objectsOf(spec, g.key).length]);
  const hosts = st && st.public ? st.hosts || [] : [];
  const missing = (pins || []).filter((p) => p.missing).length;
  const sev = (u) => worst(groups.objects.get(u) || []);
  const link = (u, label) => <button className={'b-link mono' + (sev(u) ? ' sev-' + sev(u) : '')} onClick={() => onOpen(u)}>{label}</button>;
  return (
    <div className="b-form b-overview">
      <header className="b-formhead"><div className="b-formtitle"><span className="kind">Overview</span>
        <h2>{isNew ? 'The new environment runs' : `Deployment ${words.next} runs`}</h2></div></header>
      <div className="b-counts">
        {counts.map(([g, n]) => (
          <span key={g.key} className={'b-countchip' + (n ? '' : ' none')}><b>{n}</b> {n === 1 ? g.one : g.title.toLowerCase()}</span>))}
        {missing > 0 && <span className="b-countchip bad"><b>{missing}</b> secret{missing === 1 ? '' : 's'} without a value</span>}
      </div>
      <section className="b-sect">
        <header className="b-secthead"><h3 className="b-sectitle">Entry</h3>
          <span className="muted small b-secthint">{isNew ? 'public once the environment admin makes it so' : hosts.length ? 'public' : 'not public: the route door only'}</span></header>
        <div className="b-sectbody">
          {hosts.length > 0 && <div className="b-hosts">{hosts.map((h) => <span key={h} className="mono b-host">{h}</span>)}</div>}
          {routes.length === 0 ? <p className="muted small">No routes: nothing reaches the environment from outside.</p> : (
            <div className="tblwrap"><table className="tbl b-tbl">
              <thead><tr><th>path</th><th>service</th><th>port</th><th /></tr></thead>
              <tbody>{routes.map(([n, r]) => (
                <tr key={n}><td>{link(unitKey('routes', n), (r && r.path) || '?')}</td><td className="mono">{r && r.service}</td>
                  <td className="mono">{r && r.port !== undefined ? String(r.port) : ''}</td><td className="muted small">{r && r.strip ? 'strips the path' : ''}</td></tr>))}
              </tbody></table></div>)}
        </div>
      </section>
      <section className="b-sect">
        <header className="b-secthead"><h3 className="b-sectitle">Services</h3></header>
        <div className="b-sectbody">
          {services.length === 0 ? <p className="muted small">None.</p> : (
            <div className="tblwrap"><table className="tbl b-tbl">
              <thead><tr><th>name</th><th>image</th><th>replicas</th><th>size</th><th>health check</th></tr></thead>
              <tbody>{services.map(([n, o]) => {
                const r = isObject(o) && isObject(o.resources) ? o.resources : {};
                const h = isObject(o) && isObject(o.readiness) ? (o.readiness.http ? `HTTP ${o.readiness.http.path || '/'}` : o.readiness.tcp ? 'TCP' : o.readiness.exec ? 'command' : '') : '';
                return (
                  <tr key={n}><td>{link(unitKey('services', n), n)}</td>
                    <td className="mono small">{isObject(o) && o.runtime === 'attached' ? `attached · ${o.sandbox || '—'}` : shortImage(o) || <span className="bad">no image</span>}</td>
                    <td className="mono">{isObject(o) && o.replicas !== undefined ? o.replicas : 1}</td>
                    <td className="mono small">{r.vcpus || 1} vCPU · {r.memoryMib || 512} MiB</td>
                    <td className="small">{h || <span className="muted">none</span>}</td></tr>);
              })}</tbody></table></div>)}
        </div>
      </section>
      {addons.length > 0 && (
        <section className="b-sect">
          <header className="b-secthead"><h3 className="b-sectitle">Addons</h3></header>
          <div className="b-sectbody"><div className="tblwrap"><table className="tbl b-tbl">
            <thead><tr><th>name</th><th>type</th><th>version</th><th>plan</th></tr></thead>
            <tbody>{addons.map(([n, a]) => (
              <tr key={n}><td>{link(unitKey('addons', n), n)}</td><td className="mono">{a && a.type}</td><td className="mono">{(a && a.version) || ''}</td>
                <td className="mono">{(a && a.plan) || <span className="muted">default</span>}</td></tr>))}
            </tbody></table></div></div>
        </section>)}
      <p className="muted small">Select an object on the left to change it; Review &amp; deploy shows what the changes do.</p>
    </div>
  );
}

// FirstService is an empty environment's first step: an image and a port
// named http, with the route / to it (ADR 0032).
function FirstService({ onAdd, isNew }) {
  const [image, setImage] = useState('');
  const [port, setPort] = useState(8080);
  const go = () => { if (image) onAdd(image.trim(), Number(port) || 8080); };
  return (
    <div className="b-form b-first">
      <header className="b-formhead"><div className="b-formtitle"><span className="kind">First step</span><h2>Add a service</h2></div></header>
      <p className="small">{isNew ? 'A new environment starts with one service: ' : 'The draft is empty. Start with a service: '}
        an image from any registry and the port it listens on. The builder names the port <span className="mono">http</span> and
        routes <span className="mono">/</span> to it; then Review &amp; deploy.</p>
      <div className="b-firstrow">
        <label className="b-field grow"><span className="b-label">Image</span>
          <input className="mono" placeholder="ghcr.io/acme/web:1.0" value={image} autoFocus aria-label="image"
            onChange={(e) => setImage(e.target.value)} onKeyDown={(e) => { if (e.key === 'Enter') go(); }} /></label>
        <label className="b-field"><span className="b-label">Port</span>
          <input type="number" className="num" min={1} max={65535} value={port} aria-label="port" onChange={(e) => setPort(e.target.value)}
            onKeyDown={(e) => { if (e.key === 'Enter') go(); }} /></label>
        <button className="primary" disabled={!image.trim()} onClick={go}><Icon name="plus" size={13} /> Add the service</button>
      </div>
      <ol className="b-steps small muted">
        <li><b>service</b> <span className="mono">{image ? image.split('/').pop().split(/[:@]/)[0] || 'web' : 'web'}</span> runs the image, port http {port || 8080}</li>
        <li><b>route</b> <span className="mono">/</span> → it</li>
        <li>addons, more services, jobs and secrets: <b>Add</b> in the list on the left, at any time</li>
      </ol>
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
      <textarea className={'mono b-text' + (err ? ' bad-input' : '')} spellCheck={false} value={text} readOnly={readOnly} aria-label="the draft as JSON"
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
  const added = changes.map((c) => {
    const i = c.unit.indexOf('/');
    const key = c.unit.slice(0, i);
    return { key, kind: (GROUPS.find((g) => g.key === key) || { kind: key }).kind, name: c.unit.slice(i + 1), change: c.change, unit: c.unit };
  });
  const proposed = proposeMessage(isNew ? added : diff, draft.spec);
  useEffect(() => { setAck(false); }, [heavy.length]);
  const all = applyFindings || (answer && answer.findings) || findings;
  const counts = severityCounts(all);
  const unresolved = pinned ? pinned.unresolved : [];
  const missing = (pins || []).filter((p) => p.missing).map((p) => p.name);
  const block = deployBlock({ isNew, name: draft.name, planning, answer, diff, unresolved, heavy: heavy.length, ack, conflicts, busy });
  const open = (kind, name) => {
    const key = keyOfKind(kind);
    if (key) onOpen(unitKey(key, name));
  };
  return (
    <div className="b-form b-review">
      <header className="b-formhead">
        <div className="b-formtitle">
          <span className="kind">Review</span>
          <h2>{isNew ? `Deployment 1 of ${draft.name || 'the new environment'}` : `Deployment ${next}`}</h2>
          <span className="muted small">{isNew ? 'everything is added' : `against deployment ${draft.base || '(none)'}`}</span>
          <span className="spacer" />
          {onClose && <button className="iconbtn" title="back to the forms" onClick={onClose}><Icon name="x" size={13} /></button>}
        </div>
        <div className="b-planhead">
          {planning && <span className="muted small">planning…</span>}
          {!planning && answer && (answer.valid
            ? <span className="badge ok">the plan accepts it</span>
            : <span className="badge bad">{counts.error} error{counts.error === 1 ? '' : 's'}</span>)}
          {!planning && answer && counts.warning > 0 && <span className="badge warn">{counts.warning} warning{counts.warning === 1 ? '' : 's'}</span>}
          {plan && plan.error && <span className="bad small">{plan.error}</span>}
        </div>
      </header>
      {all.length > 0 && (
        <section className="b-sect">
          <header className="b-secthead"><h3 className="b-sectitle">Findings</h3>
            {counts.error > 0 && <span className="bad small">errors block the deploy</span>}</header>
          <div className="b-sectbody"><FindingList list={all} /></div>
        </section>)}
      {missing.length > 0 && !isNew && (
        <p className="bad small">Secrets without a value: {missing.join(', ')}. Open each under Secrets to set one.</p>)}
      {pinned === null && answer && (
        <p className="panel-note">This ferund answers no image pins with a plan; it pins the images at deploy.</p>)}
      {unresolved.length > 0 && (
        <p className="bad small">These images do not resolve, so the builder does not deploy: {unresolved.join(', ')}</p>)}
      <section className="b-sect">
        <header className="b-secthead"><h3 className="b-sectitle">Changes</h3>
          {heavy.length > 0 && <span className="bad small">{heavy.length} with downtime or data loss</span>}</header>
        <div className="b-sectbody">
          {isNew
            ? (added.length === 0
              ? <p className="empty small">Nothing in the draft yet.</p>
              : <ul className="b-changes">{added.map((c) => (
                <li key={c.unit} className="b-change"><div className="b-changehead"><span className={'b-mark m-' + c.change}>{MARK[c.change][0]}</span>
                  <button className="b-link" onClick={() => open(c.kind, c.name)}>{c.kind} <span className="mono">{c.name}</span></button>
                  <span className="spacer" /><span className="muted small">added</span></div></li>))}</ul>)
            : diff.length === 0
              ? <p className="empty small">{answer ? 'Nothing changes against the base.' : 'Not planned yet.'}</p>
              : <ul className="b-changes">{diff.map((d) => <ChangeRow key={`${d.kind}/${d.name}`} d={d} onOpen={open} />)}</ul>}
        </div>
      </section>
      {answer && answer.reachability && <PlanReach reach={answer.reachability} onOpen={open} />}
      <section className="b-sect b-apply">
        <div className="b-sectbody">
          {heavy.length > 0 && (
            <label className="b-ack">
              <input type="checkbox" checked={ack} onChange={(e) => setAck(e.target.checked)} />
              <span>I accept: {heavy.map((d) => `${d.kind} ${d.name} ${d.effect}`).join('; ')}</span>
            </label>)}
          <label className="b-field">
            <span className="b-label">Message</span>
            <input placeholder={proposed || 'what this deployment does'} value={touched ? message : proposed}
              onChange={(e) => { setTouched(true); setMessage(e.target.value); }} />
          </label>
          <div className="b-row wrap">
            <button className="primary" disabled={!!block} onClick={() => onApply(touched ? message : proposed, setBusy, setErr)}>
              <Icon name="check" size={13} /> {isNew ? `Create ${draft.name || 'the environment'} with deployment 1` : `Deploy as deployment ${next}`}</button>
            {block && <span className="muted small">{block}</span>}
          </div>
          {err && <p className="bad small">{err}</p>}
        </div>
      </section>
    </div>
  );
}

// PlanReach is the plan's reachability in the review (ADR 0034,
// revised): every edge its references open, by caller, with where each
// comes from; the routes' clients; the egress that is not the internet.
export function PlanReach({ reach, onOpen = () => {} }) {
  const callers = edgesByCaller(reach);
  const routes = (Array.isArray(reach.routes) ? reach.routes : []).filter((r) => r && Array.isArray(r.from) && r.from.length);
  const egress = (Array.isArray(reach.egress) ? reach.egress : []).filter((x) => x && x.mode && x.mode !== 'internet');
  const n = callers.reduce((a, c) => a + c.edges.length, 0);
  const name = (ref) => {
    const r = refOf(ref);
    return r.kind === 'ingress' ? <span className="muted">the ingress</span>
      : <button className="b-link" onClick={() => onOpen(r.kind, r.name)}>{r.kind} <span className="mono">{r.name}</span></button>;
  };
  return (
    <section className="b-sect">
      <header className="b-secthead"><h3 className="b-sectitle">Reachability</h3>
        <span className="muted small">{n} edge{n === 1 ? '' : 's'}: what each object references, it reaches; nothing else gets in</span></header>
      <div className="b-sectbody">
        {n === 0 && <p className="empty small">No object references another: nothing reaches anything.</p>}
        <ul className="b-changes rc-plan">
          {callers.map((c) => (
            <li key={c.from} className="b-change">
              <div className="b-changehead">{name(c.from)}<span className="muted small">reaches</span></div>
              <div className="b-fields">
                {c.edges.map((e) => (
                  <div key={`${e.to}:${e.number}`} className="b-fieldchange small rc-edge">
                    <span className="mono">{refOf(e.to).name}</span>
                    <span className="mono muted">{portWords(e.port, e.number)}</span>
                    <span className="muted">{e.via}</span>
                  </div>))}
              </div>
            </li>))}
        </ul>
        {routes.length > 0 && (
          <p className="small">Routes for some clients only: {routes.map((r) => `${r.route} (${fromWords(r.from)})`).join('; ')}.</p>)}
        {egress.length > 0 && (
          <p className="small">Egress not the internet: {egress.map((x) => `${refOf(x.object).name} ${egressWords(egressOfAnswer(x))}`).join('; ')}.</p>)}
      </div>
    </section>
  );
}

// ChangeRow is one object's change in the review: what, its effect, and
// its fields from → to.
function ChangeRow({ d, onOpen }) {
  const heavy = heavyEffect(d.effect);
  const tone = heavy ? 'bad' : d.effect === 'in place' ? 'ok' : 'warn';
  return (
    <li className={'b-change' + (heavy ? ' heavy' : '')}>
      <div className="b-changehead">
        <span className={'b-mark m-' + d.change} title={MARK[d.change] ? MARK[d.change][1] : d.change}>{MARK[d.change] ? MARK[d.change][0] : '~'}</span>
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

