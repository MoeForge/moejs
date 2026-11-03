# Security policy

## Reporting a vulnerability

Report it privately through GitHub: open the Security tab of this repository
and choose "Report a vulnerability", or go straight to
https://github.com/MoeForge/moejs/security/advisories/new. Only you and
the maintainers can see the report. Include the JavaScript and the Go code
that reproduce the problem.

We publish the advisory after a fix is released, with credit to you if you
want it.

## What counts as a vulnerability

moejs runs plugin JavaScript that the host program may not trust, inside a
host program that it does trust. A vulnerability is plugin code doing
something the host did not give it. For example, plugin code that can:

- call Go functions or read Go data beyond the globals the host installed
  and the modules its resolver returned
- make a moejs call panic instead of returning an error, crash the process
  or corrupt memory
- read or change the state of another runtime, or change the builtins that
  runtimes share
- allocate far more than `Options.MemoryLimit` before it stops (the count is
  approximate and runs within a small factor of what a request allocates)
- keep running after the host calls `Interrupt`

The host decides what a plugin may do. A host function that reads files
lets plugins read files, and a plugin can run and allocate for as long as
the host lets it: set a time limit with `Interrupt` and a memory budget
with `Options.MemoryLimit`. [docs/guide.md](docs/guide.md) shows how.

## Supported versions

moejs is in alpha. Fixes go into the main branch and the next release, so
use the latest release.
