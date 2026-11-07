// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useEffect, useState } from 'react';
import { Icon } from '../Icon.jsx';
import { isDnsLabel, isObject, isSecretName, objectsOf, registryHost } from './draft.js';
import { envValueKind, refLine, refTargets, referencesTo, suggestVarName } from './refs.js';
import { atField, notAt } from './findings.js';
import { checkNewer, imageView, pinSecret, setImage } from './pins.js';
import { optionProblems, planText } from './catalog.js';
import { credentialPath } from './credentials.js';
import {
  CREDENTIAL_FIELDS, HEALTH_KINDS, SERVICE_KEYS, VALUE_TYPES, addonValue, bindingSummary, credentialBinding, freeName,
  healthOf, idleOf, idleValue, portValue, readinessOf, renameVar, retypeHealth, retypeValue, routed, serviceValue,
  textOf, toggleBinding, valueType, withText,
} from './fields.js';

// The builder's forms (ADR 0032, "The builder edits the draft in forms"):
// one per kind, in sections, with the API's fields and its defaults as
// placeholders. A form gets its object and gives back the next one
// (onChange); a change across objects (a binding added and ticked, a
// secret's pin) goes through ctx.editSpec. Its own checks only make a
// field enterable; the verdict is the plan's, whose findings show at the
// fields their paths name.
//
// ctx is what every form reads: the spec, the catalog, the secrets the
// environment has (knownSecrets) and their pins, the tenant's credentials,
// the object's findings, setSecret(name, value) (a promise of the version
// it made), editSpec(fn), whether the environment is new, whether the
// role edits attached services, and the tenant (for links).

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

function Field({ label, hint, findings, children, wide, problems }) {
  return (
    <div className={'b-field' + (wide ? ' wide' : '')}>
      <span className="b-label">{label}</span>
      <span className="b-control">{children}</span>
      {hint && <span className="b-hint muted small">{hint}</span>}
      {problems && problems.length > 0 && <span className="b-hint warntext small">{problems.join('; ')}</span>}
      <FindingList list={findings} />
    </div>
  );
}

// Section is a card of a form: a heading, its fields; folded when long
// and nothing in it needs a look.
function Section({ title, hint, children, fold = false, findings = [], extra }) {
  const [open, setOpen] = useState(!fold || findings.length > 0);
  const n = findings.length;
  return (
    <section className={'b-sect' + (open ? '' : ' folded')}>
      <header className="b-secthead">
        {fold
          ? <button className="b-link b-sectitle" onClick={() => setOpen((o) => !o)} aria-expanded={open}>
            <Icon name={open ? 'chevronDown' : 'chevronRight'} size={12} /> {title}</button>
          : <h3 className="b-sectitle">{title}</h3>}
        {n > 0 && <span className={'badge ' + (findings.some((f) => f.severity === 'error') ? 'bad' : 'warn')}>{n}</span>}
        {hint && <span className="muted small b-secthint">{hint}</span>}
        <span className="spacer" />
        {extra}
      </header>
      {open && <div className="b-sectbody">{children}</div>}
    </section>
  );
}

// hasDefault says whether a catalog's default says anything: not null,
// not an empty list.
function hasDefault(v) {
  return v !== undefined && v !== null && !(Array.isArray(v) && v.length === 0);
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

function OptNumber({ value, onChange, placeholder, min, max, step, title }) {
  return (
    <input type="number" className="num" min={min} max={max} step={step} placeholder={placeholder} title={title}
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
    <textarea className="mono b-lines" rows={Math.max(rows, Math.min(8, text.split('\n').length))} placeholder={placeholder} value={text}
      onChange={(e) => {
        setText(e.target.value);
        const list = e.target.value.split('\n').filter((l) => l !== '');
        onChange(list.length ? list : undefined);
      }} />
  );
}

// NameInput renames on blur or Enter: a rename moves the object and its
// references, so it is one edit, not one per key.
function NameInput({ name, onRename, check = isDnsLabel, what = 'a DNS label: a-z, 0-9 and -', className = '' }) {
  const [v, setV] = useState(name);
  useEffect(() => setV(name), [name]);
  const ok = check(v);
  const done = () => { if (v !== name && ok) onRename(v); else setV(name); };
  return (
    <>
      <input className={'mono ' + className + (ok ? '' : ' bad-input')} value={v} onChange={(e) => setV(e.target.value)}
        aria-label="name" onBlur={done} onKeyDown={(e) => { if (e.key === 'Enter') { e.preventDefault(); done(); } }} />
      {!ok && <span className="b-hint bad small">{what}</span>}
    </>
  );
}

function Select({ value, options, onChange, empty, title, className }) {
  const opts = [...options];
  if (value !== undefined && value !== '' && !opts.some((o) => String(o.value ?? o) === String(value))) {
    opts.unshift({ value, label: `${value} (not offered)` });
  }
  return (
    <select value={value ?? ''} title={title} className={className} onChange={(e) => onChange(e.target.value)}>
      {empty !== undefined && <option value="">{empty}</option>}
      {opts.map((o) => {
        const val = o.value ?? o;
        return <option key={String(val)} value={val}>{o.label ?? String(val)}</option>;
      })}
    </select>
  );
}

const portsOf = (o) => (Array.isArray(o.ports) ? o.ports : []).map((p) => p && (p.name || p.port)).filter((p) => p !== undefined && p !== '');

// ---------------------------------------------------------- the frame --

// FormHead is a form's title: its kind and its name (renamed in place),
// and the object's findings that no field takes.
function FormHead({ kind, name, onRename, findings, fields, extra, nameCheck }) {
  const rest = notAt(findings, fields);
  return (
    <header className="b-formhead">
      <div className="b-formtitle">
        <span className="kind">{kind}</span>
        {onRename
          ? <span className="b-name"><NameInput name={name} onRename={onRename} className="b-titlein" {...(nameCheck || {})} /></span>
          : <h2 className="mono">{name}</h2>}
        <span className="spacer" />
        {extra}
      </div>
      <FindingList list={rest} />
    </header>
  );
}

// FormFoot carries the object's remove, at the bottom of its form.
function FormFoot({ onRemove, what }) {
  if (!onRemove) return null;
  return (
    <footer className="b-formfoot">
      <button className="ghost danger" onClick={onRemove}><Icon name="trash" size={13} /> Remove {what}</button>
      <span className="muted small">from the draft; the review shows what it does</span>
    </footer>
  );
}

// ------------------------------------------------------------ services --

const SERVICE_FIELDS = ['image', 'imageAsWritten', 'imageCredentials', 'command', 'env', 'ports', 'replicas', 'resources',
  'readiness', 'idle', 'rollout', 'bindings', 'runtime', 'sandbox', 'cwd', 'mounts'];

export function ServiceForm({ name, obj, ctx, onChange, onRename, onRemove }) {
  const o = isObject(obj) ? obj : {};
  const f = ctx.findings;
  if (o.runtime === 'attached') return <AttachedForm name={name} o={o} ctx={ctx} onChange={onChange} onRemove={onRemove} />;
  const self = { key: 'services', name };
  return (
    <div className="b-form">
      <FormHead kind="Service" name={name} onRename={onRename} findings={f} fields={SERVICE_FIELDS} />
      <Section title="Container" findings={[...atField(f, 'image'), ...atField(f, 'command'), ...atField(f, 'ports'), ...atField(f, 'imageCredentials')]}>
        <ImageField o={o} onChange={onChange} findings={atField(f, 'image')} />
        <Field label="Command and arguments" hint="one per line; empty: the image's entrypoint and cmd" findings={atField(f, 'command')}>
          <Lines value={o.command} onChange={(v) => onChange(set(o, 'command', v))} placeholder="(the image's)" />
        </Field>
        <PortsField o={o} onChange={onChange} findings={atField(f, 'ports')} />
        <RegistryLogin o={o} ctx={ctx} onChange={onChange} findings={atField(f, 'imageCredentials')} />
      </Section>
      <Section title="Variables" hint="the environment of its process" findings={atField(f, 'env')}>
        <EnvTable self={self} obj={o} ctx={ctx} onChange={onChange} findings={f} />
      </Section>
      <Section title="Scaling & resources" findings={[...atField(f, 'replicas'), ...atField(f, 'resources'), ...atField(f, 'idle')]}>
        <div className="b-grid3">
          <Field label="Replicas" hint="0 to 20" findings={atField(f, 'replicas')}>
            <OptNumber value={o.replicas} min={0} max={20} placeholder="1" onChange={(v) => onChange(set(o, 'replicas', v))} />
          </Field>
          <ResourcesFields o={o} onChange={onChange} findings={f} />
        </div>
        <IdleField o={o} name={name} spec={ctx.spec} onChange={onChange} findings={atField(f, 'idle')} />
      </Section>
      <Section title="Health check" hint="when a replica gets traffic" findings={atField(f, 'readiness')}>
        <HealthField o={o} onChange={onChange} findings={atField(f, 'readiness')} />
      </Section>
      <BindingsSection self={self} o={o} ctx={ctx} findings={atField(f, 'bindings')} />
      <Section title="Rollout" fold={!o.rollout} findings={atField(f, 'rollout')}>
        <Field label="Timeout" hint="a rollout that takes longer fails; empty: 10m" findings={atField(f, 'rollout')}>
          <input className="mono short" placeholder="10m" value={(isObject(o.rollout) && o.rollout.timeout) || ''}
            onChange={(e) => onChange(set(o, 'rollout', e.target.value ? { ...(o.rollout || {}), timeout: e.target.value } : undefined))} />
        </Field>
      </Section>
      <FormFoot onRemove={onRemove} what={`service ${name}`} />
    </div>
  );
}

// AttachedForm is a service that runs in a sandbox of the tenant: a
// summary for those who write the environment alone, a form for those
// who write the tenant (ADR 0031).
function AttachedForm({ name, o, ctx, onChange, onRemove }) {
  const f = ctx.findings;
  if (!ctx.editAttached) {
    return (
      <div className="b-form">
        <FormHead kind="Service · attached" name={name} findings={f} fields={[]} />
        <div className="message"><Icon name="lock" size={14} />
          <span>It runs in sandbox <span className="mono">{o.sandbox || '—'}</span> of the tenant. Changing it needs write on the tenant;
            the draft keeps it as it is.</span></div>
        <Section title="What it runs">
          <div className="kv-grid">
            <span className="k">sandbox</span><span className="v mono">{o.sandbox || '—'}</span>
            <span className="k">command</span><span className="v mono">{(o.command || []).join(' ') || '—'}</span>
            {o.cwd && <><span className="k">directory</span><span className="v mono">{o.cwd}</span></>}
            <span className="k">idle</span><span className="v">{o.idle === 'never' ? 'stays awake' : o.idle ? `awake ${o.idle} after its last activity` : "the sandbox's own"}</span>
            <span className="k">variables</span><span className="v">{Object.keys(isObject(o.env) ? o.env : {}).join(', ') || 'none'}</span>
            <span className="k">bindings</span><span className="v">{(o.bindings || []).join(', ') || 'none'}</span>
          </div>
        </Section>
      </div>
    );
  }
  const self = { key: 'services', name };
  return (
    <div className="b-form">
      <FormHead kind="Service · attached" name={name} findings={f} fields={SERVICE_FIELDS} />
      <Section title="Process" findings={[...atField(f, 'sandbox'), ...atField(f, 'command'), ...atField(f, 'cwd')]}>
        <Field label="Sandbox" hint="the sandbox of the tenant it runs in" findings={atField(f, 'sandbox')}>
          <input className="mono" value={o.sandbox || ''} onChange={(e) => onChange(set(o, 'sandbox', e.target.value))} />
        </Field>
        <Field label="Command and arguments" hint="one per line" findings={atField(f, 'command')}>
          <Lines value={o.command} onChange={(v) => onChange(set(o, 'command', v))} placeholder="npm" />
        </Field>
        <Field label="Directory" findings={atField(f, 'cwd')}>
          <input className="mono" placeholder="/workspace" value={o.cwd || ''} onChange={(e) => onChange(set(o, 'cwd', e.target.value))} />
        </Field>
        <PortsField o={o} onChange={onChange} findings={atField(f, 'ports')} />
        <Field label="Idle" hint={'"never": the sandbox stays awake while it runs; a duration: awake that long after its last activity; empty: the sandbox\'s own'}
          findings={atField(f, 'idle')}>
          <input className="mono short" placeholder="(the sandbox's)" value={o.idle || ''} onChange={(e) => onChange(set(o, 'idle', e.target.value))} />
        </Field>
      </Section>
      <Section title="Variables" findings={atField(f, 'env')}>
        <EnvTable self={self} obj={o} ctx={ctx} onChange={onChange} findings={f} />
      </Section>
      <BindingsSection self={self} o={o} ctx={ctx} findings={atField(f, 'bindings')} />
      <FormFoot onRemove={onRemove} what={`service ${name}`} />
    </div>
  );
}

// ImageField shows the image as written and its digest while it is
// pinned; "Check for a newer image" puts the image as written back, which
// the plan resolves.
function ImageField({ o, onChange, findings }) {
  const v = imageView(o);
  return (
    <Field label="Image" findings={findings}
      hint={v.digest
        ? <>pinned to <span className="mono" title={v.digest}>{v.digest.slice(0, 19)}…</span> by the plan: a change elsewhere does not pull a tag that moved</>
        : 'from any registry; the plan resolves it to a digest'}>
      <input className="mono" placeholder="ghcr.io/acme/web:1.0" value={v.written} aria-label="image"
        onChange={(e) => onChange(setImage(o, e.target.value))} />
      {v.digest && o.imageAsWritten && (
        <button className="ghost mini" title="the image as written again, which the plan resolves"
          onClick={() => onChange(checkNewer(o))}>
          <Icon name="refresh" size={12} /> Check for a newer image</button>)}
    </Field>
  );
}

function PortsField({ o, onChange, findings }) {
  const ports = Array.isArray(o.ports) ? o.ports : [];
  const put = (i, p) => onChange({ ...o, ports: ports.map((q, j) => (j === i ? without({ ...q, ...p }) : q)) });
  return (
    <Field label="Ports" hint={ports.length ? 'routes, health checks and other services name them' : 'none: nothing reaches it'} findings={findings}>
      <div className="b-rows">
        {ports.map((p, i) => (
          <div key={i} className="b-row">
            <input className="mono b-portname" placeholder="name" aria-label="port name" value={(p && p.name) || ''}
              onChange={(e) => put(i, { name: e.target.value })} />
            <OptNumber value={p && p.port} min={1} max={65535} placeholder="8080" title="port number"
              onChange={(v) => put(i, { port: v })} />
            <button className="iconbtn" title="remove the port"
              onClick={() => onChange(set(o, 'ports', ports.length > 1 ? ports.filter((_, j) => j !== i) : undefined))}>
              <Icon name="x" size={13} /></button>
          </div>
        ))}
        <span><button className="ghost mini" onClick={() => {
          onChange({ ...o, ports: [...ports, { name: ports.length ? `port-${ports.length + 1}` : 'http', port: 8080 + ports.length }] });
        }}><Icon name="plus" size={12} /> Add a port</button></span>
      </div>
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
      <Field label="vCPUs" hint="1 to 32, each replica" findings={atField(findings, 'resources.vcpus')}>
        <OptNumber value={r.vcpus} min={1} max={32} placeholder="1" onChange={(v) => put('vcpus', v)} />
      </Field>
      <Field label="Memory (MiB)" hint="256 to 65536, each replica" findings={atField(findings, 'resources.memoryMib')}>
        <OptNumber value={r.memoryMib} min={256} max={65536} step={256} placeholder="512" onChange={(v) => put('memoryMib', v)} />
      </Field>
    </>
  );
}

// IdleField is a service's pause when idle: never, or after N minutes
// without traffic, which needs a route to wake it.
function IdleField({ o, name, spec, onChange, findings }) {
  const idle = idleOf(o.idle);
  const reach = routed(spec, name);
  return (
    <Field label="Pause when idle" findings={findings}
      hint={idle.mode === 'never' ? 'its replicas run all the time'
        : reach ? 'paused without traffic; the next request through a route wakes it' : 'nothing would wake it: no route reaches it'}>
      <Select value={idle.mode} options={[{ value: 'never', label: 'Never' }, { value: 'after', label: 'After' },
        ...(idle.mode === 'custom' ? [{ value: 'custom', label: `After ${idle.raw}` }] : [])]}
      onChange={(m) => onChange(set(o, 'idle', m === 'custom' ? idle.raw : idleValue(m, idle.minutes)))} />
      {idle.mode === 'after' && (
        <><OptNumber value={idle.minutes} min={1} placeholder="15" title="minutes" onChange={(v) => onChange(set(o, 'idle', idleValue('after', v || 1)))} />
          <span className="muted small">minutes without traffic</span></>)}
    </Field>
  );
}

// HealthField edits the readiness as a health check: none, HTTP (port,
// path), TCP (port) or a command, with its timing.
function HealthField({ o, onChange, findings }) {
  const h = healthOf(o.readiness);
  const ports = portsOf(o);
  const put = (next) => onChange(set(o, 'readiness', readinessOf(next)));
  return (
    <div className="b-rows">
      <Field label="Check" findings={findings.filter((x) => !/^readiness\.(initialDelay|period|failureThreshold)/.test(x.rest))}
        hint={h.kind === 'none' ? 'none: ready when it still runs 2 s after it was first seen running' : 'traffic once the check passes'}>
        <span className="b-seg">
          {HEALTH_KINDS.map((k) => (
            <button key={k.key} className={'ghost mini' + (h.kind === k.key ? ' on' : '')} aria-pressed={h.kind === k.key}
              onClick={() => put(retypeHealth(h, k.key, ports))}>{k.label}</button>))}
        </span>
        {(h.kind === 'http' || h.kind === 'tcp') && (
          <Select value={String(h.port ?? '')} options={ports.map(String)} title="port" onChange={(v) => put({ ...h, port: portValue(v) })} />)}
        {h.kind === 'http' && (
          <input className="mono" placeholder="/readyz" aria-label="path" value={h.path} onChange={(e) => put({ ...h, path: e.target.value })} />)}
      </Field>
      {h.kind === 'exec' && (
        <Field label="Command" hint="one argument per line; ready when it exits 0">
          <Lines value={h.command} onChange={(v) => put({ ...h, command: v || [] })} placeholder="pg_isready" />
        </Field>)}
      {h.kind !== 'none' && (
        <div className="b-grid3">
          <Field label="Initial delay" findings={atField(findings, 'readiness.initialDelay')}>
            <input className="mono short" placeholder="0s" value={h.initialDelay} onChange={(e) => put({ ...h, initialDelay: e.target.value })} />
          </Field>
          <Field label="Period" findings={atField(findings, 'readiness.period')}>
            <input className="mono short" placeholder="2s" value={h.period} onChange={(e) => put({ ...h, period: e.target.value })} />
          </Field>
          <Field label="Failure threshold" findings={atField(findings, 'readiness.failureThreshold')}>
            <OptNumber value={h.failureThreshold} min={1} placeholder="3" onChange={(v) => put({ ...h, failureThreshold: v })} />
          </Field>
        </div>)}
    </div>
  );
}

// ------------------------------------------------------------ variables --

// EnvTable edits the variables, a row each: NAME | value, the value a
// typed choice (Text, Secret, Addon output, Service address).
function EnvTable({ self, obj, ctx, onChange, findings }) {
  const env = isObject(obj.env) ? obj.env : {};
  const targets = refTargets(ctx.spec, ctx.catalog, { self: self.key === 'services' ? self.name : '', knownSecrets: ctx.knownSecrets });
  const put = (next) => onChange(set(obj, 'env', Object.keys(next).length ? next : undefined));
  const rows = Object.entries(env);
  return (
    <div className="b-rows">
      <FindingList list={atField(findings, 'env').filter((x) => x.rest === 'env')} />
      {rows.length > 0 && (
        <table className="b-vars">
          <thead><tr><th>Name</th><th>Value</th><th /></tr></thead>
          <tbody>
            {rows.map(([k, v]) => {
              const fs = atField(findings, `env.${k}`);
              return (
                <tr key={k} className={fs.some((x) => x.severity === 'error') ? 'bad' : ''}>
                  <td className="b-varcell"><VarName name={k} onRename={(to) => put(renameVar(env, k, to))} /></td>
                  <td className="b-valcell">
                    <ValueEditor name={k} v={v} targets={targets} ctx={ctx}
                      onChange={(nv, rename) => put(rename && !(rename in env) ? renameVar({ ...env, [k]: nv }, k, rename) : { ...env, [k]: nv })} />
                    <FindingList list={fs} />
                  </td>
                  <td className="b-delcell">
                    <button className="iconbtn" title={`remove ${k}`} onClick={() => {
                      const next = { ...env };
                      delete next[k];
                      put(next);
                    }}><Icon name="x" size={13} /></button>
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>)}
      {rows.length === 0 && <p className="muted small">No variables.</p>}
      <span><button className="ghost mini" onClick={() => put({ ...env, [freeName(env, 'NAME')]: '' })}>
        <Icon name="plus" size={12} /> Add a variable</button></span>
    </div>
  );
}

function VarName({ name, onRename }) {
  const [v, setV] = useState(name);
  useEffect(() => setV(name), [name]);
  const done = () => { if (v && v !== name) onRename(v); else setV(name); };
  return (
    <input className="mono b-varname" value={v} aria-label="variable name" onChange={(e) => setV(e.target.value)} onBlur={done}
      onKeyDown={(e) => { if (e.key === 'Enter') { e.preventDefault(); done(); } }} />
  );
}

const PLACEHOLDER_NAME = /^NAME(_\d+)?$/;
const NEW = '\u0000new';

// ValueEditor edits one variable's value as its typed choice. A
// variable still named NAME takes the name a reference proposes
// (DATABASE_URL for a Postgres's url): onChange(value, rename).
export function ValueEditor({ name = '', v, targets, ctx, onChange }) {
  const type = valueType(v);
  const propose = (nv, any = false) => {
    if (!any && !PLACEHOLDER_NAME.test(name)) return '';
    const k = envValueKind(nv);
    if (k === 'addon' && nv.addon) {
      const a = targets.addons.find((x) => x.name === nv.addon);
      const bp = a && ctx.catalog ? ctx.catalog.byType.get(a.type) : null;
      return suggestVarName({ key: 'addons', name: nv.addon }, { type: a ? a.type : '', key: nv.key || '', suggestion: bp && bp.suggestions[nv.key] });
    }
    if (k === 'service' && nv.service) return suggestVarName({ key: 'services', name: nv.service }, { key: nv.key || '' });
    if (k === 'secret' && nv.secret) return suggestVarName({ key: 'secrets', name: nv.secret });
    return '';
  };
  // A name still a placeholder, or the one the value before proposed,
  // follows the reference.
  const follows = PLACEHOLDER_NAME.test(name) || (name && name === propose(v, true));
  const change = (nv) => onChange(nv, follows ? propose(nv, true) : '');
  return (
    <div className="b-value">
      <Select value={type === 'other' ? '' : type} options={VALUE_TYPES.map((t) => ({ value: t.key, label: t.label }))}
        empty={type === 'other' ? 'Other (JSON)' : undefined} title="the kind of value" className="b-vtype"
        onChange={(t) => t && change(retypeValue(v, t, targets))} />
      {type === 'text' && (
        <input className="mono grow" value={textOf(v)} placeholder="value" aria-label="value" onChange={(e) => onChange(withText(v, e.target.value))} />)}
      {type === 'secret' && <SecretValue name={name} v={v} targets={targets} ctx={ctx} onChange={change} />}
      {type === 'addon' && <AddonValue v={v} targets={targets} onChange={change} />}
      {type === 'service' && <ServiceValue v={v} targets={targets} onChange={change} />}
      {type === 'other' && <span className="mono small muted grow" title="edit it in the JSON view">{JSON.stringify(v)}</span>}
    </div>
  );
}

// SecretValue picks a secret of the environment, says its version and
// pin, and sets a value (a new secret or a new version) at once.
function SecretValue({ name, v, targets, ctx, onChange }) {
  const [adding, setAdding] = useState(false);
  const [valueFor, setValueFor] = useState(false);
  const pin = (ctx.pins || []).find((p) => p.name === v.secret);
  return (
    <>
      <Select value={v.secret} empty="(a secret)" title="the secret"
        options={[...targets.secrets.map((s) => ({ value: s, label: s })), { value: NEW, label: '+ New secret…' }]}
        onChange={(s) => { if (s === NEW) setAdding(true); else { setAdding(false); onChange({ secret: s }); } }} />
      {v.secret && pin && !adding && (
        <span className="b-pin small">
          {pin.missing
            ? <span className="bad">no value yet</span>
            : pin.newer
              ? <><span className="warntext">v{pin.pinned} pinned, v{pin.latest} newer</span>{' '}
                <button className="b-link acc" onClick={() => ctx.editSpec((s) => pinSecret(s, v.secret, pin.latest))}>use v{pin.latest}</button></>
              : <span className="muted">{pin.pinned ? `v${pin.pinned}` : pin.latest ? `latest (v${pin.latest})` : 'latest'}</span>}
          {' · '}<button className="b-link acc" onClick={() => setValueFor((x) => !x)}>{pin.missing ? 'set a value' : 'new value'}</button>
        </span>)}
      {adding && (
        <SecretInline proposed={String(name || '').toLowerCase().replace(/_/g, '-')} ctx={ctx} withName
          onDone={(s) => { setAdding(false); onChange({ secret: s }); }} onCancel={() => setAdding(false)} />)}
      {valueFor && !adding && (
        <SecretInline proposed={v.secret} ctx={ctx} onDone={() => setValueFor(false)} onCancel={() => setValueFor(false)} />)}
    </>
  );
}

// SecretInline asks for a value (and a new secret's name) and writes it
// through the secrets API at once; the draft pins the version it makes.
function SecretInline({ proposed, ctx, withName = false, onDone, onCancel }) {
  const [n, setN] = useState(proposed || '');
  const [value, setValue] = useState('');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');
  const ok = isSecretName(n);
  return (
    <div className="b-inline">
      {withName && <input className="mono" placeholder="secret name" aria-label="secret name" value={n} onChange={(e) => setN(e.target.value.trim())} />}
      <input type="password" className="grow" autoComplete="new-password" aria-label="secret value"
        placeholder={withName ? 'its value' : `a new value of ${n}`} value={value} onChange={(e) => setValue(e.target.value)} />
      <button className="primary mini" disabled={busy || !value || !ok} onClick={async () => {
        setBusy(true);
        setErr('');
        try {
          await ctx.setSecret(n, value);
          setValue('');
          onDone(n);
        } catch (e) {
          setErr(e.message);
        } finally {
          setBusy(false);
        }
      }}>{ctx.isNew ? 'Keep' : 'Save'}</button>
      <button className="ghost mini" onClick={onCancel}>Cancel</button>
      <span className="muted small b-inlinehint">{ctx.isNew
        ? 'kept in this page until the first deploy writes it'
        : 'written to the environment now, never into the draft'}</span>
      {withName && n && !ok && <span className="bad small">a-z, 0-9, '.', '_' and '-'</span>}
      {err && <span className="bad small">{err}</span>}
    </div>
  );
}

function AddonValue({ v, targets, onChange }) {
  const a = targets.addons.find((x) => x.name === v.addon);
  const fields = a ? a.fields : Object.keys(v).filter((k) => k !== 'addon' && k !== 'key');
  return (
    <>
      <Select value={v.addon} options={targets.addons.map((x) => ({ value: x.name, label: `${x.name} (${x.type})` }))} empty="(an addon)" title="the addon"
        onChange={(n) => onChange(addonValue(v, { addon: n }, targets.addons.find((x) => x.name === n)))} />
      {a && a.keys.length
        ? <Select value={v.key || ''} options={a.keys} empty="(an output)" title="its output" onChange={(k) => onChange(addonValue(v, { key: k }, a))} />
        : <input className="mono short" placeholder="output" value={v.key || ''} onChange={(e) => onChange(addonValue(v, { key: e.target.value }, a))} />}
      {fields.map((fd) => (
        <label key={fd} className="b-subf" title={`${fd} (empty: the addon's own)`}><span className="muted small">{fd}</span>
          <input className="mono short" placeholder="default" aria-label={fd} value={v[fd] || ''}
            onChange={(e) => onChange(addonValue(v, { [fd]: e.target.value }, a))} /></label>
      ))}
    </>
  );
}

function ServiceValue({ v, targets, onChange }) {
  const s = targets.services.find((x) => x.name === v.service);
  return (
    <>
      <Select value={v.service} options={targets.services.map((x) => x.name)} empty="(a service)" title="the service"
        onChange={(n) => onChange(serviceValue(v, { service: n }, targets.services.find((x) => x.name === n)))} />
      <Select value={v.port === undefined ? '' : String(v.port)} options={(s ? s.ports : []).map(String)} empty="(a port)" title="its port"
        onChange={(p) => onChange(serviceValue(v, { port: portValue(p) }, s))} />
      <Select value={v.key || ''} options={SERVICE_KEYS} empty="(url, host or port)" title="what of it"
        onChange={(k) => onChange(serviceValue(v, { key: k }, s))} />
    </>
  );
}

// RegistryLogin picks the secret that holds the password of the image's
// registry, with its user; a new login always stores its password as a
// secret (registry-<host>), never as a literal (ADR 0032, ADR 0025).
function RegistryLogin({ o, ctx, onChange, findings }) {
  const c = isObject(o.imageCredentials) ? o.imageCredentials : null;
  const [adding, setAdding] = useState(false);
  const [user, setUser] = useState('');
  const [password, setPassword] = useState('');
  const [secret, setSecret] = useState('');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');
  const proposed = `registry-${registryHost(imageView(o).written)}`;
  const sname = secret || proposed;
  const secrets = [...new Set([...(ctx.knownSecrets || []), ...objectsOf(ctx.spec, 'secrets').map(([n]) => n)])].sort();
  const current = c && envValueKind(c.password) === 'secret' ? c.password.secret : '';
  const literal = c && !current;
  const username = c && typeof c.username === 'string' ? c.username : c ? textOf(c.username) : '';
  const save = async () => {
    setBusy(true);
    setErr('');
    try {
      await ctx.setSecret(sname, password);
      setPassword('');
      onChange({ ...o, imageCredentials: { username: user, password: { secret: sname } } });
      setAdding(false);
    } catch (e) {
      setErr(e.message);
    } finally {
      setBusy(false);
    }
  };
  return (
    <Field label="Private registry login" findings={findings}
      hint={c ? 'its secret is read at its latest version whenever a pod is created, not at the pin' : 'none: the image is pulled anonymously'}>
      <Select value={adding ? NEW : literal ? '\u0000literal' : current} empty="None"
        options={[...(literal ? [{ value: '\u0000literal', label: 'a password written in the spec' }] : []),
          ...secrets.map((s) => ({ value: s, label: `secret ${s}` })), { value: NEW, label: '+ New login…' }]}
        onChange={(s) => {
          if (s === NEW) { setUser(username); setAdding(true); return; }
          setAdding(false);
          if (!s) onChange(set(o, 'imageCredentials', undefined));
          else if (s !== '\u0000literal') onChange({ ...o, imageCredentials: { username: c ? c.username : '', password: { secret: s } } });
        }} />
      {c && !adding && (
        <label className="b-subf"><span className="muted small">user</span>
          <input placeholder="user" aria-label="registry user" value={username} autoComplete="off"
            onChange={(e) => onChange({ ...o, imageCredentials: { ...c, username: e.target.value } })} /></label>)}
      {literal && !adding && <span className="bad small">the password stands in the clear in the spec: store it as a secret</span>}
      {adding && (
        <div className="b-inline">
          <input placeholder="user" aria-label="registry user" value={user} onChange={(e) => setUser(e.target.value)} autoComplete="off" />
          <input type="password" placeholder="password or token" aria-label="registry password" value={password}
            onChange={(e) => setPassword(e.target.value)} autoComplete="new-password" />
          <span className="muted small">as secret</span>
          <input className="mono" value={sname} aria-label="secret name" onChange={(e) => setSecret(e.target.value)} />
          <button className="primary mini" disabled={busy || !user || !password || !isSecretName(sname)} onClick={save}>Save</button>
          <button className="ghost mini" onClick={() => { setAdding(false); setPassword(''); }}>Cancel</button>
          {err && <span className="bad small">{err}</span>}
        </div>)}
    </Field>
  );
}

// BindingsSection ticks the spec's bindings a service or job gets, and
// adds one from a credential of the tenant (ADR 0033), ticked.
function BindingsSection({ self, o, ctx, findings }) {
  const list = Array.isArray(o.bindings) ? o.bindings : [];
  const all = objectsOf(ctx.spec, 'bindings');
  const missing = list.filter((b) => !all.some(([n]) => n === b));
  // The credentials it does not get yet.
  const got = new Set(all.filter(([bn, b]) => list.includes(bn) && isObject(b) && isObject(b.tenantCredential)).map(([, b]) => b.tenantCredential.name));
  const creds = (ctx.credentials || []).filter((c) => !got.has(c.name));
  const [pick, setPick] = useState('');
  const cred = creds.some((c) => c.name === pick) ? pick : (creds[0] && creds[0].name) || '';
  const to = ctx.tenant ? credentialPath(ctx.tenant) : '';
  return (
    <Section title="Credentials & bindings" hint="APIs its code calls by a name; the platform adds the credential" findings={findings}
      fold={list.length === 0 && all.length === 0}>
      {all.length === 0 && missing.length === 0 && <p className="muted small">The draft has no bindings.</p>}
      <div className="b-checks">
        {all.map(([bn, b]) => (
          <label key={bn} className="b-check">
            <input type="checkbox" checked={list.includes(bn)}
              onChange={(e) => ctx.editSpec((s) => toggleBinding(s, self.key, self.name, bn, e.target.checked))} />
            <span className="mono">{bn}</span><span className="muted small">{bindingSummary(b)}</span>
          </label>))}
        {missing.map((bn) => (
          <label key={bn} className="b-check bad">
            <input type="checkbox" checked onChange={() => ctx.editSpec((s) => toggleBinding(s, self.key, self.name, bn, false))} />
            <span className="mono">{bn}</span><span className="small">not in the draft</span>
          </label>))}
      </div>
      <FindingList list={findings} />
      <div className="b-row wrap b-addcred">
        <span className="muted small">Add a binding from a tenant credential</span>
        {creds.length > 0 ? (
          <>
            <Select value={cred} options={creds.map((c) => ({ value: c.name, label: `${c.name} · ${String(c.upstream || '').replace(/^https?:\/\//, '')}` }))}
              title="a credential of the tenant" onChange={setPick} />
            <button className="mini" disabled={!cred} onClick={() => ctx.editSpec((s) => credentialBinding(s, cred, self.key, self.name).spec)}>
              <Icon name="plus" size={12} /> Add</button>
          </>)
          : <span className="muted small">{ctx.credentialsErr ? `(the credentials cannot be read: ${ctx.credentialsErr})`
            : (ctx.credentials || []).length ? '(it gets every one of them)' : '(the tenant has none yet)'}</span>}
        {to && <a className="small" href={to} onClick={(e) => { if (ctx.navigate && e.button === 0 && !e.metaKey && !e.ctrlKey) { e.preventDefault(); ctx.navigate(to); } }}>
          the tenant's credentials <Icon name="arrowRight" size={11} /></a>}
      </div>
    </Section>
  );
}

// ---------------------------------------------------------------- jobs --

const JOB_FIELDS = ['image', 'imageAsWritten', 'command', 'env', 'resources', 'imageCredentials', 'on', 'timeout', 'retries', 'bindings'];

export function JobForm({ name, obj, ctx, onChange, onRename, onRemove }) {
  const o = isObject(obj) ? obj : {};
  const f = ctx.findings;
  const self = { key: 'jobs', name };
  return (
    <div className="b-form">
      <FormHead kind="Job" name={name} onRename={onRename} findings={f} fields={JOB_FIELDS} />
      <Section title="Container" findings={[...atField(f, 'image'), ...atField(f, 'command'), ...atField(f, 'imageCredentials')]}>
        <ImageField o={o} onChange={onChange} findings={atField(f, 'image')} />
        <Field label="Command and arguments" hint="one per line; empty: the image's entrypoint and cmd" findings={atField(f, 'command')}>
          <Lines value={o.command} onChange={(v) => onChange(set(o, 'command', v))} placeholder="(the image's)" />
        </Field>
        <RegistryLogin o={o} ctx={ctx} onChange={onChange} findings={atField(f, 'imageCredentials')} />
      </Section>
      <Section title="Variables" findings={atField(f, 'env')}>
        <EnvTable self={self} obj={o} ctx={ctx} onChange={onChange} findings={f} />
      </Section>
      <Section title="Run" findings={[...atField(f, 'on'), ...atField(f, 'timeout'), ...atField(f, 'retries')]}>
        <div className="b-grid3">
          <Field label="Trigger" findings={atField(f, 'on')}>
            <Select value={o.on === 'release' ? 'deploy' : o.on || 'deploy'} options={[
              { value: 'deploy', label: 'Each deployment that changes it' }, { value: 'manual', label: 'Manual (Run now)' }]}
            onChange={(v) => onChange(set(o, 'on', v))} />
          </Field>
          <Field label="Timeout" hint="at most 2h" findings={atField(f, 'timeout')}>
            <input className="mono short" placeholder="10m" value={o.timeout || ''} onChange={(e) => onChange(set(o, 'timeout', e.target.value))} />
          </Field>
          <Field label="Retries" hint="0 to 5" findings={atField(f, 'retries')}>
            <OptNumber value={o.retries} min={0} max={5} placeholder="0" onChange={(v) => onChange(set(o, 'retries', v))} />
          </Field>
        </div>
      </Section>
      <Section title="Resources" findings={atField(f, 'resources')}>
        <div className="b-grid3"><ResourcesFields o={o} onChange={onChange} findings={f} /></div>
      </Section>
      <BindingsSection self={self} o={o} ctx={ctx} findings={atField(f, 'bindings')} />
      <FormFoot onRemove={onRemove} what={`job ${name}`} />
    </div>
  );
}

// --------------------------------------------------------------- addons --

export function AddonForm({ name, obj, ctx, onChange, onRename, onRemove, added = false }) {
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
  const current = o.plan || (bp && bp.defaultPlan) || '';
  const refs = referencesTo(ctx.spec, 'addons', name);
  return (
    <div className="b-form">
      <FormHead kind="Addon" name={name} onRename={onRename} findings={f} fields={['type', 'version', 'plan', 'options']} />
      <Section title="Addon" findings={[...atField(f, 'type'), ...atField(f, 'version'), ...atField(f, 'plan')]}>
        <div className="b-grid3">
          <Field label="Type" findings={atField(f, 'type')} hint={added ? 'until it is deployed' : 'fixed: another type is another addon'}>
            {added
              ? <Select value={o.type} options={ctx.catalog ? ctx.catalog.types.map((t) => t.type) : []} onChange={(v) => onChange({ ...o, type: v })} />
              : <span className="mono strong b-fixed">{o.type || '—'}</span>}
          </Field>
          <Field label="Version" findings={atField(f, 'version')} hint={added ? '' : 'a change recreates it (downtime)'}>
            <Select value={o.version} options={bp ? bp.versions : []} empty="(a version)" onChange={(v) => onChange(set(o, 'version', v))} />
          </Field>
        </div>
        <Field label="Plan" findings={atField(f, 'plan')} hint={!added && bp ? 'a change recreates it (downtime)' : ''}>
          <div className="b-plans" role="radiogroup">
            {(bp ? bp.plans : []).map((p) => (
              <label key={p.name} className={'b-plancard' + (current === p.name ? ' on' : '')}>
                <input type="radio" name={`plan-${name}`} checked={current === p.name}
                  onChange={() => onChange(set(o, 'plan', p.name === bp.defaultPlan && !o.plan ? undefined : p.name))} />
                <span className="strong">{p.name}{bp && p.name === bp.defaultPlan ? <span className="muted small"> · default</span> : null}</span>
                <span className="muted small">{planText(p).replace(/^[^·]*·\s*/, '') || '—'}</span>
              </label>))}
            {!bp && <input className="mono short" placeholder="plan" value={o.plan || ''} onChange={(e) => onChange(set(o, 'plan', e.target.value))} />}
          </div>
        </Field>
      </Section>
      <Section title="Options" fold={Object.keys(opts).length === 0 && declared.length === 0} findings={atField(f, 'options')}>
        {declared.map((d) => (
          <Field key={d.name} label={d.name + (d.required ? ' *' : '')} findings={atField(f, `options.${d.name}`)}
            hint={[d.description, hasDefault(d.default) ? `default ${JSON.stringify(d.default)}` : ''].filter(Boolean).join(' · ')}
            problems={optionProblems(d, opts[d.name])}>
            {d.kind === 'string' && <input className="mono" value={opts[d.name] ?? ''} onChange={(e) => putOpt(d.name, e.target.value || undefined)} />}
            {d.kind === 'list' && <Lines value={opts[d.name]} onChange={(v) => putOpt(d.name, v)} placeholder="one per line" />}
            {d.kind === 'boolean' && (
              <Select value={opts[d.name] === undefined ? '' : String(opts[d.name])} options={['true', 'false']} empty="(the default)"
                onChange={(v) => putOpt(d.name, v === '' ? undefined : v === 'true')} />)}
            {d.kind === 'oneOf' && (
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
      </Section>
      <Section title="Used by">
        {refs.length === 0 ? <p className="muted small">Nothing references it yet: give a variable its output.</p>
          : <ul className="b-uses">{refs.map((r, i) => <li key={i} className="mono small">{r.from.name} · {refLine(r)}</li>)}</ul>}
      </Section>
      <FormFoot onRemove={onRemove} what={`addon ${name}`} />
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
  const ports = svc ? portsOf(svc[1]).map(String) : [];
  return (
    <div className="b-form">
      <FormHead kind="Route" name={name} onRename={onRename} findings={f} fields={['path', 'service', 'port', 'strip', 'host']} />
      <Section title="Route" findings={f}>
        <Field label="Path" hint="requests whose path starts with it, by segments: /api takes /api and /api/…, not /apis" findings={atField(f, 'path')}>
          <input className="mono" placeholder="/" value={o.path || ''} onChange={(e) => onChange({ ...o, path: e.target.value })} />
        </Field>
        <div className="b-grid3">
          <Field label="Service" findings={atField(f, 'service')}>
            <Select value={o.service} options={services.map(([n]) => n)} empty="(a service)" onChange={(v) => {
              const s = services.find(([n]) => n === v);
              onChange(without({ ...o, service: v, port: s ? portsOf(s[1])[0] : undefined }));
            }} />
          </Field>
          <Field label="Port" findings={atField(f, 'port')}>
            <Select value={o.port === undefined ? '' : String(o.port)} options={ports} empty="(a port)"
              onChange={(v) => onChange(set(o, 'port', portValue(v)))} />
          </Field>
        </div>
        <label className="b-check">
          <input type="checkbox" checked={!!o.strip} onChange={(e) => onChange(set(o, 'strip', e.target.checked || undefined))} />
          <span>Strip the path prefix</span>
          <span className="muted small">{o.path && o.path !== '/' ? `${o.path}/x reaches the service as /x` : 'the service sees the path without it'}</span>
        </label>
        <FindingList list={atField(f, 'strip')} />
      </Section>
      <FormFoot onRemove={onRemove} what={`route ${o.path || name}`} />
    </div>
  );
}

// ------------------------------------------------------------- bindings --

const BINDING_FIELDS = ['upstream', 'mode', 'caPem', 'pathPrefixes', 'forwardedHost', 'rateLimit', 'credential', 'tenantCredential'];

// BindingForm is a binding: a credential of the tenant (ADR 0033), or,
// folded as advanced, the binding stated here with ADR 0028's fields,
// its secret fields the environment's secrets.
export function BindingForm({ name, obj, ctx, onChange, onRename, onRemove }) {
  const o = isObject(obj) ? obj : {};
  const f = ctx.findings;
  const tc = isObject(o.tenantCredential) ? o.tenantCredential : null;
  const creds = ctx.credentials || [];
  const cred = tc ? creds.find((c) => c.name === tc.name) : null;
  const users = referencesTo(ctx.spec, 'bindings', name);
  const to = ctx.tenant ? credentialPath(ctx.tenant, tc ? tc.name : '') : '';
  return (
    <div className="b-form">
      <FormHead kind="Binding" name={name} onRename={onRename} findings={f} fields={BINDING_FIELDS} />
      <Section title="Credential" findings={atField(f, 'tenantCredential')}>
        <span className="b-seg">
          <button className={'ghost mini' + (tc ? ' on' : '')} aria-pressed={!!tc}
            onClick={() => { if (!tc) onChange({ tenantCredential: { name: (creds[0] && creds[0].name) || '' } }); }}>A tenant credential</button>
          <button className={'ghost mini' + (tc ? '' : ' on')} aria-pressed={!tc}
            onClick={() => { if (tc) onChange({ upstream: cred ? cred.upstream : 'https://', mode: cred ? cred.mode : 'name' }); }}>Stated here (advanced)</button>
        </span>
        {tc && (
          <>
            <Field label="Credential" findings={atField(f, 'tenantCredential.name')}
              hint={cred ? <>{cred.upstream} · reached as {cred.mode === 'intercept' ? 'its own name' : <span className="mono">http://{name}/</span>}</>
                : creds.length ? 'not a credential of the tenant' : ctx.credentialsErr || 'the tenant has no credentials yet'}>
              <Select value={tc.name} options={creds.map((c) => c.name)} empty="(a credential)"
                onChange={(v) => onChange({ ...o, tenantCredential: { name: v } })} />
              {to && <a className="small" href={to} onClick={(e) => { if (ctx.navigate && e.button === 0 && !e.metaKey && !e.ctrlKey) { e.preventDefault(); ctx.navigate(to); } }}>
                {tc.name && cred ? `credential ${tc.name}` : "the tenant's credentials"} <Icon name="arrowRight" size={11} /></a>}
            </Field>
            <Field label="Version" findings={atField(f, 'tenantCredential.version')}
              hint={tc.version ? 'pinned: a new version reaches it with a deployment that pins it' : 'the deployment pins the latest version when it is applied'}>
              <span className="mono">{tc.version ? `v${tc.version}` : 'latest'}</span>
              {cred && tc.version && cred.version > tc.version && (
                <><span className="warntext small">v{cred.version} is newer</span>
                  <button className="ghost mini" onClick={() => onChange({ ...o, tenantCredential: { ...tc, version: cred.version } })}>Use v{cred.version}</button></>)}
            </Field>
          </>)}
        {!tc && <LiteralBinding o={o} ctx={ctx} onChange={onChange} findings={f} />}
      </Section>
      <Section title="Used by">
        {users.length === 0 ? <p className="muted small">No service or job gets it yet: tick it in their Credentials &amp; bindings.</p>
          : <ul className="b-uses">{users.map((r, i) => <li key={i} className="mono small">{r.from.key === 'jobs' ? 'job' : 'service'} {r.from.name}</li>)}</ul>}
      </Section>
      <FormFoot onRemove={onRemove} what={`binding ${name}`} />
    </div>
  );
}

// LiteralBinding is ADR 0028's binding: upstream, mode, paths, rate
// limit, CA and the credential, each value field text or a secret.
function LiteralBinding({ o, ctx, onChange, findings }) {
  const c = isObject(o.credential) ? o.credential : null;
  const kind = c ? c.kind : '';
  const secrets = [...new Set([...(ctx.knownSecrets || []), ...objectsOf(ctx.spec, 'secrets').map(([n]) => n)])].sort();
  const targets = { addons: [], services: [], secrets };
  const rl = isObject(o.rateLimit) ? o.rateLimit : {};
  const putCred = (patch) => onChange({ ...o, credential: without({ ...c, ...patch }) });
  const headers = c && isObject(c.headers) ? c.headers : {};
  return (
    <div className="b-rows">
      <Field label="Upstream" hint="https://host[:port]; plain http is refused" findings={atField(findings, 'upstream')}>
        <input className="mono" placeholder="https://api.example.com" value={o.upstream || ''} onChange={(e) => onChange({ ...o, upstream: e.target.value })} />
      </Field>
      <div className="b-grid3">
        <Field label="Reached as" findings={atField(findings, 'mode')}>
          <Select value={o.mode || 'name'} options={[{ value: 'name', label: 'its binding name (http://name/)' }, { value: 'intercept', label: 'its own name (intercept)' }]}
            onChange={(v) => onChange(set(o, 'mode', v === 'name' ? undefined : v))} />
        </Field>
        <Field label="Requests per second" hint="per pod; empty: no limit" findings={atField(findings, 'rateLimit')}>
          <OptNumber value={rl.perSecond} min={1} placeholder="—" onChange={(v) => {
            const next = without({ ...rl, perSecond: v });
            onChange(set(o, 'rateLimit', Object.keys(next).length ? next : undefined));
          }} />
        </Field>
        <Field label="Burst">
          <OptNumber value={rl.burst} min={0} placeholder="the rate" onChange={(v) => {
            const next = without({ ...rl, burst: v });
            onChange(set(o, 'rateLimit', Object.keys(next).length ? next : undefined));
          }} />
        </Field>
      </div>
      <Field label="Paths" hint="the path prefixes the pods may reach, one per line; none: all" findings={atField(findings, 'pathPrefixes')}>
        <Lines value={o.pathPrefixes} onChange={(v) => onChange(set(o, 'pathPrefixes', v))} placeholder="/v1/" />
      </Field>
      <label className="b-check"><input type="checkbox" checked={!!o.forwardedHost}
        onChange={(e) => onChange(set(o, 'forwardedHost', e.target.checked || undefined))} /><span>Send X-Forwarded-Host and -Proto</span></label>
      <Field label="Upstream CA (PEM)" hint="empty: the WebPKI" findings={atField(findings, 'caPem')}>
        <textarea className="mono b-lines" rows={2} placeholder="-----BEGIN CERTIFICATE-----" value={o.caPem || ''}
          onChange={(e) => onChange(set(o, 'caPem', e.target.value))} />
      </Field>
      <Field label="Credential" hint="what the platform adds to each request; secret fields name the environment's secrets" findings={atField(findings, 'credential')}>
        <Select value={kind} empty="None" options={[{ value: 'headers', label: 'Headers' }, { value: 'basic', label: 'Basic' },
          { value: 'oauth2', label: 'OAuth2 client credentials' }, { value: 'clientCertificate', label: 'Client certificate' }]}
        onChange={(k) => onChange(set(o, 'credential', k ? { kind: k } : undefined))} />
      </Field>
      {kind === 'headers' && (
        <table className="b-vars">
          <thead><tr><th>Header</th><th>Value</th><th /></tr></thead>
          <tbody>
            {Object.entries(headers).map(([h, v]) => (
              <tr key={h}>
                <td className="b-varcell"><VarName name={h} onRename={(to) => putCred({ headers: renameVar(headers, h, to) })} /></td>
                <td className="b-valcell"><CredValue v={v} targets={targets} ctx={ctx} onChange={(nv) => putCred({ headers: { ...headers, [h]: nv } })} /></td>
                <td className="b-delcell"><button className="iconbtn" title={`remove ${h}`} onClick={() => {
                  const next = { ...headers };
                  delete next[h];
                  putCred({ headers: Object.keys(next).length ? next : undefined });
                }}><Icon name="x" size={13} /></button></td>
              </tr>))}
          </tbody>
        </table>)}
      {kind === 'headers' && <span><button className="ghost mini" onClick={() => putCred({ headers: { ...headers, [freeName(headers, 'Authorization')]: { secret: '' } } })}>
        <Icon name="plus" size={12} /> Add a header</button></span>}
      {kind && (CREDENTIAL_FIELDS[kind] || []).map((fd) => (
        <Field key={fd} label={fd} findings={atField(findings, `credential.${fd}`)}>
          {fd === 'tokenUrl'
            ? <input className="mono" placeholder="https://auth.example.com/token" value={textOf(c[fd])} onChange={(e) => putCred({ [fd]: e.target.value || undefined })} />
            : <CredValue v={c[fd] === undefined ? '' : c[fd]} targets={targets} ctx={ctx} onChange={(nv) => putCred({ [fd]: nv === '' ? undefined : nv })} />}
        </Field>))}
      {kind === 'oauth2' && (
        <label className="b-check"><input type="checkbox" checked={!!c.clientAuthInBody}
          onChange={(e) => putCred({ clientAuthInBody: e.target.checked || undefined })} /><span>Client authentication in the body</span></label>)}
    </div>
  );
}

// CredValue is a binding credential's value: text or a secret.
function CredValue({ v, targets, ctx, onChange }) {
  const type = valueType(v) === 'secret' ? 'secret' : 'text';
  return (
    <div className="b-value">
      <Select value={type} options={[{ value: 'text', label: 'Text' }, { value: 'secret', label: 'Secret' }]} className="b-vtype"
        onChange={(t) => onChange(retypeValue(v, t, targets))} />
      {type === 'text'
        ? <input className="mono grow" value={textOf(v)} placeholder="value" onChange={(e) => onChange(withText(v, e.target.value))} />
        : <SecretValue name="" v={v} targets={targets} ctx={ctx} onChange={onChange} />}
    </div>
  );
}

// NewBinding is "+ Add" of the bindings: a credential of the tenant, or
// a binding stated here.
export function NewBinding({ ctx, onAdd }) {
  const creds = ctx.credentials || [];
  const [pick, setPick] = useState('');
  const [name, setName] = useState('');
  const cred = pick || (creds[0] && creds[0].name) || '';
  return (
    <div className="b-form">
      <FormHead kind="New binding" name="an API its code calls" findings={[]} fields={[]} />
      <Section title="From a tenant credential">
        <p className="muted small">The tenant states an API and its credential once (ADR 0033); a binding of it names it, and the
          deployment pins its version.</p>
        {creds.length > 0
          ? <div className="b-row wrap">
            <Select value={cred} options={creds.map((c) => ({ value: c.name, label: `${c.name} · ${String(c.upstream || '').replace(/^https?:\/\//, '')}` }))} onChange={setPick} />
            <button className="primary mini" disabled={!cred} onClick={() => onAdd(credentialBinding(ctx.spec, cred))}>
              <Icon name="plus" size={12} /> Add binding {cred}</button>
          </div>
          : <p className="muted small">{ctx.credentialsErr ? `The credentials cannot be read: ${ctx.credentialsErr}` : 'The tenant has no credentials yet.'}</p>}
      </Section>
      <Section title="Stated here (advanced)" fold>
        <p className="muted small">An upstream and a credential whose secret fields are this environment's secrets (ADR 0028).</p>
        <div className="b-row wrap">
          <input className="mono" placeholder="name" value={name} onChange={(e) => setName(e.target.value.trim())} />
          <button className="mini" disabled={!isDnsLabel(name) || objectsOf(ctx.spec, 'bindings').some(([n]) => n === name)}
            onClick={() => onAdd({ spec: { ...ctx.spec, bindings: { ...(ctx.spec.bindings || {}), [name]: { upstream: 'https://', mode: 'name' } } }, name })}>
            <Icon name="plus" size={12} /> Add</button>
        </div>
      </Section>
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
      <FormHead kind="Secret" name={name} findings={ctx.findings} fields={['version']} />
      <Section title="Versions" findings={atField(ctx.findings, 'version')}>
        <div className="kv-grid">
          <span className="k">pinned</span>
          <span className="v">{pin.pinned ? <>version <span className="mono">{pin.pinned}</span></> : <span className="muted">the latest when deployed</span>}</span>
          <span className="k">latest</span>
          <span className="v">
            {pin.latest ? <>version <span className="mono">{pin.latest}</span></>
              : pin.missing ? <span className="bad">missing: the environment has no such secret</span>
                : <span className="muted">set when the environment is made</span>}
            {pin.newer && (
              <> · <span className="warntext">version {pin.latest}, pinned {pin.pinned}</span>{' '}
                <button className="ghost mini" onClick={() => onPin(pin.latest)}>Use version {pin.latest}</button></>)}
          </span>
        </div>
        <FindingList list={atField(ctx.findings, 'version')} />
        {login && <p className="panel-note">A registry login reads it at its latest version whenever a pod is created, not at the pin.</p>}
        <Field label={pin.missing ? 'Set a value' : 'Set a new value'} wide
          hint={ctx.isNew ? 'kept in this page only until the first deploy, which writes it first' : 'written to the environment at once; the draft pins the version it makes'}>
          <input type="password" className="grow" autoComplete="new-password" placeholder="the value" aria-label="secret value" value={value}
            onChange={(e) => setValue(e.target.value)} />
          <button className="primary mini" disabled={busy || !value} onClick={save}>
            {pin.missing ? 'Set a value' : 'Save as a new version'}</button>
          {err && <span className="bad small">{err}</span>}
        </Field>
      </Section>
      <Section title="Used by">
        {refs.length === 0 ? <p className="muted small">Nothing references it: a deployment drops its pin.</p>
          : <ul className="b-uses">{refs.map((r, i) => <li key={i} className="mono small">{r.from.name} · {refLine(r)}</li>)}</ul>}
      </Section>
      <FormFoot onRemove={onRemove} what={`secret ${name}`} />
    </div>
  );
}

// NewSecret is "+ Add" of the secrets: a name and a value, written at
// once (a new environment's kept until its first deploy).
export function NewSecret({ ctx, onDone }) {
  const [name, setName] = useState('');
  const [value, setValue] = useState('');
  const [err, setErr] = useState('');
  const [busy, setBusy] = useState(false);
  const exists = (ctx.knownSecrets || []).includes(name) || objectsOf(ctx.spec, 'secrets').some(([n]) => n === name);
  return (
    <div className="b-form">
      <FormHead kind="New secret" name="a value the services read" findings={[]} fields={[]} />
      <Section title="Secret">
        <p className="muted small">{ctx.isNew
          ? 'Its value stays in this page until the first deploy, which writes it before the deployment.'
          : 'Its value goes to the environment at once and never into the draft, this browser\'s storage or an export.'}</p>
        <Field label="Name" hint={name && !isSecretName(name) ? "a-z, 0-9, '.', '_' and '-'; \"addon.\" is the platform's" : ''}>
          <input className="mono" placeholder="stripe-key" value={name} onChange={(e) => setName(e.target.value.trim())} />
        </Field>
        <Field label="Value">
          <input type="password" className="grow" autoComplete="new-password" placeholder="the value" value={value}
            onChange={(e) => setValue(e.target.value)} />
        </Field>
        {exists && <p className="bad small">{name} exists already: open it to set a new version.</p>}
        <div className="b-row">
          <button className="primary" disabled={busy || !value || !isSecretName(name) || exists} onClick={async () => {
            setBusy(true);
            setErr('');
            try {
              await ctx.setSecret(name, value);
              onDone(name);
            } catch (e) {
              setErr(e.message);
            } finally {
              setBusy(false);
            }
          }}>{ctx.isNew ? 'Keep for the first deploy' : 'Save the secret'}</button>
          {err && <span className="bad small">{err}</span>}
        </div>
      </Section>
    </div>
  );
}
