# Security Policy

## Supported Versions

Nistru is pre-1.0. Only the latest `v0.x.y` release receives security fixes.

| Version   | Supported          |
| --------- | ------------------ |
| latest `v0.x.y` | Yes          |
| older `v0.x.y`  | No           |

## Reporting a Vulnerability

Please report security issues **privately**. Do not open a public GitHub issue.

File a private report via GitHub's Security Advisories:
<https://github.com/savimcio/nistru/security/advisories/new>

That channel is end-to-end private between the reporter and the maintainer(s) until a fix ships and a coordinated advisory is published. See GitHub's [privately reporting a security vulnerability](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability) docs if you haven't used it before.

Please include:

- A description of the issue and its impact.
- Exact reproduction steps or a proof-of-concept.
- The Nistru commit SHA or tag, Go version, OS, and terminal.
- Whether you have a suggested fix.

You should receive an acknowledgment within **72 hours**. PGP/GPG is not required; a key can be provided on request.

## Scope

In scope — examples of what we consider a security issue:

- Host ↔ plugin isolation bypass (a plugin reading or modifying host state it
  was not granted).
- Path traversal when loading plugin config, plugin binaries, or editor files
  from disk.
- Memory-safety or panics in the JSON-RPC codec when fed untrusted bytes.
- Authentication or capability checks that can be trivially bypassed.

Out of scope:

- Feature requests framed as security issues.
- Theoretical denial-of-service without a concrete trigger or proof of concept.
- Issues that require an already-compromised local account or a malicious
  plugin the user has explicitly installed and trusted (plugins are code — if
  you install a hostile one, it's hostile).
- Vulnerabilities in third-party dependencies without a demonstrated impact on
  Nistru; please report those upstream.

## Disclosure Policy

We practice coordinated disclosure. Our target is to ship a fix and publish an
advisory within **90 days** of acknowledging a valid report, unless the
reporter agrees to extend the window. Credit is offered to the reporter in the
advisory unless you ask us not to.
