# ADR 0033: Credentials of an environment: a binding stated once, versioned, used by name

**Status:** Superseded by [ADR 0055](0055-a-secret-is-a-value-or-an-http-api.md) · **Date:** 2026-11-06, corrected 2026-11-10

It decided that an HTTP binding's whole definition is an object of an
environment, a credential, stated once under a name, versioned, its key
bound to its upstream: a sandbox names it at its latest version, a
spec's binding names it by `credentialRef` at a pinned one, and
environments share none. ADR 0055 keeps all of that and makes the
object a secret of kind `http`, the one place an HTTP API is stated: a
workload gets it by a variable that references it, spec bindings and a
sandbox's own set give way, and the credentials of before are HTTP
secrets of the same names and versions. Its alternatives and their
reasons stand there.
