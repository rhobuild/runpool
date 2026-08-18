# A deployment authenticates as a GitHub App, not only as a person

**Status:** accepted; implementation pending
**Date:** 2026-08-17

`credentials[].type` accepts `token` and nothing else, and the adapter
constructs its client through the personal access token path alone.
Every deployment therefore runs on a credential that belongs to a human
being: it carries that person's permissions, it appears in that person's
account, and it stops working when they leave or rotate it.

## What the provider library already does

The vendored client has a GitHub App constructor. It takes a client id,
an installation id and a private key in PEM form; it mints its own JWT
with a nine-minute expiry, exchanges it for an installation access
token, and refreshes the Actions service token before expiry on its own.

The deployment documentation says Runpool "does not refresh GitHub App
installation tokens". That describes which constructor the adapter
calls, not a limitation of the protocol or of the library. Nothing about
the App lifecycle has to be implemented here.

## Decision

**A credential declares a type, and `github_app` is one of them.**

```yaml
credentials:
  - id: runners
    type: github_app
    clientID: Iv1.0123456789abcdef
    installationID: 12345678
    privateKeyFile: /run/secrets/runpool/app.pem
```

The private key follows the rule the token already follows: the value
never appears in configuration. `privateKeyFile` names a mounted secret
or `privateKeyEnv` names an environment variable, exactly one of the
two, and a file is subject to the same owner-only mode check a token
file is — a key readable by group or other is refused, not warned about.

`token` keeps its meaning and its shape. A deployment that has a working
personal access token does not have to change.

**`runpool doctor` proves the credential the same way for both types.**
The existing check resolves a runner group through a live call; it does
so through whichever constructor the credential names, so an App with a
missing installation or an unreadable key fails at `doctor` rather than
at the first job.

## Consequences

- An organization can run Runpool on a credential that belongs to the
  organization. Membership changes stop being an outage.
- Installation scope replaces personal permission scope: an App is
  installed on the repositories or the organization it needs, and it
  carries nothing else.
- The deployment holds a private key on disk. It is a longer-lived
  secret than a token and it is the one credential whose leak cannot be
  contained by revoking a single token, so it takes the strictest mode
  check the credential loader has.
- The refresh loop lives in the provider library, so a token expiring
  mid-job is the library's concern rather than a lifecycle Runpool
  reimplements.
- The credential probe is the only live exercise of the App path that
  runs outside a job.
