# Security policy

## Reporting a vulnerability

Please do not open a public issue for security problems.

Use GitHub's private reporting: **Security → Report a vulnerability** on this
repository, or open a draft advisory at
`https://github.com/NemoNobody/velruna/security/advisories/new`. You will get a
response within a few days.

## Scope and notes

- `velruna` stores facts in **plaintext by design**. A best-effort secret
  guard (default mode `reject`) scans writes: a high-confidence credential
  shape refuses the write, while a medium-confidence heuristic hit (long
  hex/base64, a weak assignment) is replaced with
  `[velruna-redacted:pattern]` before the fact is stored — the answer lists
  `redactions`, so a stored value can visibly differ from the one written (a
  marker can be longer or shorter than the span it replaced).
  It is not protection: "Never store secrets" remains a rule for agents (see
  the README). Reports about the store not being encrypted are by design.
- The OS keyring is **best-effort per platform**: Secret Service on Linux
  (gnome-keyring, or KWallet serving the name; plus a KWallet-native D-Bus
  fallback when no usable Secret Service provider is available — no owner, or
  gnome-keyring on KDE), Keychain on macOS. On Windows the
  binary uses Credential Manager through the same library and the `windows` CI
  job exercises it, but the policy around a locked store or a dismissed prompt
  is unexplored — a config file only the user can read or `OPENSEARCH_PASSWORD`
  stays the reproducible path. When the backend is unusable, the secret is not
  written at all — there is no silent success: `velruna creds set` fails with a
  hint instead. On KDE where gnome-keyring owns `org.freedesktop.secrets`, that
  provider is refused explicitly (its login collection uses an ephemeral master
  key, so the password would not survive a logout).
- Relevant areas: credential handling (`velruna.conf`, environment variables),
  the MCP server (stdio), request construction to OpenSearch, and anything that
  could leak a secret into logs or outputs.
- The OpenSearch cluster itself is out of scope — report those to the OpenSearch
  project or your cluster operator.
- On a shared cluster, run `velruna` as a least-privilege store user: the
  recipes (role, user, role mapping) are in
  [docs/setup.md](docs/setup.md) §7. The role deliberately cannot delete the
  facts index; authorship labels stamped on writes come from the account the
  server authenticates.
