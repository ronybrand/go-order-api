# Security Policy

## Supported versions

Single-branch project (no maintained release lines): security fixes land on `main` only. There is no
LTS or backport policy.

## Reporting a vulnerability

Please **do not** open a public GitHub issue for a suspected vulnerability. Use
[GitHub's private vulnerability reporting](../../security/advisories/new) for this repository
(Security tab, "Report a vulnerability"), or contact the maintainer through the email on the
[GitHub profile](https://github.com/ronybrand).

Include, where applicable: a description and the potential impact, steps to reproduce (a minimal
request is ideal), and the affected endpoint or component.

The project is maintained on a best-effort basis (no SLA), but reports are taken seriously.

## Scope

In scope: the code in this repository (`cmd/`, `internal/`), the notification worker, and the
Dockerfile and CI workflows that build and ship them.

Out of scope: third-party services used for local development (PostgreSQL, RabbitMQ, Mailpit);
report those upstream. Dependency vulnerabilities are tracked by Dependabot and `govulncheck` (run in CI)
instead of manual reports.

## What the project already does

- Weekly Dependabot updates, auto-merged (patch and minor only) after CI passes; actions pinned by SHA.
- `govulncheck` and `golangci-lint` (with `gosec`, `govet` and `gofmt`) enforced in CI; tests run with `-race`.
- RS256 JWT verification only (no signing key in the service), with required `exp`, `aud` and `iss`
  (see [ADR 0003](docs/adr/0003-jwt-rs256-somente-verificacao.md)).
- Security headers, a request body limit that also counts chunked uploads, per-user, write and per-IP rate limits.
- Production configuration fails fast: missing JWT audience or issuer, empty or wildcard CORS, or a
  non-`amqps://` broker stops the process at startup; SMTP requires STARTTLS.
- No secrets in the repository: configuration comes from environment variables.
- PII (tax id, passport, e-mail) is never logged (`LogValue` on the types that carry it) and is never
  returned by the customer endpoints.
