# RemediationProposal

`RemediationProposal` (`dorguv1alpha1`, group `dorgu.io`, namespaced, short name `rprop`) is the one way a change request enters Dorgu. Dorgu's own planner writes one for every plan it makes, and any other proposer, such as an external agent, writes the same object. Only the operator's proposal reconciler turns an accepted proposal into a `RemediationAction`, and a human still approves that action.

```
proposer ──create──► RemediationProposal (spec is immutable)
                        │
                        ▼
              intake: dedupe, rate limit, incident, gate
                        │
          ┌─────────────┴──────────────┐
          ▼                            ▼
 RemediationAction (Pending)    proposal.status = Rejected{stage, reason}
 spec.provenance = this proposal
```

Nothing about a proposal makes it approved. A converted action starts `Pending` like every other.

## Spec

| Field | Required | Meaning |
|-------|----------|---------|
| `proposer.username` | yes | The Kubernetes username of the proposer. Dorgu's planner proposes as the operator ServiceAccount (`system:serviceaccount:<namespace>:<name>`). Until an admission policy requires it to equal the authenticated requester, it is a claim and not a proof. |
| `source` | no | `dorgu-planner` for Dorgu's planner, `agent` (the default) for anything else. `rollback` is reserved. Recorded and reported, never read by a decision. |
| `agent{name, version, runID}` | no | Informational detail about the agent. |
| `idempotencyKey` | yes | A retried proposal with the same `proposer` and key in the same namespace is rejected as a duplicate and does not become a second action. |
| `incidentRef{name, namespace}` | no | The incident the proposal answers. Absent: the operator opens one. Present: it must exist in the proposal's namespace. |
| `target{kind, namespace, name}` | yes | The persona the plan changes. `target.namespace` must equal the proposal's namespace. |
| `steps[]` | yes in practice | The ordered plan, in the same typed schema as `RemediationAction.spec.steps`. The gate validates every step and can narrow or refuse the plan. A proposal with no steps is refused. At most 64. |
| `rationale` | yes | Why the proposer wants this, at most 4000 characters. |
| `evidence[]` | no | References (`kind`, `name`, `namespace`) to what the proposer looked at. At most 20. |
| `rollbackOf{name}` | no | **Reserved.** The operator does not honour it yet: a proposal that sets it is rejected with `Intake/NotYetSupported`. |

The whole spec is immutable (`self == oldSelf`). To change a request, write a new proposal.

### The rationale is untrusted text

`rationale` is shown to people, as the converted action's `spec.explanation`, and nothing else reads it. No guardrail, gate input or branch in the operator looks at it, so text in it that claims pre-approval or asks for a guardrail to be skipped changes nothing. Show it as machine-written text.

## Status

| Field | Meaning |
|-------|---------|
| `phase` | `Received` (seen, not yet decided), `Converted` or `Rejected`. |
| `rejection{stage, reason}` | Where and why a proposal was refused. |
| `remediationActionRef{name}` | The action the proposal became. |
| `incidentRef` | The incident the proposal was answered against, named or opened. |
| `observedGeneration` | The generation the operator decided. |
| `conditions` | `Converted`: `True` on success, `False` on rejection, with the human-readable message. |

### Rejections

`stage` is `Intake` for a refusal before the gate, or the gate stage that refused (`Validate`, `Ground`, `Ownership`, `Guardrails`, `ModePolicy`, `DedupCooldown`). At a gate stage `reason` is the gate's guardrail code, for example `deny-list` or `blast-radius`.

| Stage / reason | When |
|----------------|------|
| `Intake` / `DuplicateIdempotencyKey` | An earlier proposal from the same proposer in the same namespace used the key. The earlier one is the one kept. |
| `Intake` / `RateLimited` | The proposer already had 30 proposals received in the hour before this one. Dorgu's own planner counts as one proposer. |
| `Intake` / `IncidentNotFound` | `incidentRef` names an incident that does not exist in the proposal's namespace. |
| `Intake` / `NotYetSupported` | `rollbackOf` is set. |
| `Intake` / `TargetNamespaceMismatch` | The proposal's namespace is not `target.namespace`. A CRD validation rule cannot read `metadata.namespace`, so the operator enforces this. |

Rejected proposals are kept for 7 days and then pruned. A converted proposal is owned by its action, so deleting the action prunes the proposal.

## What the gate needs from an incident

A persona change (`persona-update`) is admitted only when the rules recommend a resource adjustment for the incident. For a proposal the operator derives that from the incident's recorded signal: out-of-memory kills, memory and CPU saturation, and a crash loop correlated with an out-of-memory signal. A proposal that names no incident gets an operator-opened incident with the signal `ProposerReported`, which the rules do not map to a resource change, so such a proposal can carry advisory steps and a persona change is refused at `Validate/action-class`. To propose a persona change, name an incident Dorgu detected.

## RemediationAction.spec.provenance

The operator writes `spec.provenance` when it converts a proposal:

```yaml
provenance:
  proposal: ra-web-resource-adjustment-1a2b3c4d5e6f
  proposer: {username: system:serviceaccount:dorgu-system:dorgu-operator}
  source: dorgu-planner
  requestedBy: ""        # reserved for ticketed rollback
  agent: {name: dorgu-operator}
```

It is immutable: it cannot be added to an action that has none and cannot be changed once set. An action created before v0.14 has no provenance and is treated as proposed by the operator ServiceAccount.

## Permissions the operator needs

`remediationproposals` get, list, watch, create, update, patch and delete (delete prunes rejected proposals), `remediationproposals/status` get, update and patch, plus the create it already had on `remediationactions` and `incidentmemories`. The chart sets `OPERATOR_SERVICE_ACCOUNT` from the downward API so the planner knows its own username.
