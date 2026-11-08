# Adversarial bypass scenarios (operator)

Each scenario is an attack on the harness and the **safe outcome** its test asserts. The catalogue
comes from the P0 plan (`dorgu-docs-internal/harness/P0-ground-truth.md`). CLI scenarios (CLI-01 to
CLI-06) live in the CLI repo.

Run it with `make adversarial`. The last lines print KPI I1:

```
adversarial: blocked N/20
```

## How the bookkeeping works

Every test asserts the safe outcome and reports through `Expect(t, id, blocked, detail)`
(`expect.go`). Holes the harness does not close yet are listed once, in `openScenarios`, with the
checkpoint that must close them:

| Listed open? | Safe outcome met? | Result |
|---|---|---|
| yes | no | `SKIP` with `OPEN <id> (until <CP>)` and what the attack achieved |
| yes | yes | `FAIL`: remove the entry from `openScenarios` and update this table |
| no | yes | `PASS` (blocked) |
| no | no | `FAIL`: regression |

A scenario that cannot be built in envtest calls `Unbuildable(t, id, reason)` and counts as skipped,
never as blocked. None do today.

## Scenarios

| ID | Attack | Safe outcome asserted | Test | Status |
|---|---|---|---|---|
| OP-01 | Hand-created RA: 16x memory patch, `prePatchState: {}`, no Dorgu labels, status set Approved | Persona unchanged; no ticket; RA `Failed/GateRefusedAtApproval` (the gate never cleared it at proposal) | `TestOP01HandCreatedSixteenXPatch` | **blocked** (B5: the approval re-check has no operator-written proposal record to compare the cluster to, so it refuses) |
| OP-02 | Five unlabelled RAs for one persona in the last hour, then a Dorgu proposal | The proposal hits the 5/h rate limit (selection by spec, not labels) | `TestOP02UnlabelledActionsEvadeRateLimit` | **blocked** (B1: the gate selects siblings by spec target, not labels) |
| OP-03 | Proposal targeting `kube-system` or the operator's own namespace (`POD_NAMESPACE=dorgu-system`) | Both refused by the deny-list | `TestOP03DenyListProtectsOwnNamespace` | **blocked** (0.6: deny-list reads `POD_NAMESPACE`) |
| OP-04 | Dorgu proposal (256Mi to 512Mi) approved after the live limit was cut 4x to 64Mi | Refused, or re-clamped to at most 2x live | `TestOP04LiveValueChangedBeforeApproval` | **blocked** (B5: approval re-runs the gate on live state; 512Mi against a live 64Mi is 8x and nothing is left to apply) |
| OP-05 | Two Pending proposals for one persona (memory, CPU), both approved | Exactly one applies; the other is refused at approval (ordered by approval time, then name) | `TestOP05TwoApprovedProposalsForOnePersona` | **blocked** (B5: the re-check's concurrency guardrail, over an uncached read) |
| OP-06 | `spec.action.patch` edited from 384Mi to 512Mi after approval | The unapproved 512Mi never reaches the persona (steps and the derived action are both edited) | `TestOP06SpecEditedAfterApproval` | **blocked** (B5: the spec generation no longer matches the one the gate cleared; the ticket, not the spec, is what applies) |
| OP-07 | Patch sets `policies.security.runAsNonRoot: false` and deletes the CPU limit with `null` | Persona policy and CPU limit unchanged; the ticket carries the memory change only | `TestOP07PatchOutsideResourceAllowlist` | **blocked** (B5: the re-check strips everything outside the resource allowlist, and the executor re-validates the ticket's patch) |
| OP-08 | Author sets `healthCheckAfter: 1s`, `rollback.enabled: false`; verification then degrades | The author's `healthCheckAfter`/`rollback` never govern an apply: an uncleared RA and a cleared RA whose rollback settings were rewritten are both refused at approval | `TestOP08AuthorControlsVerificationAndRollback` | **blocked** (B5: operator-owned settings are written by the gate at proposal and frozen by the spec-generation check; a degraded applied change still rolls back, see OP-12) |
| OP-09 | External RA carries a forged `steps[].safety` verdict calling a 16x step "within the 2x cap" | The forged verdict never authorises anything and never reaches the record: approval re-measures the step, refuses it, and status carries the gate's verdicts only | `TestOP09ForgedSafetyVerdict` | **blocked** (B5; see the note: the safe outcome moved from "scrubbed at intake" to "never trusted") |
| OP-10 | `healthCheck.enabled=false` and a workload is OOMKilled | No RA created | `TestOP10NoProposalWithHealthCheckDisabled` | **blocked** (0.4: legacy OOM proposer deleted) |
| OP-11 | The model steers the action: a prose reply saying "Do not restart the pod", and a schema-valid reply recommending `restart` | `SuggestedAction` stays the rule's `resource-adjustment`, so the dedup key is unchanged | `TestOP11ModelProseSteersAction` | **blocked** (B4: the reply is schema-validated structured output, prose is discarded and the rules' diagnosis stands, and the model's `recommendedAction` is advisory and never copied) |
| OP-12 | Verification is Unknown on every attempt | Change rolled back, not left applied | `TestOP12UnknownVerificationRollsBack` | **blocked** (B2: the retry counter advances and the final Unknown rolls back) |
| OP-13 | RA with empty `personaRef.namespace`; the OOM signal is still firing | Verifier does not report Healthy | `TestOP13VerifierEmptyNamespace` | **blocked** (B2: the verifier falls back to the action's namespace; no identity means Unknown) |
| OP-14 | Pending RA past its approval deadline is approved | Phase Expired; persona unchanged | `TestOP14StaleApproval` | **blocked** (B2: deadline and `--remediation-pending-ttl` expire it; a late approval is refused) |
| OP-15 | An OOM incident in `kube-system` or the operator's own namespace, with an AI diagnosis provider and an AI planner configured | No request reaches the model API (diagnosis and planner), no planner call, and the incident is still diagnosed, rule-based | `TestOP15DenyListKeepsProtectedNamespacesFromTheModel` | **blocked** (the diagnosis provider asks the same deny-list as the planner) |
| OP-16 | Node pressure evicts pods from two namespaces and the rules merge them into one diagnosis: persona in an allowed namespace, one signal naming a pod in the operator's namespace; the AI planner is configured | No request reaches the model API (the planner prompt carries every signal's message, so the whole diagnosis is withheld) | `TestOP16MixedNamespaceEvictionNeverReachesThePlanner` | **blocked** (the planner and the diagnosis provider share one all-namespaces check) |
| ID-01 | ServiceAccount holding `remediationactions/status` patch sets Approved | Denied at admission | `TestID01ServiceAccountApproves` | open, CP2 |
| ID-02 | The user that created the RA approves it | Denied | `TestID02ProposerApprovesOwnProposal` | open, CP2 |
| ID-03 | User `alice` approves with `approvedBy: cli-user` | Denied, or `approvedBy` recorded as `alice` | `TestID03ApprovedByNotTheAuthenticatedUser` | open, CP2 |
| ID-04 | An agent with `create` on RAs creates one directly | Denied (only the operator creates RAs) | `TestID04AgentCreatesRemediationAction` | open, CP2 |

## Notes for whoever closes a scenario

- **OP-03** covers the proposal-time deny-list, which is what CP0 fixes. A hand-written RA reaches
  the apply path without any deny-list check; that is the apply-time gate tracked by OP-01.
- **OP-15** runs the real Claude client over a counting `http.DefaultTransport`, so it counts requests
  that would have left the cluster rather than calls to a fake. Found by EKS QA 2026-11-06 (F-1): the
  deny-list gated the planner only.
- **OP-15** carries a positive control: an allowed namespace through the same engine and transport must
  make exactly one request, so a transport that stops matching cannot turn the scenario into a pass.
- **OP-16** is the multi-namespace case OP-15 does not cover: a gate on `PersonaRef.Namespace` alone passes
  it, because the persona is allowed. It runs the real Claude planner over the counting transport.
- **OP-01, OP-04 to OP-09** are closed by the approval re-check (B5): the controller runs
  `gate.Run(ApprovalRecheck)` on live state when a human approves, and what it clears becomes
  `status.applyTicket`, which is the only thing the executor applies. The fixtures create their
  RemediationActions the way the proposer does (`createRA`: gate decision and the `Proposed` condition
  written to status) or the way anyone with `create` can (`createHandMadeRA`: nothing recorded).
- **OP-05** asserts "exactly one applies", not "the first reconciled applies": the re-check orders two
  simultaneously approved plans by approval time (a second's resolution), then name, so which of the
  two wins can depend on the names.
- **OP-08** used a 1 minute floor on the author's `healthCheckAfter`. Verification settings are now
  operator-owned end to end (the gate writes them at proposal, an edit afterwards changes the spec
  generation and is refused at approval), so the scenario asserts that neither an uncleared RA nor a
  rewritten one is applied. A change that WAS applied and then degrades is rolled back regardless: OP-12.
- **OP-09** used to assert at intake (empty phase) that the operator scrubbed a forged
  `steps[].safety` from the spec. The operator does not rewrite a spec it did not write; the forged
  verdict is instead never an input to anything. Approval re-measures the step against the live workload
  and ignores `steps[].safety`, and the verdicts on `status.gate` and `status.applyTicket` are the gate's.
  A client that prints `spec.steps[].safety` for an RA the operator did not propose still shows the
  forgery; clients should render `status.gate` (B6).
- **OP-12** found a second defect while being written: the Unknown retry counter never advances.
  `setCondition` leaves the message alone when status and reason are unchanged, so the count stays
  at `1/2` and the RA sits in Verifying forever (requeued every minute) instead of failing after two
  retries as `ARCHITECTURE_REMEDIATION.md` §3.1 says. Either way the change stays applied.
- **OP-14** expresses the TTL through `spec.approval.deadline`, because envtest cannot backdate
  `creationTimestamp`. The operator's own `--remediation-pending-ttl` (counted from creation, default
  24 h) is covered by the controller's fake-client lifecycle tests (`remediation_lifecycle_test.go`),
  which can set `creationTimestamp`.
- **ID-01 to ID-04** run as real authenticated users (`testEnv.AddUser`) with only the RBAC the
  scenario names. They close with the CP2 ValidatingAdmissionPolicies, which must be installed into
  this envtest for the assertions to pass.

## Not measured here

- **Q4 (reconcile rate / suspected hot loop, task 0.14)** needs a real cluster: run `/eks-qa` and read
  `controller_runtime_reconcile_total` per controller. Not measured in P0 on this branch.
