# Provider parity fixes: skill drift, metadata patch, credential consistency, org pin

Status: implemented; validation results below
Date: 2026-09-18
Owner: unassigned

## Implementation outcomes (2026-10-15)

Implemented against current `main` (base `43eb94f`), including its federation support.
The SDK remains pinned to v1.67.0. The original proposal below is retained for context;
these measured outcomes and implementation details supersede its open assumptions.

- **Probe A, live `terraform-tests`:** agents and environments both preserved `{a="1", b="2"}`
  after an update containing only `{a="1"}`, and after `{}`. Explicit per-key nulls
  produced an empty bag. Reads were taken 1.3 seconds after each write. Both now use
  the shared metadata patch helper, along with vaults and vault credentials.
- **SDK check:** both agent and environment update parameter structs embed the SDK
  parameter metadata and expose `SetExtraFields`; no override fallback is needed.
- **Probe B, live `terraform-tests`:** three credential display-name update trials,
  polling every 100ms for 3 seconds, observed zero stale reads (6 reads per trial).
  First reads reflected the new field and timestamp at 425ms, 409ms and 388ms.
  The immediate read after deleting the credential returned 404. This does not rule
  out a shorter or intermittent window; no credential consistency wait was added.
  Temporary probe resources were cleaned up.
- **Skill tracking:** computed relative-path SHA-256 maps drive in-place skill version
  publication and explicit immutable-version replacement. Existing/imported state adopts
  missing hashes without a write solely for adoption; actual established file-list
  changes still take effect. State-only updates persist `force_destroy`. Apply checks
  known planned hashes before uploads. Reads retain the recorded baseline.
- **Organization identity, read-only live check:** the configured standard API key
  successfully retrieved an organization ID from `/v1/organizations/me` (HTTP 200).
  The pin checks every configured client, including standard keys and federated OAuth,
  and fails closed if any lookup fails or mismatches. OAuth/admin behavior is covered
  by local HTTP-server tests; no live OAuth/Admin mutation was performed.
- **Pinned skill versions:** deletion behavior for an agent-pinned version is not
  live-verified. Resource documentation warns about deletion failure or affected consumers
  and describes replacement ordering; no active agent/session is used for validation.

Validation: `make` passed (formatting, module tidiness, golangci-lint with zero
issues, the full Go unit suite, build/install and documentation generation).
Seven targeted live acceptance cases passed across the final runs: skill basics,
content updates including file additions/removals and relocation, import adoption
for skills and versions, immutable-version basics/replacement, and metadata
removal on agents and environments. The live import test confirms adoption does
not publish a version. All pre-commit hooks passed, including Terraform lint, actionlint, poutine, semgrep
and Checkov. The tflint binary was installed under `/tmp` using the version and
checksum pinned in CI; mise trust was scoped to the worktree process.

## Why now

Anthropic shipped their own Terraform provider into private beta and asked us for feedback.
Reading their docs against ours surfaced four things worth doing on our side. Three are bugs
in this provider — silent divergence between config and the API. The fourth is a safety rail
they designed well and we lack.

This is not a migration plan. We are staying on this provider; their beta has no organization
administration (workspaces, members, rate limits, API keys), which is half of what we use
Terraform for, and it is distributed as a tarball with no lockfile pinning. Nothing below
reshapes our schemas to match theirs.

## Sequencing

Two of the four items are gated on a measurement, because the fix depends on what the API
actually does. Run both probes first; each is well under an hour against the `terraform-tests`
workspace.

| Order | Work | Gate |
|---|---|---|
| 1 | Probe A — metadata semantics on agents and environments | gates item 2 |
| 2 | Probe B — vault credential read-after-write | gates item 3 |
| 3 | Item 1 — skill file hashing | none |
| 4 | Item 2 — metadata patch | Probe A |
| 5 | Item 3 — credential consistency wait | Probe B |
| 6 | Item 4 — organization pin | none |

One PR per item. Items 1 and 4 can start immediately and in parallel with the probes.

---

## 1. Skill file contents are invisible to Terraform

### What happens today

`anthropic_skill` and `anthropic_skill_version` take `files` as a list of local **paths**
(`skill_resource.go:72`, `skill_version_resource.go:72`). Nothing hashes the contents. Both
resources declare `Update` a no-op because every attribute is ForceNew
(`skill_resource.go:195`, `skill_version_resource.go:189`).

Two consequences:

- Edit `SKILL.md` in place and `terraform plan` reports no changes. The repo and the deployed
  skill diverge silently, with no signal anywhere. This is the failure mode worth fixing.
- Add or remove a path and the `RequiresReplace` on `files` **destroys and recreates the
  skill** — new ID, and `Delete` fails outright when versions exist unless `force_destroy`
  is already true in state.

### Design

Hash file contents into state and make a content change publish a new version instead of
replacing the skill.

- Add a computed `file_hashes` map attribute: key is the file's path **relative to the bundle
  root** (what `provretry.DeriveBundleRoot` returns), value is its SHA-256. Keying on the
  relative path means moving the bundle directory on disk does not churn the hashes; only
  contents and bundle structure do.
- Implement `ModifyPlan` on both resources. It reads the planned `files` from disk, computes
  the map, and writes it into the plan. A missing or unreadable file becomes a plan-time
  error rather than an apply-time one, which is the behaviour G-8 asks for.
- On `anthropic_skill`: drop `RequiresReplace` from `files` and implement a real `Update`
  that uploads a new version through `Beta.Skills.Versions.New` and refreshes
  `latest_version`. The skill ID stays stable, so agents referencing the skill keep working.
- On `anthropic_skill_version`: keep `RequiresReplace`. A version is immutable by definition,
  so a content change means publish a new version and drop the old one.

### Upgrade path

This is the part that needs care. Existing state has no `file_hashes`. On the first plan
after the upgrade, `ModifyPlan` computes hashes from disk, the prior value is null, and
Terraform shows an in-place update — which would publish a new version of every skill in
every config the moment someone upgrades.

So `Update` must treat a null prior-state hash map as adoption: record the hashes, upload
nothing. Only a map that was present and has changed triggers a new version. Cover this
explicitly in a unit test; it is the kind of thing that looks like an edge case and then
bills everyone a version bump.

### Tests

- Unit: hash map construction from a temp bundle, including a nested subdirectory; stable
  under a bundle-root move; changed on a content edit.
- Unit: `Update` with a null prior hash map uploads nothing.
- Acceptance: create a skill, rewrite a file's contents, apply, assert `latest_version`
  advanced and `id` did not.
- Acceptance: the same edit on `anthropic_skill_version` replaces the version.

### Risks

- Replacing a skill version deletes it. An agent pinned to that version breaks. Document it
  on the resource page; consider recommending `create_before_destroy`.
- `ModifyPlan` reads the filesystem at plan time. That is already true of apply, so any
  runner that can apply can plan, but it does mean a plan on a machine without the bundle
  now fails where it previously succeeded quietly. That is the correct trade.

### Size

Roughly 400–500 lines including tests. Largest of the four.

---

## 2. Metadata key removal on agents and environments

### What happens today

We fixed this for vaults. `buildMetadataPatch` (`vault_resource.go:405`) upserts every
planned key and sets keys removed since the prior state to null, and its own doc comment
scopes it to "vaults and vault credentials" (`vault_resource.go:319`,
`vault_credential_resource.go:611`).

Agents and environments never got it:

- `agent_resource.go:752` — sends the planned map, or `{}` when metadata goes null.
- `environment_resource.go:412` — sends the planned map, or **nothing at all** when metadata
  goes null.

Under PATCH semantics (omitted keys preserved, explicit null deletes), removing one key from
a metadata map leaves it on the server; the response then carries a key the plan did not
have, and Terraform fails the apply with "Provider produced inconsistent result after apply."
Dropping the block entirely is a silent no-op in both resources.

### What is verified and what is not

PATCH semantics are confirmed on the **vaults** endpoint only. They are an assumption for
agents and environments. This matters, because under replace semantics the agent's `{}` is
already correct and only the environment is wrong. Do not write code before Probe A.

### Probe A

Against the `terraform-tests` workspace, with the standard API key:

1. Create an agent with `metadata = {a = "1", b = "2"}`.
2. Update sending only `{a = "1"}`. `GET` the agent. Is `b` still there?
3. Update sending `{}`. `GET`. Is anything left?
4. Repeat 1–3 for an environment.

Record the result in this file and in `CLAUDE.md`.

### Design

If PATCH is confirmed: promote `buildMetadataPatch` out of `internal/services/vaults/` into a
shared package (`internal/metadata`, exported as `BuildPatch`), and wire all four resources
through it. Vaults keep their current behaviour; the helper moves, it does not change.

One thing to check before committing to that: the vault path sends the patch via
`params.SetExtraFields(map[string]any{"metadata": patch})`, because the typed
`map[string]string` field cannot carry per-key nulls. Whether `BetaAgentUpdateParams` and the
environment update params expose `SetExtraFields` is **unverified** — the module cache was
empty in the container this plan was written in. If they do not, fall back to
`param.Override` on the metadata field. Confirm before estimating.

If the probe shows replace semantics instead: agents are already correct, and the fix is
environments only — send an empty map rather than omitting the field, so that removing the
block clears the metadata.

Either way, environments has a bug.

### Tests

- Internal unit tests with an `httptest` server asserting the request body for: key added,
  key removed, all keys removed, block deleted.
- Acceptance: add a metadata key, apply, remove it, apply, assert it is gone from the API.

### Size

Small. A day including the probe, assuming `SetExtraFields` is available.

---

## 3. Read-after-write on vault credentials

### What happens today

`anthropic_vault` polls `Get` after a successful update until `updated_at` is no older than
the write, because the vaults endpoint serves stale reads for up to about a second
(`vault_resource.go:259`, consts at `229`–`230`, called at `330`). Left alone, Terraform's
post-apply refresh lands inside that window and the next plan shows a phantom in-place update.

`anthropic_vault_credential` has the same structure (`vault_credential_resource.go:575`) and
no wait. `CLAUDE.md` records that its endpoint was never probed and that nobody has observed
flakiness there — which is not evidence of absence, given the vault flake only surfaced under
a specific test.

### Probe B

Against `terraform-tests`, three trials:

1. Create a vault and a credential in it.
2. Update a mutable field on the credential (`display_name`, or `expires_at` on an
   `mcp_oauth` credential). Record the write timestamp.
3. `GET` the credential every 100ms for 3s. Record when the response reflects the write.
4. Separately: delete a credential and `GET` immediately, to check the delete window that bit
   the vault destroy check on 2026-09-02.

### Design

If a window exists: generalize the existing wait rather than copying it. Turn
`awaitVaultUpdateVisible` into a helper parameterized by a getter function and a terminal-error
predicate, and call it from the credential's `Update`. Keep the timeout and interval as
**arguments, not package-level vars** — the unit tests shrink them to milliseconds, and the
vaults acceptance tests exercise the real `Update` in the same binary, so a mutable global
would race the moment any vault test opts into `t.Parallel()`.

Preserve the two load-bearing details from the vault implementation: the comparison is
non-strict (`!UpdatedAt.Before(writtenAt)`, since an equal timestamp must count as visible),
and 401/403/404 bail out immediately instead of retrying to the deadline.

If no window exists: write the measurement into `CLAUDE.md`, close the open question, and
ship nothing.

### Tests

Unit tests against an `httptest` server that serves the stale object N times then the fresh
one, with millisecond timings. Mirror the existing vault tests.

### Size

Small. Half a day if the probe finds a window, an hour if it does not.

---

## 4. Organization pin

### Problem

`ANTHROPIC_ADMIN_API_KEY` is organization-wide and nothing in the provider checks which
organization it belongs to. A wrong key in CI applies to the wrong organization and the first
sign is the resources appearing somewhere unexpected. We provision per-PR preview stacks and
per-customer workspaces from this provider, so the blast radius is real.

The official beta requires an `organization_id` in every provider block and refuses to plan
when the credentials resolve elsewhere. The idea is right. Two details of their execution are
not worth copying: `organization_id = "unchecked"` puts a magic sentinel in an ID-typed field,
and the unchecked mode warns on every plan.

### Design

- New optional provider attribute `organization_id`, also readable from
  `ANTHROPIC_ORGANIZATION_ID`, resolved through the existing `resolveCredential` helper so an
  unknown value at plan time counts as unset.
- Unset means no check. That is the default and it stays the default — this is a guard you opt
  into, not a knob that changes what the provider manages.
- A `skip_organization_check` boolean for the escape hatch, instead of a sentinel value. No
  per-plan warning.
- Enforcement in `Configure`: when `organization_id` is set and an admin client exists, call
  `GET /v1/organizations/me` — the same endpoint the organization data source already uses
  (`organization_data_source.go:92`) — and fail when the IDs differ, with a message naming
  both the configured and the actual organization.

Note on the env var: `ANTHROPIC_ORGANIZATION_ID` is also one of the SDK's federation
variables. We pass `option.WithoutEnvironmentDefaults()`, so the SDK never reads it and there
is no conflict inside the provider. It names the same thing in both places, so honouring it is
the least surprising choice. Document the overlap on the provider page.

### Known gap

`/v1/organizations/me` is an Admin API endpoint and rejects a standard API key, so this check
cannot cover a config that only sets `api_key`. The official provider pins the organization for
any credential, which suggests a non-admin endpoint exists that returns org identity. Worth
fifteen minutes of looking before shipping; if one turns up, extend the check to the standard
client. Until then the provider page should say plainly that the pin covers admin and OAuth
credentials.

### Cost

One extra API call per `Configure`, which means per plan and per apply, only when the pin is
set. Acceptable.

### Tests

`httptest`-backed `Configure` tests: matching org passes, mismatched org fails with both IDs
in the message, unset makes no call, `skip_organization_check` makes no call.

### Docs

A section in `templates/index.md.tmpl` next to the credential documentation, then
`make generate`.

### Size

Small to medium. Mostly tests and documentation.

---

## Not in this plan

- **Reshaping schemas to match the official provider** — `model = { id }`, a single `tools`
  list, nested `auth = {}` on credentials, `/` instead of `:` in compound import IDs. We are
  not adopting their provider, so breaking every existing config to converge with it is cost
  with no payoff. If that ever changes, it is one deliberate major version, not a drip.
- **`archive_on_destroy` on agents** — the API only archives (`agent_resource.go` calls
  `Beta.Agents.Archive` unconditionally), so the toggle would have one reachable branch.
- **Memory stores and memories** — our largest feature gap against the official provider, but
  a whole new service. Schedule-driven, not bug-driven: worth doing when we or a customer
  actually use CMA memory.
- **Per-secret write-only version counters** — today one shared `token_wo_version` covers
  every secret on a credential. Additive and low urgency; only matters when rotating one
  secret of a multi-secret credential.
- **`anthropic_vault` / `anthropic_vaults` data sources** and **agent config validators**
  (cross-checking `mcp_toolsets[].mcp_server_name` against declared `mcp_servers`, and moving
  the unknown-built-in-tool failure from mid-apply to plan time). Both are real gaps, both are
  small, neither is a bug. File as issues.

## Open questions

1. Do `BetaAgentUpdateParams` and the environment update params expose `SetExtraFields`? Gates
   the shape of item 2.
2. Is there a non-admin endpoint that returns organization identity? Gates whether the item 4
   pin can cover standard API keys.
3. Does deleting a skill version break an agent pinned to it, or does the API refuse the
   delete? Changes how loudly item 1 needs to warn.
