# ADR 0037: An environment can be renamed; its ID is its identity

**Status:** Merged into [ADR 0018](0018-environments-and-deployments.md) · **Date:** 2026-11-08

It decided that an environment's ID is its identity and its name a
handle that one call changes: everything FeRun keeps or makes for an
environment is keyed by the ID, a rename changes the name in answers,
paths and the console alone, and an attached service's process is named
by the environment's ID rather than its name. The rename lives in
ADR 0018 (the environment's name and identity); the process's name lives
in [ADR 0042](0042-attached-services.md).
