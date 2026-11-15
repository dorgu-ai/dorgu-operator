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

> **Identity fields are claims until admission policies ship.** `proposer.username`, `source` and `spec.provenance` are not verified by the API server yet: anyone who can create a proposal can name any username, and anyone who can create a RemediationAction can write any provenance. The operator's admission policies (brief C2) are what make them an identity, and this feature is not released without them. Until then, do not use them to decide who may approve what.

## Spec

| Field | Required | Meaning |
|-------|----------|---------|
| `proposer.username` | yes | The Kubernetes username of the proposer. Dorgu's planner proposes as the operator ServiceAccount (`system:serviceaccount:<namespace>:<name>`). Until an admission policy requires it to equal the authenticated requester, it is a claim and not a proof. |
| `source` | no | `dorgu-planner` for Dorgu's planner, `agent` (the default) for anything else. `rollback` is reserved. The operator does not copy it: the action's `provenance.source` is `dorgu-planner` only when the operator decided the proposal together with its own planner's in-process draft, and `agent` for every other proposal, whatever this field says. |
| `agent{name, version, runID}` | no | Informational detail about the agent. |
| `idempotencyKey` | yes | A retried proposal with the same `proposer` and key in the same namespace is rejected as a duplicate and does not become a second action. |
| `incidentRef{name, namespace}` | no | The incident the proposal answers. Absent: the operator opens one. Present: it must exist in the proposal's namespace. |
| `target{kind, namespace, name}` | yes | The persona the plan changes. `target.namespace` must equal the proposal's namespace. |
| `steps[]` | yes in practice | The ordered plan, in the same typed schema as `RemediationAction.spec.steps`. The gate validates every step and can narrow or refuse the plan. A proposal with no steps is refused. At most 64 steps; per step `id` at most 128 characters, `description` 1024, `rationale` 4000, `command` 1024, at most 16 `safety` entries. |
| `rationale` | yes | Why the proposer wants this, at most 4000 characters. |
| `evidence[]` | no | References (`kind`, `name`, `namespace`) to what the proposer looked at. At most 20. |
| `rollbackOf{name}` | no | **Reserved.** The operator does not honour it yet: a proposal that sets it is rejected with `Intake/NotYetSupported`. |

The whole spec is immutable (`self == oldSelf`). To change a request, write a new proposal.

### The rationale is untrusted text

`rationale` and the step `description` and `rationale` are text from the proposer. The operator removes control and invisible characters from them (terminal escapes, bidirectional and zero-width characters, Unicode tag characters, variation selectors and every other format character) and stores the rest. `proposer.username` is never cleaned or cut: an identity is recorded exactly as it is or not at all, so a name with a control, format or invisible character, an empty one, or one over 253 characters is rejected with `Intake/ProposerInvalid`. The rationale is copied into the converted action's `spec.explanation`, so it reaches the terminal of whoever approves the action: show it as machine-written text, and expect it to say anything.

No decision reads it. No guardrail, gate input or branch in the operator looks at it, so text that claims pre-approval or asks for a guardrail to be skipped changes nothing. It is also kept out of Dorgu's own planner: past actions' explanations are put in the model's prompt only when the action's provenance is the operator's own planner, never for an agent's action.

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
| `Intake` / `DuplicateIdempotencyKey` | An earlier proposal from the same proposer in the same namespace holds the key: it is `Received` or `Converted`. The earlier one is the one kept. A `Rejected` proposal does not hold its key, so after a `RateLimited`, `IncidentNotFound` or gate refusal the proposer can fix the request and retry with the same key. |
| `Intake` / `RateLimited` | The proposer already had 30 proposals received in the hour before this one. Dorgu's own planner is not counted: it is exempt when the operator decides its proposal together with its in-process draft, which a username cannot fake. The gate's per-persona limits still apply to it. |
| `Intake` / `IncidentNotFound` | `incidentRef` names an incident that does not exist in the proposal's namespace. |
| `Intake` / `IncidentTargetMismatch` | `incidentRef` names an incident detected on a different persona than `target`. An incident unlocks changes to its own persona and no other. |
| `Intake` / `IncidentResolved` | `incidentRef` names an incident that is already `Resolved`. Dorgu's own planner never proposes against one. |
| `Intake` / `ActionInvalid` | The API server refused the `RemediationAction` this proposal would become as invalid. Retrying cannot help, so it is final. The condition message carries the server's reason, cleaned and cut to 512 characters. It is also the answer for an action this intake made earlier that lost its provenance, could not be deleted and has since been given a phase: the message names it, and it needs deleting by hand. |
| `Intake` / `NotYetSupported` | `rollbackOf` is set. |
| `Intake` / `ProposerInvalid` | `proposer.username` is empty, longer than 253 characters, or has a control, format or invisible character. The admission policy refuses the same names before they are stored. |
| `Intake` / `TargetNamespaceMismatch` | The proposal's namespace is not `target.namespace`. A CRD validation rule cannot read `metadata.namespace`, so the operator enforces this. |

Rejected proposals are kept for 7 days and then pruned. A converted proposal is owned by its action, so deleting the action prunes the proposal.

### What the rate limit and the duplicate check count

Both count proposals that exist in the API server, not a counter in the operator's memory, so an operator restart does not reset them. They reset when the objects go: a proposer who can delete proposals can create one, wait for the decision, delete it and start again, and a converted proposal disappears with its action. For that reason the agent-proposer role (brief C2) grants `create`, `get`, `list` and `watch` on proposals and **no `delete`**. Until the admission policies verify `proposer.username`, varying it also gives a proposer a fresh allowance.

Proposals are ordered by creation time (whole seconds), then by name. Of two identical proposals created within one second the one whose name sorts first wins, which may be the later arrival. Exactly one of them converts, whichever the reconciler sees first. The rate-limit count includes duplicates and rate-limited proposals themselves, so a proposer that keeps sending while limited stays limited.

## What the gate needs from an incident

A persona change (`persona-update`) is admitted only when the rules recommend a resource adjustment for the incident. For a proposal the operator derives that from the incident's recorded signal: out-of-memory kills, memory and CPU saturation, CPU throttling, and a crash loop correlated with an out-of-memory signal. A proposal that names no incident gets an operator-opened incident with the signal `ProposerReported`, which the rules do not map to a resource change, so such a proposal can carry advisory steps and a persona change is refused at `Validate/action-class`. To propose a persona change, name an incident Dorgu detected on the persona you are targeting.

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

It is immutable: it cannot be added to an action that has none and cannot be changed once set. An action created before v0.14 has no provenance and is treated as proposed by the operator ServiceAccount. `source` is `dorgu-planner` only for a proposal the operator decided together with its own planner's in-process draft. If the operator crashes after its planner stored a proposal and before it was converted, the reconciler converts it later without the draft, and the action is recorded with `source: agent`; it is valid and goes through the gate like any other.

## When the intake is closed

The operator keeps the intake closed, and says so in the `ProposalIntakeReady` condition of the ClusterPersona and in its log, while:

| Reason | Cause | Fix |
|--------|-------|-----|
| `OperatorIdentityMismatch` | `OPERATOR_SERVICE_ACCOUNT` and `POD_NAMESPACE` are well formed, but the API server (a SelfSubjectReview) authenticates the operator as someone else. | Run the operator as the ServiceAccount it names, or fix the two variables. |
| `OperatorIdentityUnverified` | The operator could not ask the API server who it is. | Let the operator create `selfsubjectreviews` (`authentication.k8s.io`); every authenticated user can by default. |
| `OperatorIdentityUnknown` | `OPERATOR_SERVICE_ACCOUNT` (or `POD_NAMESPACE`) is missing or is not a valid name. The operator never invents a username. | Set both from the downward API, as the chart does. |
| `ProposalCRDMissing` | The `RemediationProposal` CRD is not installed or not established. | Apply `dorgu.io_remediationproposals.yaml`. |
| `ActionProvenanceMissing` | The installed `RemediationAction` CRD has no `spec.provenance`, so the API server would drop the provenance of every action. | Apply `dorgu.io_remediationactions.yaml`. |

While it is closed nothing is converted (proposals wait, untouched), Dorgu's planner does not plan or propose and spends no model call, and detection, diagnosis and the lifecycle of existing actions go on. The checks run at startup and every minute; the operator does not restart to recover. Apply both CRDs before upgrading the operator.

## Permissions the operator needs

`remediationproposals` get, list, watch, create, update, patch and delete (delete prunes rejected proposals), `remediationproposals/status` get, update and patch, plus the create it already had on `remediationactions` and `incidentmemories`, and `get` on exactly two CustomResourceDefinitions, `remediationactions.dorgu.io` and `remediationproposals.dorgu.io`, for the check above. The chart sets `OPERATOR_SERVICE_ACCOUNT` from the downward API so the planner knows its own username.
