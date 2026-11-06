// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useEffect, useState } from 'react';
import { Icon } from '../Icon.jsx';
import { isDnsLabel, isObject, isSecretName, objectsOf, registryHost } from './draft.js';
import { SERVICE_KEYS, envValueKind, makeRef, refLine, refTargets, suggestVarName } from './refs.js';
import { atField, notAt } from './findings.js';
import { checkNewer, imageView, setImage } from './pins.js';
import { planText } from './catalog.js';

// The builder's forms (ADR 0032, "Forms, the catalog, a first
// deployment"): one per kind, with the API's fields and its defaults as
// placeholders. Every change is an edit of the draft's JSON: a form gets
// its object and gives back the next one (onChange). Its own checks only
// make a field enterable; the verdict is the plan's, whose findings show
// at their fields.
//
// ctx is what every form reads: the spec, the catalog, the secrets the
// environment has (names), the object's findings, setSecret(name, value)
// (a promise of the version it made), and whether the environment is new.

// ------------------------------------------------------------- widgets --

export function FindingList({ list }) {
  if (!list || list.length === 0) return null;
  return (
    <ul className="b-findings">
      {list.map((f, i) => (
        <li key={i} className={'b-finding ' + f.severity}>
          <span className="b-sev">{f.severity}</span>
          {f.path && <span className="mono small b-fpath">{f.path}</span>}
          <span>{f.problem}</span>
          {f.remedy && <span className="muted"> — {f.remedy}</span>}
        </li>
      ))}
    </ul>
  );
}

function Field({ label, hint, findings, children, wide }) {
  return (
    <div className={'b-field' + (wide ? ' wide' : '')}>
      <span className="b-label">{label}</span>
      <span className="b-control">{children}</span>
      {hint && <span className="b-hint muted small">{hint}</span>}
      <FindingList list={findings} />
    </div>
  );
}

// without is o without the keys whose value is undefined.
function without(o) {
  const out = {};
  for (const [k, v] of Object.entries(o)) if (v !== undefined) out[k] = v;
  return out;
}

// set is o with k set to v; undefined or '' removes it.
function set(o, k, v) {
  return without({ ...o, [k]: v === '' ? undefined : v });
}

function OptNumber({ value, onChange, placeholder, min, max }) {
  return (
    <input type="number" className="num" min={min} max={max} placeholder={placeholder}
      value={value === undefined || value === null ? '' : value}
      onChange={(e) => onChange(e.target.value === '' ? undefined : Number(e.target.value))} />
  );
}

// Lines edits a list of strings, one per line ("" lines dropped).
function Lines({ value, onChange, placeholder, rows = 2 }) {
  const [text, setText] = useState((value || []).join('\n'));
  useEffect(() => {
    if ((value || []).join('\n') !== text.split('\n').filter((l) => l !== '').join('\n')) setText((value || []).join('\n'));
  }, [value]); // eslint-disable-line react-hooks/exhaustive-deps
  return (
    <textarea className="mono b-lines" rows={rows} placeholder={placeholder} value={text}
      onChange={(e) => {
        setText(e.target.value);
        const list = e.target.value.split('\n').filter((l) => l !== '');
        onChange(list.length ? list : undefined);
      }} />
  );
}

// NameInput renames on blur or Enter: a rename moves the object and its
// references, so it is one edit, not one per key.
function NameInput({ name, onRename, check = isDnsLabel, what = 'a DNS label: a-z, 0-9 and -' }) {
  const [v, setV] = useState(name);
  useEffect(() => setV(name), [name]);
  const ok = check(v);
  const done = () => { if (v !== name && ok) onRename(v); else setV(name); };
  return (
    <>
      <input className={'mono' + (ok ? '' : ' bad-input')} value={v} onChange={(e) => setV(e.target.value)}
        onBlur={done} onKeyDown={(e) => { if (e.key === 'Enter') { e.preventDefault(); done(); } }} />
      {!ok && <span className="b-hint bad small">{what}</span>}
    </>
  );
}

function Select({ value, options, onChange, empty }) {
  const opts = [...options];
  if (value && !opts.some((o) => (o.value ?? o) === value)) opts.unshift({ value, label: `${value} (not offered)` });
  return (
    <select value={value ?? ''} onChange={(e) => onChange(e.target.value)}>
      {empty !== undefined && <option value="">{empty}</option>}
      {opts.map((o) => {
        const val = o.value ?? o;
        return <option key={String(val)} value={val}>{o.label ?? String(val)}</option>;
      })}
    </select>
  );
}

// ---------------------------------------------------------- the objects --

// ObjectHead is a form's head: the kind, the name (renamed in place),
// the remove, and the object's findings that no field takes.
function ObjectHead({ kind, name, onRename, onRemove, findings, fields, extra, nameCheck }) {
  return (
    <div className="b-objhead">
      <span className="badge acc">{kind}</span>
      {onRename
        ? <span className="b-name"><NameInput name={name} onRename={onRename} {...(nameCheck || {})} /></span>
        : <span className="mono strong">{name}</span>}
      <span className="spacer" />
      {extra}
      {onRemove && <button className="ghost mini danger" onClick={onRemove}><Icon name="trash" size={12} /> Remove</button>}
      <div className="b-headfindings"><FindingList list={notAt(findings, fields)} /></div>
    </div>
  );
}

const SERVICE_FIELDS = ['image', 'imageAsWritten', 'imageCredentials', 'command', 'env', 'ports', 'replicas', 'resources',
  'readiness', 'idle', 'rollout', 'bindings', 'runtime', 'sandbox', 'cwd', 'mounts'];

export function ServiceForm({ name, obj, ctx, onChange, onRename, onRemove }) {
  const o = isObject(obj) ? obj : {};
  const f = ctx.findings;
  if (o.runtime === 'attached') {
    return (
      <div className="b-form">
        <ObjectHead kind="service" name={name} findings={f} fields={SERVICE_FIELDS} onRemove={onRemove} />
        <p className="message"><Icon name="alert" size={14} />
          <span>An attached service runs in sandbox <span className="mono">{o.sandbox || '—'}</span>. The builder shows it
            and keeps it as it is; edit it in the JSON view.</span></p>
        <FindingList list={f} />
      </div>
    );
  }
  const ports = Array.isArray(o.ports) ? o.ports : [];
  return (
    <div className="b-form">
      <ObjectHead kind="service" name={name} onRename={onRename} onRemove={onRemove} findings={f} fields={SERVICE_FIELDS} />
      <ImageField o={o} onChange={onChange} findings={atField(f, 'image')} />
      <Field label="command" hint="one argument per line; empty: the image's entrypoint and cmd" findings={atField(f, 'command')}>
        <Lines value={o.command} onChange={(v) => onChange(set(o, 'command', v))} placeholder="(the image's)" />
      </Field>
      <Field label="ports" findings={atField(f, 'ports')}>
        <div className="b-rows">
          {ports.map((p, i) => (
            <div key={i} className="b-row">
              <input className="mono" placeholder="name" value={(p && p.name) || ''}
                onChange={(e) => onChange({ ...o, ports: ports.map((q, j) => (j === i ? { ...q, name: e.target.value } : q)) })} />
              <OptNumber value={p && p.port} min={1} max={65535} placeholder="port"
                onChange={(v) => onChange({ ...o, ports: ports.map((q, j) => (j === i ? without({ ...q, port: v }) : q)) })} />
              <button className="iconbtn" title="remove the port"
                onClick={(e) => { e.preventDefault(); onChange(set(o, 'ports', ports.length > 1 ? ports.filter((_, j) => j !== i) : undefined)); }}>
                <Icon name="x" size={13} /></button>
            </div>
          ))}
          <button className="ghost mini" onClick={(e) => {
            e.preventDefault();
            onChange({ ...o, ports: [...ports, { name: ports.length ? `port-${ports.length + 1}` : 'http', port: 8080 + ports.length }] });
          }}><Icon name="plus" size={12} /> port</button>
        </div>
      </Field>
      <div className="b-grid3">
        <Field label="replicas" hint="0 to 20" findings={atField(f, 'replicas')}>
          <OptNumber value={o.replicas} min={0} max={20} placeholder="1" onChange={(v) => onChange(set(o, 'replicas', v))} />
        </Field>
        <ResourcesFields o={o} onChange={onChange} findings={f} />
      </div>
      <ReadinessField o={o} onChange={onChange} findings={atField(f, 'readiness')} />
      <div className="b-grid3">
        <Field label="idle" hint="paused after this without traffic (≥ 1m, a route's target); empty: never" findings={atField(f, 'idle')}>
          <input className="mono" placeholder="never" value={o.idle || ''} onChange={(e) => onChange(set(o, 'idle', e.target.value))} />
        </Field>
        <Field label="rollout timeout" findings={atField(f, 'rollout')}>
          <input className="mono" placeholder="10m" value={(isObject(o.rollout) && o.rollout.timeout) || ''}
            onChange={(e) => onChange(set(o, 'rollout', e.target.value ? { ...(o.rollout || {}), timeout: e.target.value } : undefined))} />
        </Field>
      </div>
      <EnvEditor self={{ key: 'services', name }} obj={o} ctx={ctx} onChange={onChange} findings={f} />
      <RegistryLogin o={o} ctx={ctx} onChange={onChange} findings={atField(f, 'imageCredentials')} />
      <BindingsLine o={o} findings={atField(f, 'bindings')} />
    </div>
  );
}

const JOB_FIELDS = ['image', 'imageAsWritten', 'command', 'env', 'resources', 'imageCredentials', 'on', 'timeout', 'retries', 'bindings'];

export function JobForm({ name, obj, ctx, onChange, onRename, onRemove }) {
  const o = isObject(obj) ? obj : {};
  const f = ctx.findings;
  return (
    <div className="b-form">
      <ObjectHead kind="job" name={name} onRename={onRename} onRemove={onRemove} findings={f} fields={JOB_FIELDS} />
      <ImageField o={o} onChange={onChange} findings={atField(f, 'image')} />
      <Field label="command" hint="one argument per line" findings={atField(f, 'command')}>
        <Lines value={o.command} onChange={(v) => onChange(set(o, 'command', v))} placeholder="(the image's)" />
      </Field>
      <div className="b-grid3">
        <Field label="runs" findings={atField(f, 'on')}>
          <Select value={o.on === 'release' ? 'deploy' : o.on || 'deploy'} options={[
            { value: 'deploy', label: 'with every deployment that changes it' }, { value: 'manual', label: 'when asked' }]}
          onChange={(v) => onChange(set(o, 'on', v))} />
        </Field>
        <Field label="timeout" hint="at most 2h" findings={atField(f, 'timeout')}>
          <input className="mono" placeholder="10m" value={o.timeout || ''} onChange={(e) => onChange(set(o, 'timeout', e.target.value))} />
        </Field>
        <Field label="retries" hint="0 to 5" findings={atField(f, 'retries')}>
          <OptNumber value={o.retries} min={0} max={5} placeholder="0" onChange={(v) => onChange(set(o, 'retries', v))} />
        </Field>
      </div>
      <div className="b-grid3"><ResourcesFields o={o} onChange={onChange} findings={f} /></div>
      <EnvEditor self={{ key: 'jobs', name }} obj={o} ctx={ctx} onChange={onChange} findings={f} />
      <RegistryLogin o={o} ctx={ctx} onChange={onChange} findings={atField(f, 'imageCredentials')} />
      <BindingsLine o={o} findings={atField(f, 'bindings')} />
    </div>
  );
}

// ImageField shows the image as written and its digest while it is
// pinned; "Check for a newer image" puts the image as written back, which
// the plan resolves.
function ImageField({ o, onChange, findings }) {
  const v = imageView(o);
  return (
    <Field label="image" findings={findings}
      hint={v.digest
        ? <>pinned to <span className="mono">{v.digest.slice(0, 19)}…</span>: a change elsewhere does not pull a tag that moved</>
        : 'any registry: the plan resolves it to a digest'}>
      <input className="mono" placeholder="ghcr.io/acme/web:1.0" value={v.written}
        onChange={(e) => onChange(setImage(o, e.target.value))} />
      {v.digest && o.imageAsWritten && (
        <button className="ghost mini" title="the image as written again, which the plan resolves"
          onClick={(e) => { e.preventDefault(); onChange(checkNewer(o)); }}>
          <Icon name="refresh" size={12} /> Check for a newer image</button>)}
    </Field>
  );
}

function ResourcesFields({ o, onChange, findings }) {
  const r = isObject(o.resources) ? o.resources : {};
  const put = (k, v) => {
    const next = without({ ...r, [k]: v });
    onChange(set(o, 'resources', Object.keys(next).length ? next : undefined));
  };
  return (
    <>
      <Field label="vCPUs" hint="1 to 32" findings={atField(findings, 'resources.vcpus')}>
        <OptNumber value={r.vcpus} min={1} max={32} placeholder="1" onChange={(v) => put('vcpus', v)} />
      </Field>
      <Field label="memory (MiB)" hint="256 to 65536" findings={atField(findings, 'resources.memoryMib')}>
        <OptNumber value={r.memoryMib} min={256} max={65536} placeholder="512" onChange={(v) => put('memoryMib', v)} />
      </Field>
    </>
  );
}

function ReadinessField({ o, onChange, findings }) {
  const r = isObject(o.readiness) ? o.readiness : null;
  const kind = !r ? 'none' : r.http ? 'http' : r.tcp ? 'tcp' : r.exec ? 'exec' : 'none';
  const ports = (Array.isArray(o.ports) ? o.ports : []).map((p) => p && (p.name || p.port)).filter(Boolean);
  const timing = r ? without({ initialDelay: r.initialDelay, period: r.period, failureThreshold: r.failureThreshold }) : {};
  const setKind = (k) => {
    if (k === 'none') return onChange(set(o, 'readiness', undefined));
    const port = ports[0] || 'http';
    const check = k === 'http' ? { http: { port, path: '/' } } : k === 'tcp' ? { tcp: { port } } : { exec: { command: [] } };
    onChange({ ...o, readiness: { ...check, ...timing } });
  };
  const put = (patch) => onChange({ ...o, readiness: without({ ...r, ...patch }) });
  return (
    <Field label="readiness" findings={findings}
      hint={kind === 'none' ? 'ready when it still runs 2 s after it was first seen running' : 'traffic once the check passes'}>
      <div className="b-row wrap">
        <Select value={kind} options={['none', 'http', 'tcp', 'exec']} onChange={setKind} />
        {(kind === 'http' || kind === 'tcp') && (
          <Select value={String(r[kind].port ?? '')} options={ports.map(String)}
            onChange={(v) => put({ [kind]: { ...r[kind], port: /^\d+$/.test(v) ? Number(v) : v } })} />)}
        {kind === 'http' && (
          <input className="mono" placeholder="/readyz" value={r.http.path || ''}
            onChange={(e) => put({ http: { ...r.http, path: e.target.value } })} />)}
        {kind === 'exec' && (
          <Lines value={r.exec.command} onChange={(v) => put({ exec: { command: v || [] } })} placeholder="one argument per line" />)}
        {kind !== 'none' && (
          <>
            <input className="mono short" title="initial delay" placeholder="0s" value={r.initialDelay || ''}
              onChange={(e) => put({ initialDelay: e.target.value || undefined })} />
            <input className="mono short" title="period" placeholder="2s" value={r.period || ''}
              onChange={(e) => put({ period: e.target.value || undefined })} />
            <OptNumber value={r.failureThreshold} min={1} placeholder="3" onChange={(v) => put({ failureThreshold: v })} />
          </>)}
      </div>
    </Field>
  );
}

function BindingsLine({ o, findings }) {
  const list = Array.isArray(o.bindings) ? o.bindings : [];
  if (list.length === 0 && (!findings || findings.length === 0)) return null;
  return (
    <Field label="bindings" hint="the bindings' forms come later; edit them in the JSON view" findings={findings}>
      <span className="labels">{list.map((b) => <span key={b} className="labelchip mono">{b}</span>)}</span>
    </Field>
  );
}

// ------------------------------------------------------------ variables --

const KIND_WORDS = { string: 'text', value: 'text', secret: 'secret', addon: 'addon', service: 'service', invalid: 'other' };

// EnvEditor edits the variables, a row each: a name and a value, plain or
// a reference to an addon's output, a service's port or a secret; "Add
// reference" asks for the target and the key, as the graph's drop does.
function EnvEditor({ self, obj, ctx, onChange, findings }) {
  const env = isObject(obj.env) ? obj.env : {};
  const [adding, setAdding] = useState(false);
  const targets = refTargets(ctx.spec, ctx.catalog, { self: self.key === 'services' ? self.name : '', knownSecrets: ctx.knownSecrets });
  const put = (next) => onChange(set(obj, 'env', Object.keys(next).length ? next : undefined));
  const rename = (from, to) => {
    if (!to || to === from || to in env) return;
    const next = {};
    for (const [k, v] of Object.entries(env)) next[k === from ? to : k] = v;
    put(next);
  };
  const free = (base) => { let n = base; for (let i = 2; n in env; i++) n = `${base}_${i}`; return n; };
  return (
    <Field label="variables" wide findings={atField(findings, 'env').filter((f) => f.rest === 'env')}>
      <div className="b-rows">
        {Object.entries(env).map(([k, v]) => (
          <div key={k} className="b-envrow">
            <div className="b-row wrap">
              <VarName name={k} onRename={(to) => rename(k, to)} />
              <ValueEditor v={v} targets={targets} ctx={ctx} onChange={(nv) => put({ ...env, [k]: nv })} />
              <button className="iconbtn" title="remove the variable" onClick={(e) => {
                e.preventDefault();
                const next = { ...env };
                delete next[k];
                put(next);
              }}><Icon name="x" size={13} /></button>
            </div>
            <FindingList list={atField(findings, `env.${k}`)} />
          </div>
        ))}
        <div className="b-row">
          <button className="ghost mini" onClick={(e) => { e.preventDefault(); put({ ...env, [free('NAME')]: '' }); }}>
            <Icon name="plus" size={12} /> variable</button>
          <button className="ghost mini" onClick={(e) => { e.preventDefault(); setAdding((a) => !a); }}>
            <Icon name="plus" size={12} /> reference</button>
        </div>
        {adding && (
          <AddReference targets={targets} catalog={ctx.catalog} spec={ctx.spec} onCancel={() => setAdding(false)}
            onAdd={(name, value) => { put({ ...env, [free(name)]: value }); setAdding(false); }} />)}
      </div>
    </Field>
  );
}

function VarName({ name, onRename }) {
  const [v, setV] = useState(name);
  useEffect(() => setV(name), [name]);
  const done = () => { if (v && v !== name) onRename(v); else setV(name); };
  return (
    <input className="mono b-varname" value={v} onChange={(e) => setV(e.target.value)} onBlur={done}
      onKeyDown={(e) => { if (e.key === 'Enter') { e.preventDefault(); done(); } }} />
  );
}

// ValueEditor edits one env value in its form; switching the form starts
// the new one with its first target.
function ValueEditor({ v, targets, ctx, onChange }) {
  const kind = envValueKind(v);
  const setKind = (k) => {
    if (k === 'text') return onChange(typeof v === 'string' ? v : '');
    if (k === 'secret') return onChange({ secret: targets.secrets[0] || '' });
    if (k === 'addon') {
      const a = targets.addons[0];
      return onChange(makeRef({ key: 'addons', name: a ? a.name : '' }, { key: a && a.keys.includes('url') ? 'url' : (a && a.keys[0]) || '' }));
    }
    const s = targets.services[0];
    return onChange(makeRef({ key: 'services', name: s ? s.name : '' }, { key: 'url', port: s ? s.ports[0] : '' }));
  };
  return (
    <>
      <Select value={KIND_WORDS[kind]} options={['text', 'secret', 'addon', 'service']} onChange={setKind} />
      {(kind === 'string' || kind === 'value') && (
        <input className="mono grow" value={kind === 'value' ? v.value : v} placeholder="value"
          onChange={(e) => onChange(kind === 'value' ? { value: e.target.value } : e.target.value)} />)}
      {kind === 'secret' && (
        <Select value={v.secret} options={targets.secrets} empty="(a secret)" onChange={(s) => onChange({ secret: s })} />)}
      {kind === 'addon' && <AddonRefEditor v={v} targets={targets} onChange={onChange} />}
      {kind === 'service' && <ServiceRefEditor v={v} targets={targets} onChange={onChange} />}
      {kind === 'invalid' && <span className="mono small bad">{JSON.stringify(v)}</span>}
    </>
  );
}

function AddonRefEditor({ v, targets, onChange }) {
  const a = targets.addons.find((x) => x.name === v.addon);
  const fields = a ? a.fields : Object.keys(v).filter((k) => k !== 'addon' && k !== 'key');
  return (
    <>
      <Select value={v.addon} options={targets.addons.map((x) => x.name)} empty="(an addon)"
        onChange={(n) => onChange({ ...v, addon: n })} />
      {a && a.keys.length
        ? <Select value={v.key || ''} options={a.keys} empty="(a key)" onChange={(k) => onChange(set(v, 'key', k))} />
        : <input className="mono short" placeholder="key" value={v.key || ''} onChange={(e) => onChange(set(v, 'key', e.target.value))} />}
      {fields.map((fd) => (
        <input key={fd} className="mono short" placeholder={fd} title={fd} value={v[fd] || ''}
          onChange={(e) => onChange(set(v, fd, e.target.value))} />
      ))}
    </>
  );
}

function ServiceRefEditor({ v, targets, onChange }) {
  const s = targets.services.find((x) => x.name === v.service);
  return (
    <>
      <Select value={v.service} options={targets.services.map((x) => x.name)} empty="(a service)"
        onChange={(n) => onChange({ ...v, service: n })} />
      <Select value={v.port === undefined ? '' : String(v.port)} options={(s ? s.ports : []).map(String)} empty="(a port)"
        onChange={(p) => onChange(set(v, 'port', /^\d+$/.test(p) ? Number(p) : p))} />
      <Select value={v.key || ''} options={SERVICE_KEYS} empty="(a key)" onChange={(k) => onChange(set(v, 'key', k))} />
    </>
  );
}

// AddReference asks for a target and its key and proposes the variable's
// name: DATABASE_URL for a postgres's url.
export function AddReference({ targets, catalog, onAdd, onCancel }) {
  const all = [
    ...targets.addons.map((a) => ({ id: `addons/${a.name}`, key: 'addons', name: a.name, label: `addon ${a.name} (${a.type})`, a })),
    ...targets.services.map((s) => ({ id: `services/${s.name}`, key: 'services', name: s.name, label: `service ${s.name}`, s })),
    ...targets.secrets.map((n) => ({ id: `secrets/${n}`, key: 'secrets', name: n, label: `secret ${n}` })),
  ];
  const [id, setId] = useState(all[0] ? all[0].id : '');
  const t = all.find((x) => x.id === id);
  const keys = !t ? [] : t.key === 'addons' ? t.a.keys : t.key === 'services' ? SERVICE_KEYS : [];
  const [key, setKey] = useState('');
  const [port, setPort] = useState('');
  const [fields, setFields] = useState({});
  const k = key || (keys.includes('url') ? 'url' : keys[0] || '');
  const p = port || (t && t.s ? String(t.s.ports[0] || '') : '');
  const bp = t && t.a && catalog ? catalog.byType.get(t.a.type) : null;
  const proposed = t ? suggestVarName(t, { type: t.a ? t.a.type : '', key: k, suggestion: bp && bp.suggestions[k] }) : '';
  const [name, setName] = useState('');
  if (all.length === 0) {
    return <p className="muted small">Nothing to reference yet: add an addon, another service or a secret.</p>;
  }
  return (
    <div className="b-addref">
      <div className="b-row wrap">
        <Select value={id} options={all.map((x) => ({ value: x.id, label: x.label }))}
          onChange={(v) => { setId(v); setKey(''); setPort(''); setFields({}); setName(''); }} />
        {t && t.key === 'services' && <Select value={p} options={t.s.ports.map(String)} onChange={setPort} />}
        {keys.length > 0 && <Select value={k} options={keys} onChange={setKey} />}
        {t && t.a && t.a.fields.map((fd) => (
          <input key={fd} className="mono short" placeholder={fd} value={fields[fd] || ''}
            onChange={(e) => setFields({ ...fields, [fd]: e.target.value })} />))}
      </div>
      <div className="b-row wrap">
        <span className="muted small">as</span>
        <input className="mono" value={name || proposed} onChange={(e) => setName(e.target.value)} />
        <button className="primary mini" disabled={!t || !(name || proposed)} onClick={(e) => {
          e.preventDefault();
          onAdd(name || proposed, makeRef(t, { key: k, port: /^\d+$/.test(p) ? Number(p) : p, fields }));
        }}>Add</button>
        <button className="ghost mini" onClick={(e) => { e.preventDefault(); onCancel(); }}>Cancel</button>
      </div>
    </div>
  );
}

// RegistryLogin asks for a user and a password, and always stores the
// password as a secret of the environment (registry-<host>), never as a
// literal (ADR 0032, ADR 0025).
function RegistryLogin({ o, ctx, onChange, findings }) {
  const c = isObject(o.imageCredentials) ? o.imageCredentials : null;
  const [open, setOpen] = useState(false);
  const [user, setUser] = useState('');
  const [password, setPassword] = useState('');
  const [secret, setSecret] = useState('');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');
  const proposed = `registry-${registryHost(imageView(o).written)}`;
  const name = secret || proposed;
  const save = async () => {
    setBusy(true);
    setErr('');
    try {
      await ctx.setSecret(name, password);
      setPassword('');
      onChange({ ...o, imageCredentials: { username: user, password: { secret: name } } });
      setOpen(false);
    } catch (e) {
      setErr(e.message);
    } finally {
      setBusy(false);
    }
  };
  const literal = c && envValueKind(c.password) !== 'secret';
  return (
    <Field label="registry login" wide findings={findings}
      hint={c ? 'its secret is read at its latest version whenever a pod is created, not at the pin' : 'none: the image is pulled anonymously'}>
      {c && !open && (
        <div className="b-row wrap">
          <span className="mono small">user {typeof c.username === 'string' ? c.username : JSON.stringify(c.username)}</span>
          <span className="mono small">· password {envValueKind(c.password) === 'secret' ? `secret ${c.password.secret}` : 'a literal'}</span>
          {literal && <span className="bad small">stands in the clear in the spec: store it as a secret</span>}
          <button className="ghost mini" onClick={(e) => { e.preventDefault(); setUser(typeof c.username === 'string' ? c.username : ''); setOpen(true); }}>Change</button>
          <button className="ghost mini danger" onClick={(e) => { e.preventDefault(); onChange(set(o, 'imageCredentials', undefined)); }}>Remove</button>
        </div>)}
      {!c && !open && <button className="ghost mini" onClick={(e) => { e.preventDefault(); setOpen(true); }}><Icon name="key" size={12} /> Add a login</button>}
      {open && (
        <div className="b-row wrap">
          <input placeholder="user" value={user} onChange={(e) => setUser(e.target.value)} autoComplete="off" />
          <input type="password" placeholder="password or token" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="new-password" />
          <span className="muted small">as secret</span>
          <input className="mono" value={name} onChange={(e) => setSecret(e.target.value)} />
          <button className="primary mini" disabled={busy || !user || !password || !isSecretName(name)} onClick={(e) => { e.preventDefault(); save(); }}>
            Save</button>
          <button className="ghost mini" onClick={(e) => { e.preventDefault(); setOpen(false); setPassword(''); }}>Cancel</button>
          {err && <span className="bad small">{err}</span>}
        </div>)}
    </Field>
  );
}

// --------------------------------------------------------------- addons --

export function AddonForm({ name, obj, ctx, onChange, onRename, onRemove }) {
  const o = isObject(obj) ? obj : {};
  const f = ctx.findings;
  const bp = ctx.catalog ? ctx.catalog.byType.get(o.type) : null;
  const opts = isObject(o.options) ? o.options : {};
  const declared = bp ? bp.options.filter((x) => x.kind !== 'json') : [];
  const declaredNames = new Set(declared.map((x) => x.name));
  const rest = {};
  for (const [k, v] of Object.entries(opts)) if (!declaredNames.has(k)) rest[k] = v;
  const putOpt = (k, v) => {
    const next = without({ ...opts, [k]: v });
    onChange(set(o, 'options', Object.keys(next).length ? next : undefined));
  };
  const plan = bp && bp.plans.find((p) => p.name === (o.plan || bp.defaultPlan));
  return (
    <div className="b-form">
      <ObjectHead kind="addon" name={name} onRename={onRename} onRemove={onRemove} findings={f}
        fields={['type', 'version', 'plan', 'options']} />
      <div className="b-grid3">
        <Field label="type" findings={atField(f, 'type')}>
          <Select value={o.type} options={ctx.catalog ? ctx.catalog.types.map((t) => t.type) : []} onChange={(v) => onChange({ ...o, type: v })} />
        </Field>
        <Field label="version" findings={atField(f, 'version')}>
          <Select value={o.version} options={bp ? bp.versions : []} empty="(a version)" onChange={(v) => onChange(set(o, 'version', v))} />
        </Field>
        <Field label="plan" hint={plan ? planText(plan) : ''} findings={atField(f, 'plan')}>
          <Select value={o.plan} options={bp ? bp.plans.map((p) => ({ value: p.name, label: planText(p) })) : []}
            empty={bp && bp.defaultPlan ? `the default (${bp.defaultPlan})` : '(the default)'} onChange={(v) => onChange(set(o, 'plan', v))} />
        </Field>
      </div>
      {declared.map((d) => (
        <Field key={d.name} label={d.name} findings={atField(f, `options.${d.name}`)}
          hint={[d.description, d.default !== undefined ? `default ${JSON.stringify(d.default)}` : ''].filter(Boolean).join(' · ')}>
          {d.kind === 'string' && <input className="mono" value={opts[d.name] ?? ''} onChange={(e) => putOpt(d.name, e.target.value || undefined)} />}
          {d.kind === 'list' && <Lines value={opts[d.name]} onChange={(v) => putOpt(d.name, v)} placeholder="one per line" />}
          {d.kind === 'boolean' && (
            <Select value={opts[d.name] === undefined ? '' : String(opts[d.name])} options={['true', 'false']} empty="(the default)"
              onChange={(v) => putOpt(d.name, v === '' ? undefined : v === 'true')} />)}
          {d.kind === 'enum' && (
            <Select value={opts[d.name] ?? ''} options={d.values} empty="(the default)" onChange={(v) => putOpt(d.name, v || undefined)} />)}
        </Field>
      ))}
      <OtherOptions rest={rest} onChange={(next) => {
        const all = { ...without(Object.fromEntries(declared.map((d) => [d.name, opts[d.name]]))), ...next };
        onChange(set(o, 'options', Object.keys(all).length ? all : undefined));
      }} findings={atField(f, 'options').filter((x) => ![...declaredNames].some((n) => atField([x], `options.${n}`).length))}
      declared={declared.length > 0} />
      {ctx.catalog && ctx.catalog.builtin && (
        <p className="panel-note">This ferund answers no catalog of addons; the forms use the list this UI knows.</p>)}
    </div>
  );
}

// OtherOptions are the options no field of the blueprint declares, as
// JSON; text that does not parse is marked and not written.
function OtherOptions({ rest, onChange, findings, declared }) {
  const text0 = Object.keys(rest).length ? JSON.stringify(rest, null, 2) : '';
  const [text, setText] = useState(text0);
  const [bad, setBad] = useState('');
  useEffect(() => { if (!bad) setText(text0); }, [text0]); // eslint-disable-line react-hooks/exhaustive-deps
  return (
    <Field label={declared ? 'other options (JSON)' : 'options (JSON)'} wide findings={findings} hint={bad || ''}>
      <textarea className={'mono b-lines' + (bad ? ' bad-input' : '')} rows={Math.min(8, Math.max(2, text.split('\n').length))}
        placeholder="{}" value={text}
        onChange={(e) => {
          setText(e.target.value);
          if (!e.target.value.trim()) { setBad(''); onChange({}); return; }
          try {
            const v = JSON.parse(e.target.value);
            if (!isObject(v)) throw new Error('options are a JSON object');
            setBad('');
            onChange(v);
          } catch (err) {
            setBad(err.message);
          }
        }} />
    </Field>
  );
}

// --------------------------------------------------------------- routes --

export function RouteForm({ name, obj, ctx, onChange, onRename, onRemove }) {
  const o = isObject(obj) ? obj : {};
  const f = ctx.findings;
  const services = objectsOf(ctx.spec, 'services').filter(([, s]) => isObject(s));
  const svc = services.find(([n]) => n === o.service);
  const ports = svc && Array.isArray(svc[1].ports) ? svc[1].ports.map((p) => p && (p.name || p.port)).filter(Boolean).map(String) : [];
  return (
    <div className="b-form">
      <ObjectHead kind="route" name={name} onRename={onRename} onRemove={onRemove} findings={f}
        fields={['path', 'service', 'port', 'strip', 'host']} />
      <Field label="path" hint="requests whose path starts with it (by segments)" findings={atField(f, 'path')}>
        <input className="mono" placeholder="/" value={o.path || ''} onChange={(e) => onChange({ ...o, path: e.target.value })} />
      </Field>
      <div className="b-grid3">
        <Field label="service" findings={atField(f, 'service')}>
          <Select value={o.service} options={services.map(([n]) => n)} empty="(a service)" onChange={(v) => {
            const s = services.find(([n]) => n === v);
            const first = s && Array.isArray(s[1].ports) && s[1].ports[0] ? s[1].ports[0].name || s[1].ports[0].port : undefined;
            onChange(without({ ...o, service: v, port: first }));
          }} />
        </Field>
        <Field label="port" findings={atField(f, 'port')}>
          <Select value={o.port === undefined ? '' : String(o.port)} options={ports} empty="(a port)"
            onChange={(v) => onChange(set(o, 'port', /^\d+$/.test(v) ? Number(v) : v))} />
        </Field>
        <Field label="strip" hint="removes the path before it is forwarded" findings={atField(f, 'strip')}>
          <input type="checkbox" checked={!!o.strip} onChange={(e) => onChange(set(o, 'strip', e.target.checked || undefined))} />
        </Field>
      </div>
    </div>
  );
}

// -------------------------------------------------------------- secrets --

// SecretForm shows a secret: its pinned and latest versions and what
// references it. A value entered goes to the secrets API at once (a new
// version, which the draft pins); none is in the draft, the browser's
// storage or an export.
export function SecretForm({ name, pin, refs, ctx, onPin, onRemove }) {
  const [value, setValue] = useState('');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');
  const save = async () => {
    setBusy(true);
    setErr('');
    try {
      await ctx.setSecret(name, value);
      setValue('');
    } catch (e) {
      setErr(e.message);
    } finally {
      setBusy(false);
    }
  };
  const login = refs.some((r) => r.via === 'imageCredentials');
  return (
    <div className="b-form">
      <ObjectHead kind="secret" name={name} onRemove={onRemove} findings={ctx.findings} fields={['version']} />
      <div className="kv-grid">
        <span className="k">pinned</span>
        <span className="v">{pin.pinned ? <>version <span className="mono">{pin.pinned}</span></> : <span className="muted">the latest when applied</span>}</span>
        <span className="k">latest</span>
        <span className="v">
          {pin.latest ? <>version <span className="mono">{pin.latest}</span></>
            : pin.missing ? <span className="bad">missing: the environment has no such secret</span>
              : <span className="muted">set when the environment is made</span>}
          {pin.newer && (
            <> · <span className="warntext">version {pin.latest}, pinned {pin.pinned}</span>{' '}
              <button className="ghost mini" onClick={() => onPin(pin.latest)}>Use version {pin.latest}</button></>)}
        </span>
        <span className="k">referenced by</span>
        <span className="v">
          {refs.length === 0 ? <span className="muted">nothing: a deployment drops it</span>
            : refs.map((r, i) => <div key={i} className="mono small">{r.from.name} · {refLine(r)}</div>)}
        </span>
      </div>
      <FindingList list={atField(ctx.findings, 'version')} />
      {login && <p className="panel-note">A registry login reads it at its latest version whenever a pod is created, not at the pin.</p>}
      <Field label={pin.missing ? 'Set a value' : 'a new version'} wide
        hint={ctx.isNew ? 'kept in this page only until the first apply, which writes it first' : 'written to the environment at once; the draft pins the version it makes'}>
        <div className="b-row">
          <input type="password" className="grow" autoComplete="new-password" placeholder="the value" value={value}
            onChange={(e) => setValue(e.target.value)} />
          <button className="primary mini" disabled={busy || !value} onClick={(e) => { e.preventDefault(); save(); }}>
            {pin.missing ? 'Set a value' : 'Save as a new version'}</button>
        </div>
        {err && <span className="bad small">{err}</span>}
      </Field>
    </div>
  );
}

// ----------------------------------------------------------- other kinds --

// RawForm shows an object of a kind without a form yet (bindings come in
// step 2): its JSON, edited in the JSON view.
export function RawForm({ kind, name, obj, ctx, onRemove }) {
  return (
    <div className="b-form">
      <ObjectHead kind={kind} name={name} onRemove={onRemove} findings={ctx.findings} fields={[]} />
      <pre className="specpre b-raw">{JSON.stringify(obj, null, 2)}</pre>
      <p className="panel-note">Its form comes later; edit it in the JSON view.</p>
    </div>
  );
}
