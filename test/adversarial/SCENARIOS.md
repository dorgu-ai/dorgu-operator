# Adversarial bypass scenarios (operator)

Each scenario is an attack on the harness and the **safe outcome** its test asserts. The catalogue
comes from the P0 plan (`dorgu-docs-internal/harness/P0-ground-truth.md`). CLI scenarios (CLI-01 to
CLI-06) live in the CLI repo.

Run it with `make adversarial`. The last lines print KPI I1:

```
adversarial: blocked N/18
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
| OP-01 | Hand-created RA: 16x memory patch, `prePatchState: {}`, no Dorgu labels, status set Approved | Persona unchanged; RA not Applying | `TestOP01HandCreatedSixteenXPatch` | open, CP1 |
| OP-02 | Five unlabelled RAs for one persona in the last hour, then a Dorgu proposal | The proposal hits the 5/h rate limit (selection by spec, not labels) | `TestOP02UnlabelledActionsEvadeRateLimit` | open, CP1 |
| OP-03 | Proposal targeting `kube-system` or the operator's own namespace (`POD_NAMESPACE=dorgu-system`) | Both refused by the deny-list | `TestOP03DenyListProtectsOwnNamespace` | open, CP0 (0.6) |
| OP-04 | Dorgu proposal (256Mi to 512Mi) approved after the live limit was cut 4x to 64Mi | Refused, or re-clamped to at most 2x live | `TestOP04LiveValueChangedBeforeApproval` | open, CP1 |
| OP-05 | Two Pending proposals for one persona (memory, CPU), both approved | The second does not apply | `TestOP05TwoApprovedProposalsForOnePersona` | open, CP1 |
| OP-06 | `spec.action.patch` edited from 384Mi to 512Mi after approval | The unapproved 512Mi never reaches the persona | `TestOP06SpecEditedAfterApproval` | open, CP1 |
| OP-07 | Patch sets `policies.security.runAsNonRoot: false` and deletes the CPU limit with `null` | Persona policy and CPU limit unchanged | `TestOP07PatchOutsideResourceAllowlist` | open, CP1 |
| OP-08 | Author sets `healthCheckAfter: 1s`, `rollback.enabled: false`; verification then degrades | Verification window at least 1m; degraded change rolled back anyway | `TestOP08AuthorControlsVerificationAndRollback` | open, CP1 |
| OP-09 | External RA carries a forged `steps[].safety` verdict calling a 16x step "within the 2x cap" | Operator recomputes verdicts at intake; the forged entry is gone | `TestOP09ForgedSafetyVerdict` | open, CP1 |
| OP-10 | `healthCheck.enabled=false` and a workload is OOMKilled | No RA created | `TestOP10NoProposalWithHealthCheckDisabled` | open, CP0 (0.4) |
| OP-11 | Model reply says "Do not restart the pod" | `SuggestedAction` stays the rule's `resource-adjustment`, so the dedup key is unchanged | `TestOP11ModelProseSteersAction` | open, CP1 |
| OP-12 | Verification is Unknown on every attempt | Change rolled back, not left applied | `TestOP12UnknownVerificationRollsBack` | open, CP1 |
| OP-13 | RA with empty `personaRef.namespace`; the OOM signal is still firing | Verifier does not report Healthy | `TestOP13VerifierEmptyNamespace` | open, CP1 |
| OP-14 | Pending RA past its approval deadline is approved | Phase Expired; persona unchanged | `TestOP14StaleApproval` | open, CP1 |
| ID-01 | ServiceAccount holding `remediationactions/status` patch sets Approved | Denied at admission | `TestID01ServiceAccountApproves` | open, CP2 |
| ID-02 | The user that created the RA approves it | Denied | `TestID02ProposerApprovesOwnProposal` | open, CP2 |
| ID-03 | User `alice` approves with `approvedBy: cli-user` | Denied, or `approvedBy` recorded as `alice` | `TestID03ApprovedByNotTheAuthenticatedUser` | open, CP2 |
| ID-04 | An agent with `create` on RAs creates one directly | Denied (only the operator creates RAs) | `TestID04AgentCreatesRemediationAction` | open, CP2 |

## Notes for whoever closes a scenario

- **OP-03** covers the proposal-time deny-list, which is what CP0 fixes. A hand-written RA reaches
  the apply path without any deny-list check; that is the apply-time gate tracked by OP-01.
- **OP-08** uses a 1 minute floor. The operator default is 10 minutes; P1 owns the exact bound.
- **OP-09** asserts at intake (empty phase), because the CLI shows verdicts on Pending RAs before
  anyone approves.
- **OP-12** found a second defect while being written: the Unknown retry counter never advances.
  `setCondition` leaves the message alone when status and reason are unchanged, so the count stays
  at `1/2` and the RA sits in Verifying forever (requeued every minute) instead of failing after two
  retries as `ARCHITECTURE_REMEDIATION.md` §3.1 says. Either way the change stays applied.
- **OP-14** expresses the TTL through `spec.approval.deadline`, because envtest cannot backdate
  `creationTimestamp`. If P1 derives the TTL from creation time instead, the controller needs a
  clock seam and this test needs to use it.
- **ID-01 to ID-04** run as real authenticated users (`testEnv.AddUser`) with only the RBAC the
  scenario names. They close with the CP2 ValidatingAdmissionPolicies, which must be installed into
  this envtest for the assertions to pass.

## Not measured here

- **Q4 (reconcile rate / suspected hot loop, task 0.14)** needs a real cluster: run `/eks-qa` and read
  `controller_runtime_reconcile_total` per controller. Not measured in P0 on this branch.
