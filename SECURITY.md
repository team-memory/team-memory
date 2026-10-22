# Security policy

## Reporting a vulnerability

Please do not open a public issue for a security problem.

**Contact:** use GitHub's private vulnerability reporting on this repository (Security tab, "Report a vulnerability"), or email `tyler@lafronz.com` with "team-memory security" in the subject.

Include affected version or commit, reproduction steps, and impact. Do not include real credentials or other people's data.

This is a single-maintainer, pre-release project. There is no commercial support or guaranteed response time; the maintainer aims to acknowledge reports within a week.

## Supported versions

Only the latest release (or `main` before the first tag) receives security fixes.

## Scope and known limits

The security model, residual risks and prompt-injection limits are in `docs/security/threat-model.md` and `docs/security/limits.md`. No external security review has been done; one is recommended before relying on Team Memory for sensitive data.

Secrets accidentally committed to memories can be removed with the procedure in `docs/operations/secret-purge.md`.
