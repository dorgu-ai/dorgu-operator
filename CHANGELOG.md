# Changelog

All notable changes to the Dorgu Operator are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

- **Resource saturation no longer counts pods no node has accepted.** `ClusterPersona.status.resourceSummary.cpuUtilization` read **1689%** on a cluster where 25% was requested and 1% was in use. `setClaimedResources` skipped terminal pods and nothing else, so pods sitting in the scheduling queue had their requests summed against node allocatable. That is not an over-estimate to be tightened: an unscheduled pod holds no allocation on any node, and because such a pod can request more than the cluster owns, the error has no upper bound.

  Only pods a node has actually accepted are counted now. `podHoldsAllocation` makes both exclusions explicit and says why they differ: a Succeeded or Failed pod ran to completion and released its allocation, while a pod with an empty `spec.nodeName` never had one, whether nothing can fit it, it is waiting on a node that has not joined, or it is gated.

  ```
  before: usedCPU 63965m / allocatable 3860m  ->  cpuUtilization 1657%
  after:  usedCPU   965m / allocatable 3860m  ->  cpuUtilization   25%
  ```

  A Pending pod that *has* been bound to a node still counts, because a reservation held while an image pulls is a real reservation. This field feeds the dashboard's cluster view, where a wrong number reads as authoritative in a way terminal output does not; the CLI stopped depending on it in the same fix set (the paired CLI release computes saturation itself from nodes and pods), so the two are corrected from both ends.

  Worth noting: the CRD's own description of `usedCPU` already said "the CPU claimed by the resource requests of **scheduled** pods". The schema was right and the code did not match it.
### Upgrade notes

> **With `aiRemediation.enabled=true`, remediations become appliable again. Before this change they were not, and following the quickstart verbatim could not heal anything.** Clean-room run #4 measured it: 9 AI-planned remediations, 0 that could change a workload. The planner described the memory fix as `workload-apply` steps, which the CRD's CEL rule forbids from ever being `autoExecutable`, and gave the one `persona-update` step no patch at all. `spec.action.type` therefore came out `notification`, `dorgu remediation approve` printed "No resource change to apply", and the pod was still in CrashLoopBackOff 42 minutes later. Turning the planner off healed the identical app on the first try.
>
> **A number the model asked for may now be replaced by one Dorgu computed.** When a plan's persona-update step carries no usable patch, or carries one the blast-radius guardrail refuses, Dorgu fills in the value from the same deterministic calculation the rule-based path uses. The plan keeps the model's root cause; the number that reaches your cluster is the rule engine's. Every substitution is recorded on the step, so nothing about it has to be inferred from prose.
>
> **New optional CRD field: `spec.steps[].safety`.** A list of guardrail verdicts, each naming the field, what was requested, what is permitted, the ratio, the ceiling, and a ready-to-print message. Existing objects need no migration: the field is optional and absent means no guardrail ruled. CRD regenerated in `config/crd/bases` and mirrored into `charts/dorgu-operator/crds`. A CLI that does not know the field yet renders exactly as it does today.

### Fixed

- **An AI plan that diagnoses a resource change now always produces a `persona-update` step with a populated patch, or is not written at all.** Three separate holes fed the same outcome. The prompt let a container resource change be expressed as `workload-apply`, which is advisory by construction. `patch` was not required on a `persona-update` step, so the model could and did omit it. And nothing checked, before persisting, that the plan could do the thing it said it would. The fix is in that last place, because the first two are the model's behaviour and cannot be relied on: after the guardrails run, a plan for a diagnosed resource change either carries an appliable patch or Dorgu supplies one from `calculateResourceChange`, the rule engine's own conservative increase measured against the live workload. `assertAppliableWhenResourceDiagnosed` re-checks the invariant against the object immediately before the `Create`, so it holds regardless of what any future change does to the steps in between. A plan Dorgu cannot make appliable is refused and the rule-based proposal takes over, which records `planSource: rule-based` and is therefore auditable as what it is.
- **Dorgu fills in a step the plan already has; it does not invent one.** A plan containing no `persona-update` step did not propose a persona change at all, and grafting Dorgu's step into an AI-labelled plan would produce a plan no model wrote. That case falls back to rules wholesale.
- **The one exemption, and it is deliberate: when Dorgu's own rule engine declines to size a change, the advisory plan stands.** The live container setting no CPU limit means raising one would introduce a field the workload has never had (F-05), so the rule-based path would produce nothing either. An advisory plan that says exactly that is worth more than no plan, and it is not the defect above.
- **A `persona-update` step carrying no patch is now removed from the plan rather than displayed.** It applies nothing and it instructs nobody, since "update the ApplicationPersona" is Dorgu's own job, and it is what rendered as `(no changes)` under a plan that read like a fix. A step whose patch a guardrail emptied is kept, because it carries a record of which field was refused and why: that is the difference between a step that explains an absence and a step that is one.
- **A patch returned as a bare JSON object instead of a JSON-encoded string no longer throws the whole plan away.** The tool schema asks for a string, and decoding an object into that field failed the entire tool input, not the one field, so the plan was retried once, failed identically, and degraded to rules with the model's diagnosis discarded. Both shapes are now accepted, and an unusable value leaves the patch empty rather than failing the step around it.
- **The blast-radius verdict is structured data, not a prefix on the model's prose.** It used to be spliced onto the front of `rationale` as `[safety:blast-radius] …`, so a computed refusal read as part of the model's reasoning, one line below the model's own claim that the same 16x change was "well within a 2x ceiling". The verdict now lives in `spec.steps[].safety`, and Dorgu writes the step's description and rationale on any step a guardrail has touched. The model is no longer in a position to characterise a verdict that was never its to give, and the prompt now forbids it from commenting on caps at all.
- **A refused field is removed from the step's patch, so nothing can advertise a change that will not happen.** Previously the patch kept the refused value while the step was demoted to advisory, and the diff block went on offering `8Mi -> 128Mi` to a reader who would have got nothing. Where Dorgu can substitute a permitted value the patch carries that instead, and the diff shows what will actually happen.
- **The owner instruction is built after the guardrails, not before.** It quotes concrete values out of the plan's patches, so building it first told a Helm user to put the refused value in their values file. This was the same defect as the one above, one screen further down, and was caught by moving the code.
- **`checkBlastRadius` reports every field that breaches the cap, in a stable order.** It returned the first breach it happened across while ranging over a map, so a plan moving two fields too far had one named at random and the other silently ignored.

### Added

- **`spec.steps[].safety`, a list of guardrail verdicts on a step.** Each entry carries `rule` (`blast-radius`, `plan-validation` or `absent-field`), `verdict` (`clamped`, `rejected` or `derived`), the `field`, the `baseline` it was measured against, what was `requested`, what is `permitted`, the `ratio` and `maxRatio`, and a Dorgu-authored `message`. Every string in it comes from Dorgu's own arithmetic, so a client can render it, filter on it or alert on it without parsing a sentence a model wrote.
- **F-05's "Dorgu will not introduce a field the workload lacks" is recorded structurally too,** as an `absent-field` entry beside the prose it already produced.
- **A guard test asserting the invariant across six adversarial plan shapes,** including the exact three-step plan the clean room recorded, and covering the case that must stay quiet: a diagnosis whose fix is not a resource change may be answered with advice alone.

### Known limitation

- **The 2x cap still anchors to the live value, so an app that has drifted far below its recorded intent cannot be restored in one step.** After a drift down to 8Mi the largest permitted step is 16Mi, which may still be too little. Dorgu now says so in the step's verdict and in the plan summary rather than implying the fix is complete, but the anchor itself is unchanged: moving it to the persona's recorded intent would weaken the guardrail in exactly the case where the persona is the thing that is stale.

## [0.10.0] - 2026-08-25

### Upgrade notes

> **1. Incidents now stay open roughly 6 minutes longer in the happy case. This is deliberate: silence is not recovery.** Resolution used to need two absences, no matching signal this cycle and 5 minutes since the last one. A pod deep in CrashLoopBackOff emits nothing while it backs off, which is precisely when an incident used to close. Resolution now additionally requires an observed stable window: pods present, Ready, and continuously Ready for `RecoveryStabilityWindow` (**6 minutes**, chosen to exceed the 5-minute maximum CrashLoopBackOff interval), with no container restart or return to a waiting state inside it. The added latency is the price of not calling a dead pod healthy. There is no flag to opt out.
>
> **2. AI model calls now scale with the number of simultaneously broken applications.** Diagnosis used to run once per signal type across the whole cluster, which was the defect rather than an optimisation: four broken apps produced one diagnosis and therefore one incident. It now runs once per broken application per cycle, so four broken apps cost four diagnoses per cycle where they previously cost one. **Worth knowing if you supply your own key.** This is inherent to the fix, because a per-application diagnosis cannot come out of a single cluster-wide call.
>
> **3. The manager ClusterRole gains exactly one verb: `delete` on `dorguevents.dorgu.io`.** Nothing else widened. The grant is scoped to Dorgu's own CRD and to no workload kind, so the published claim that the operator never writes your workloads is unchanged. A test now asserts the operator still cannot `create`, `update`, `patch` or `delete` Deployments, ReplicaSets, Pods, Nodes or Namespaces. Check it on a live cluster rather than taking it on trust:
>
> ```
> kubectl auth can-i delete dorguevents.dorgu.io --as=system:serviceaccount:dorgu-system:dorgu-operator -A   # yes
> kubectl auth can-i delete deployments          --as=system:serviceaccount:dorgu-system:dorgu-operator -A   # no
> ```
>
> **Existing `IncidentMemory` objects need no migration.** `spec.attribution` is a new optional enum, and an empty value reads as persona-attributed, which is what every pre-upgrade incident was. The field and its label are backfilled on first touch, so no duplicate incidents are raised across the upgrade. The CLI needs no change either: an unattributed incident's `personaRef.name` is the workload name and renders normally.

### Changed

- **An incident is resolved only on positive evidence of recovery, never on the absence of a signal.** `resolveCleared` used to need two absences: no matching signal this cycle, and 5 minutes since the last one. Silence now only opens the question, and `verifyRecovery` has to answer it with an observation. Pods must exist, be Ready, have been Ready for the 6-minute stability window, and have no container that restarted or re-entered a waiting state inside that window. The verdict is three-valued rather than two: a terminating or completed pod says nothing and counts as neither evidence nor a blocker, so a resolution needs at least one pod actually confirmed healthy and an all-terminating set is not a vacuous "all pods Ready". Zero pods is not recovery by default; it falls through to the workload, where no Deployment at all or a Deployment scaled to zero resolves as `workload no longer running`, while a Deployment that wants replicas and has none stays open. Any error reading the cluster leaves the incident open, because not knowing is not recovery. The reason is written to `spec.resolution.action`, so why an incident closed lives on the object rather than only in a log line that will rotate. A Failed or evicted pod matching the workload holds its incident open until it is garbage-collected, deliberately, under prefer-open-to-wrongly-closed; the reason names the pod so it is actionable. This was reported as `platform/checkout` reaching 51 occurrences and then going `Resolved` while still crash-looping.
- **Diagnosis is partitioned by application, so one incident can no longer span several.** `diagnoseOOM` and `diagnoseCrashLoop` took *all* signals of a type as a single finding, named `firstPersonaRef` as the owner, and listed every affected pod, so four broken apps in three namespaces collapsed into one incident owned by whichever persona sorted first. A new `detection.GroupSignals` partitions signals into per-persona, per-unattributed-workload and cluster-scoped groups **before** diagnosis, so a rule can only ever see one application's signals and therefore can only ever describe one application. This is also the upstream cause of the invented cluster-wide root causes: given every pod eviction in the cluster as one finding, the model reasonably reported cluster-wide memory pressure on nodes that were sitting at 23%. Pinned by `TestReconcile_Acceptance_FourBrokenAppsAreFourIncidents`, which puts 4 broken apps in 3 namespaces through the real collector, correlator and rules and requires 4 incidents.
- **Mass resolution had the same root cause and is fixed by the same change.** Because only one diagnosis existed per signal type, only one entry landed in `activeSignalKeys`, so the other applications' incidents matched nothing that cycle and aged out through the grace period. Reported as a separate defect; it was a symptom of the bundling above.
- **A foreign `Update`-operation field manager that owns a container's `resources` block now counts as ownership.** The classifier counted `Apply`-operation managers only, on the stated premise that an `Update` entry (what `kubectl patch` and `kubectl set` produce) claims no ongoing ownership and so predicts nothing about a future conflict. That premise is false, and was reproduced against kube-apiserver 1.36.2: server-side apply conflicts with whoever owns the field, however they came to own it. `unrecognisedApplier` is replaced by `foreignFieldOwner`, which counts any foreign `Apply` entry (unchanged) plus a foreign `Update` entry owning the exact field set a remediation writes. The check reads `managedFields[].fieldsV1` rather than treating any foreign `Update` as ownership, because without that scoping every cluster running KEDA, an HPA-alike or a sidecar injector would classify as owned and Dorgu would refuse to heal anything: a manager holding only `spec.replicas` or a pod-template annotation is not in the way of a resource patch. `kubectl-*` managers stay excluded on purpose, so a Deployment carrying `kubectl-set:Update` on the target fields still reads `unmanaged` and is still healed. That is a human with kubectl, which is the definition of unmanaged, and healing there leaves the user better off: the `Update` moves the claim onto `dorgu` and the CLI then drops it, clearing a pre-existing conflict instead of adding one.
- **A refusal on an unknown owner names the field manager it refused on behalf of,** in both the plan step and the owner instruction. "Dorgu could not identify what manages this Deployment" is a dead end. The manager's name is a lead.
- **The `DorguEvent` cleaner's batch size is raised from 50 to 200, and it runs one pass at startup** instead of waiting a full interval. 50 records per 30 minutes barely exceeded the arrival rate, so the backlog never drained, and a restarted operator inherits the largest backlog it will ever see.

### Added

- **Unattributed incidents, for workloads no persona claims.** A broken Deployment with no matching ApplicationPersona used to be folded into a neighbouring application's incident, which is how one app got blamed for another's outage. It now gets its own incident recorded against the workload, carrying `spec.attribution: unattributed` and a `dorgu.io/attribution` label. The root cause says plainly that no persona claims it and points at `dorgu persona import`. No remediation is proposed for it and no persona status is synced, since there is no persona to sync. An unattributed incident hands over to an attributed one only when the attributed incident already exists and is open, so onboarding an app in the middle of an outage never leaves it with no incident at all.
- **`spec.attribution` on `IncidentMemory`,** a new optional enum of `persona` or `unattributed`. CRD regenerated in `config/crd/bases` and mirrored into `charts/dorgu-operator/crds`.
- **`DorguEvent` records are now bounded by count as well as age.** `dorguEvents.maxRecords` (default **2000**, `--dorgu-event-max-records`) prunes oldest first once the cap is exceeded, and `dorguEvents.retention` (default **24h**, `--dorgu-event-retention`) is the age bound, plumbed into both the writer and the cleaner so a record states the retention the operator is actually running with. A TTL alone bounded nothing: records arrive in proportion to cluster size, so five apps producing 187 records in 100 minutes were all inside the 24-hour window and all in etcd. Set `maxRecords: 0` to keep retention as the only bound.
- **`delete` on `dorguevents.dorgu.io` in the manager ClusterRole.** The cleaner existed but had no permission to act, so every cycle logged a failure and nothing was ever removed. The grant is scoped to Dorgu's own CRD and to no workload kind, so the published claim that the operator never writes workloads is unchanged; two tests now hold that line, one asserting the new verb and one asserting the operator still cannot create, update, patch or delete Deployments, ReplicaSets, Pods, Nodes or Namespaces. Verify it yourself rather than take it on trust: `kubectl auth can-i delete dorguevents.dorgu.io --as=system:serviceaccount:dorgu-system:dorgu-operator -A` says yes, the same question about `deployments` says no.

### Fixed

- **Signal attribution now requires exactly one persona.** Personas `api` and `api-server` both claim pod `api-server-7f9d-x2q`. The more specific claim now wins, and a genuine tie is left unattributed rather than decided by API list order. The correlator never crosses namespaces.
- **Diagnoses were dropped on a `NotFound` race rather than a `Conflict` one.** `retryIncidentWrite` now retries on both, with a backoff tuned for cache lag (6 steps to about 1.5s) instead of for optimistic concurrency (`retry.DefaultRetry` gives up in about 50ms). The status write straight after `Create` no longer re-reads on the first attempt, because the object returned by `Create` already carries a valid ResourceVersion and the unconditional `Get` was what exposed the write to a cache that had not caught up. `AlreadyExists` on create now adopts the existing incident and folds the diagnosis into it instead of returning an error and discarding a paid-for analysis. The rule from v0.9.0 is intact: a genuinely lost diagnosis is still ERROR plus a `DorguDiagnosisDiscarded` event plus a `DorguEvent`, never silently dropped.
- **kustomize detection never fired on kustomize's own output.** Confirmed against kustomize v5.8.1 as shipped in kubectl. The only marker kustomize generates itself is opt-in `buildMetadata: [managedByLabel]`, and it writes `app.kubernetes.io/managed-by: kustomize-v5.8.1`, which is **versioned**, so the old exact match on `kustomize` could only ever match a value a user hand-wrote into `commonLabels`. The managed-by label is now matched by prefix, and the two other opt-in build-metadata markers (`config.kubernetes.io/origin` and `alpha.config.kubernetes.io/transformations`) are recognised. The honest limit is now stated in the code and pinned by a test: **a plain overlay is undetectable and reads `unmanaged`.** kustomize emits no label and no annotation by default, and `kubectl apply -k` records the same `kubectl-client-side-apply:Update` manager as `kubectl apply -f`, because kustomize is a client-side renderer with no controller. There is nothing to detect, by design.
- **171 of 188 ERROR lines in a 20-minute window came from a write that had already succeeded.** `DorguEvent` names are content-addressed, the event informer re-lists everything it holds on each resync, and the resync period was the same 5 minutes as the store's dedup window, so a settled event came back round just as its dedup entry expired and the `Create` returned `AlreadyExists`. That is idempotent and is now handled as such, at `V(1)`. The store also stopped logging the error it returns: every caller already logged it, so each real failure was printed twice, at two layers.
- **A duplicate record aborted the pipeline before the emit step.** Because the benign `AlreadyExists` came back as an error, `processEvent` returned on it and the Kubernetes Event was never emitted. Nine ERROR lines a minute on a five-app cluster is enough to make log-based alerting useless on day one, and the docs point that alerting at `DorguDiagnosisDiscarded`, so the noise was breaking the mechanism it was reported through.
- **An event's reason is now part of what identifies it.** Neither the dedup key nor the record name included the Kubernetes Event reason, so two unrelated things happening to one object inside one category were the same event and the second was dropped. On an `IncidentMemory` that meant an ordinary health event could swallow the `DorguDiagnosisDiscarded` record users are told to alert on. Event times have second granularity, so same-second collisions between distinct reasons were losing real records too.
- **The event pipeline no longer observes its own output.** The emitter writes a Kubernetes Event for every record it stores, the watcher then saw that Event and stored a second `DorguEvent` saying Dorgu had said something. `DorguDetected` and `DorguDiagnosisDiscarded` accounted for 24 of the duplicate-key errors and every one of those records was an echo. Nothing is lost by dropping them: the Kubernetes Event alerting selects on is still emitted, and the reconciler writes its own `DorguEvent` directly.

## [0.9.0] - 2026-08-23

### Upgrade notes

> **Remediations for workloads owned by Helm, ArgoCD, Flux or kustomize are now recommendation-only. Dorgu will not patch them.** The operator observes the live Deployment, records who owns it, and shapes the plan as instructions for that owner's source of truth instead of a command that writes to the cluster.
>
> This is deliberate. Patching a Deployment that Helm or ArgoCD reconciles claims the fields Dorgu writes, and the next `helm upgrade` then fails outright with a field-manager conflict rather than quietly reverting the fix. A fix that breaks your next deploy is not a fix. Clean-room run #2 hit exactly that.
>
> **What is unchanged:** `persona-update` steps still patch the ApplicationPersona, the operator still does that itself, and ownership has no bearing on it. Their `autoExecutable` semantics are the same as before. Only the write to the Deployment is gated, and only `managedBy: unmanaged` permits it.
>
> **`unknown` is treated as owned.** Where the Deployment cannot be resolved, or its server-side applier is not one Dorgu recognises, `managedBy` is `unknown` and Dorgu explains rather than writes. Nothing is patched on a guess. One consequence worth knowing: a workload the operator cannot resolve loses its advisory `kubectl` commands too, so resolution failures now cost plan usefulness as well as grounding.
>
> **This guard is enforced by the CLI, so upgrade both.** The operator supplies the facts in `spec.workloadRef` and strips the workload-writing commands; refusing the patch is `dorgu remediation heal`'s job. **CLI v0.9.0 and older do not read `workloadRef` at all** and will still patch an owned Deployment on approval. Pair this release with **CLI v0.10.0 or newer**.

### Changed

- **Remediations are grounded in the live workload rather than the persona.** The ApplicationPersona is a point-in-time import, and in a brownfield cluster it drifts from the running Deployment. Dorgu previously computed sizes, stated numbers, and diffed as though the persona were authoritative, so it would quote a memory limit the container had not had for weeks. Every stated fact and every cap now comes from the live container: the rule-based proposer sizes increases off the live limit, the explanation quotes only observed values and names the Deployment and container they were read from, and where the workload is unreadable it says so and warns that the persona figure may have drifted.
- **The blast-radius cap is measured against the live value, not the higher of the two.** Capping against `max(persona, live)` still permits a 192Mi proposal on a 32Mi container whenever the persona is the stale, larger number, which was the reported case. The cap is now strictly relative to what the container actually runs with. An action's `prePatchState` still records the persona's prior value, because that is what a persona rollback restores.
- **Dorgu no longer proposes resource keys the workload does not have.** `observedResources` distinguishes absent from zero, so approving a memory fix can no longer silently introduce a CPU limit the Deployment never set. The rule-based path refuses to raise a limit the container does not set and says why; the AI path drops any patch leaf targeting a missing key, records the omission in the step rationale, and demotes a step whose patch is emptied by that pruning.
- **Plans are shaped for whoever owns the workload.** For an owned Deployment, every step whose command writes to the cluster loses that command and is rewritten as what to change at the source: chart values for a Helm release (hedged as chart-specific, because Dorgu has not read the chart), the Git manifests for an ArgoCD application, the Git source for a Flux resource, the kustomize overlay. Each carries one line on what a direct patch would have broken. Read-only commands such as `kubectl logs` and `kubectl get events` survive, because they matter most on the workloads Dorgu will not touch. An `unmanaged` workload keeps its direct `kubectl` command exactly as planned.
- **The AI planner prompt leads with ground truth.** A new `## Live workload (ground truth)` section sits ahead of the persona, which is now explicitly labelled a stale snapshot, followed by six grounding rules and six ownership rules. The only image versions the model may name are ones Dorgu has actually read, from the `dorgu.io/imported-image` annotation and `status.deployments`, so it can no longer invent a prior-good tag.

### Added

- **`spec.workloadRef` on `RemediationAction`,** populated by the operator at proposal time from the live Deployment. It carries `kind`, `name`, `namespace` and `container` (the live object's name, which in a brownfield cluster is usually not the persona name: persona `frontend` resolving to Deployment `frontend-podinfo`), the observed `resources` requests and limits where an empty string means the workload does not set that key, plus `observedImage` and `observedAt`.
- **`workloadRef.managedBy`,** one of `helm`, `argocd`, `flux`, `kustomize`, `unmanaged` or `unknown`, defaulting to `unknown`. It is derived from server-side-apply field managers plus labels and annotations, most specific owner first: ArgoCD, then Flux, then Helm, then kustomize. A Flux HelmRelease reads as `flux` rather than `helm`, because Flux is what reconciles it. Update-operation field managers, which is what `kubectl patch` and `kubectl set` leave behind, claim no ongoing ownership and so read as `unmanaged`. `managedByDetail` names the specific owner in prose, for example `Helm release "frontend" in namespace apps`, so a refusal can name what owns the workload instead of saying that something does.

### Fixed

- **176 AI diagnoses were silently discarded on write conflicts.** `updateExistingIncident` mutated the `IncidentMemory` it got from a `List` and called a bare `Update`, so that snapshot's ResourceVersion lost to any concurrent write and the whole diagnosis went out with the error. The status write beside it had been given `retry.RetryOnConflict` earlier; the spec write never was, and the spec write is the one carrying the AI root cause, which is why the diagnoses being dropped were the better ones. The spec write now retries with a re-fetch and re-applies the diagnosis to the fresh object, and `createIncident`'s initial status write got the same treatment, since a conflict there left an incident created but statusless with its diagnosis gone.
- **Losing a diagnosis is now visible instead of silent.** Nothing counted the losses, which is why 176 of them across 4h20m only surfaced by grepping raw logs. Exhausted retries now log at ERROR, record a `DorguEvent`, and emit a Kubernetes Warning on the incident under the new reason `DorguDiagnosisDiscarded`, so the loss shows up in `kubectl get events`, in `dorgu health`, and to log-based alerting. Every cycle ends with a tally: a cycle that lost work says so at ERROR, a clean one stays at `V(1)`.
- **Rejecting a remediation re-proposed the same fix 30 seconds later.** `Rejected` counted as terminal and therefore non-blocking, so the next health-check cycle proposed again, and `dorgu remediation reject` patched only `status.phase`, leaving no timestamp to hold a cooldown against. The remediation controller now stamps a `Rejected` condition once, on first sight of the phase, so re-reconciling cannot restart the clock. The health-check reconciler consults that history before calling the proposer, which puts the suppression ahead of any billable planning call. Suppression is scoped to the incident and its target and lifts after **1 hour** or when the signal materially changes, meaning the live diagnosis outranks the severity the declined incident was opened at: a warning someone waved off that has since gone critical is a different question. An unreadable rejection history fails closed, and a rejection with no timestamp yet still suppresses, because an un-timestamped no is still a no.
- **The release rehearsal deleted a tracked file.** The step 8 rehearsal packaged the chart into `dist/`, mirroring release CI, but `dist/install.yaml` is committed in this repo, so cleaning up with `rm -rf dist` removed it and it had to be restored before the release commit. The rehearsal now packages into `/tmp/dorgu-release`, with a comment recording why it differs from CI, and gained a step that renders the packaged chart's `NOTES.txt` before tagging: the notes are the one artifact no test sees until publish, since `helm template` cannot render them at all.

## [0.8.1] - 2026-08-22

### Fixed

- **The install notes pointed at a CLI release that has never existed.** `NOTES.txt` told anyone onboarding a brownfield cluster that `dorgu persona import` "requires CLI v0.8.2 or newer". The CLI tags go 0.8.0, 0.8.1, 0.9.0: there is no 0.8.2, and `persona import` shipped in **v0.9.0**. Someone on 0.8.1 reading the notes would conclude they were new enough, run the command, and get "unknown command". A chart test now pins the claim, so it can only change deliberately, and the constant carries the command for re-deriving it from the CLI repo.
- Two links in `CONTRIBUTING.md` to the CLI's contributing guide used `/blob/main/`. The CLI's default branch is `master`, so both 404'd. `README.md` already had it right.
- **The `/release` runbook described a repo we do not have.** It told the releaser to run `make check` (no such target here; it is `make test`), `go test ./test/chart/` (no such path; the chart guards live in `./charts/dorgu-operator/`), `goreleaser release --snapshot` (there is no GoReleaser in this repo, which ships a container image and an OCI Helm chart), `go install <module-path>@<VERSION>` as the install check (nothing installable), and `git push origin main` (the default branch is `master`, so the push would fail outright). Every step now matches what `.github/workflows/release.yaml` actually does, and pre-flight gained an uncapped `golangci-lint` run: the default caps of 50 issues per linter and 3 per message mean a release check can stop counting before it has told you the tree is red.

## [0.8.0] - 2026-08-16

### Upgrade notes

> **1. The chart now enables detection by default (`healthCheck.enabled: true`).** Before this release the flag defaulted to `false`, so following the chart's own installation instructions produced an operator that detected nothing, diagnosed nothing, and proposed nothing, with no hint as to why. Self-healing is the product, so the loop is on out of the box.
>
> On upgrade, a cluster that never set `healthCheck.enabled` will **start detecting**: expect `IncidentMemory` records to appear, and `RemediationAction` proposals wherever the `ClusterPersona` self-healing mode is `propose` (the default). Nothing is applied without human approval. To keep the previous behaviour:
>
> ```bash
> helm upgrade dorgu-operator ... --set healthCheck.enabled=false
> ```
>
> **AI stays opt-in and is unchanged.** Detection is local and free; inference costs money and sends cluster data to a third party, so it still requires `llm.provider`, a key, and `aiRemediation.enabled` set explicitly. A default install spends nothing on inference.
>
> The deployment template now renders `--enable-health-check=<value>` unconditionally rather than omitting the flag, so the setting is visible in the pod spec either way.

> **2. The validating webhook no longer exempts unlabelled Deployments.** A Deployment without an `app.kubernetes.io/name` label on the object itself was skipped with "no app.kubernetes.io/name label; skipping persona validation". Helm, kustomize and most hand-written YAML label the pod template only, so in practice a large share of real workloads were never validated at all. The webhook now resolves the persona through the same fallback chain as the controller (`app.kubernetes.io/name` label, `app` label, `metadata.name`, then `spec.selector.matchLabels`), so those Deployments are matched and checked.
>
> **If you run `webhook.mode: enforcing`, workloads that previously passed unchecked may now be rejected.** They were never being validated, so nothing about them changed; what changed is that Dorgu can finally see them. Before upgrading an enforcing cluster, review the personas those workloads will now be validated against, or run one cycle in `webhook.mode: advisory` and read the warnings first:
>
> ```bash
> helm upgrade dorgu-operator ... --set webhook.mode=advisory
> ```
>
> Clusters with the webhook disabled (the default, `webhook.enabled: false`) are unaffected.

### Added

- **`RemediationStep.command`**, an optional ready-to-run kubectl command for an advisory step. A correct diagnosis used to end in prose: the planner would identify a mistyped image tag, name the correct one, and leave the reader to write the `kubectl set image` themselves. The AI planner is now asked for a fully resolved command where a single one does the job, and `dorgu remediation diff` prints it. The field is **never executed**, by the operator or the CLI: it is printed for a human to read and run. Because it can be model-authored it is filtered through `SanitizeStepCommand` before being persisted (single line, must start with `kubectl `, no shell metacharacters, bounded length), and a CEL rule plus `maxLength` on the CRD enforce the same thing at the API server for clients that write a `RemediationAction` directly.
- **The Helm chart ships a `NOTES.txt`.** Installing used to succeed in silence, including silence about detection being off. The notes now report the state the install actually landed in: whether detection is on (with the interval) or off (with the exact `helm upgrade` to turn it on), whether AI is on or off (with the secret and upgrade commands, and the reminder that rule-based detection and diagnosis need no key), the five CRDs to expect, `dorgu health`, `dorgu persona import` for a cluster that already has apps, and the incident and remediation commands.

### Changed

- **Detection is on by default.** See the upgrade note above. `charts/dorgu-operator/values.yaml` sets `healthCheck.enabled: true`, and four chart render tests guard the default, the opt out, the interval override, and the invariant that a default install enables no AI flags.
- **`IncidentMemory` resolution outcomes gained `acknowledged`** and `RemediationAction` phases gained `Acknowledged`, for an approved plan that had nothing to apply (see below).
- **`RemediationAction.spec.explanation` no longer restates `planSummary`.** The proposer wrote the root cause into `planSummary` and the same sentence with a prefix into `explanation`, so `dorgu remediation diff` printed one paragraph twice under two headings. `planSummary` stays the root cause (why it broke); `explanation` now describes the shape of the response, for example `AI remediation plan: 3 steps, 1 applied on approval and 2 advisory`, and says outright when a plan is all advisory and nothing will be applied for you.
- **The whole repo passes `golangci-lint`,** and the Lint workflow is green for the first time. 171 findings across ten linters were cleared: unchecked errors, repeated literals promoted to constants, preallocation, modernisation, unused parameters, a dead assignment, and two deprecated API uses. No behaviour changes. `main()` in `cmd/main.go` is excluded from `gocyclo` with the reasoning recorded in `.golangci.yml`, rather than restructuring the operator's startup path for a linter.
- **Removed `.github/workflows/ci.yaml`,** which triggered on `main` while the default branch is `master`, so it had never run once. Its `lint` and `test` jobs duplicate `lint.yml` and `test.yml`, which do run.

### Fixed

- **The AI diagnosis you paid for now reaches the incident record.** The operator logged `{"provider": "ai-enhanced", "count": 2}` while `grep -c ai-enhanced` across every `IncidentMemory` returned **0**: every incident read `Provider: rule-based`, `Confidence: 70%`. The cause was the diagnosis engine's "higher confidence wins" merge. `AIProvider` re-runs the rule-based logic and applies the LLM's `ConfidenceAdjustment`, which no response parser populates, so an AI-enhanced diagnosis carries the rule-based confidence *to the digit*. Under a strict `>` comparison over a provider list that starts with the rule-based provider, the AI result lost every tie and was dropped without a word. Ties now go to the later provider (the one enhancing the earlier result), the losing diagnosis is logged at INFO with the reason, and `updateExistingIncident` refreshes the root cause at equal confidence so an incident first recorded before AI was configured is upgraded rather than frozen as `rule-based`.
- **Approving an advisory remediation no longer fails it and blacks out the app for 30 minutes.** Dorgu proposed a `notification`-type remediation, printed its own approve command, and approving it produced `apply failed: precondition failed: unsupported action type "notification"`, phase `Failed`, and then `[rate-limit] failed remediation ... within 30m0s cooldown period`. An approved plan with nothing to apply now settles as **`Acknowledged`**: the approval is recorded, the operator changes nothing, and the incident is marked `acknowledged` (not resolved, because nothing was fixed). Separately, a plan the executor refuses before touching the cluster is recorded as `PreconditionRejected` and is **excluded from the failure cooldown**, which now only counts remediations that actually went wrong.
- **Dorgu's Kubernetes Events are recorded again.** Every event was dropped by client-go with `"Could not construct reference, will not report event" err="object does not implement the common interface for accessing the SelfLink"` (77 of them in one clean-room run), so `kubectl get events --field-selector reason=DorguDetected` returned nothing and anyone reading the operator log saw a wall of ERRORs. The emitter passed a hand-rolled `runtime.Object` carrying only a GVK, a name and a namespace; it now passes a `corev1.ObjectReference` with the apiVersion filled in for the kinds dorgu attaches events to. An event whose involved object has no kind or no name is refused with an error instead of being emitted into the void. `record.FakeRecorder` ignores the object it is handed, which is why the old tests passed: the new ones resolve the reference the way the real recorder does and assert the `Event` object is created through a real broadcaster and sink.
- **Cluster resource saturation is actually computed.** `usedCPU`, `usedMemory`, `cpuUtilization` and `memoryUtilization` were declared on `ClusterPersona.status.resourceSummary` and never written by anything, which is why `dorgu health` printed `CPU: n/a requests / allocatable ( / 3860m)` with an empty left operand on every cluster. They are now totalled from the resource requests of scheduled pods (sum of app containers, floored by the largest init container, terminal pods excluded), matching what the CLI renders as "requests / allocatable". Requests rather than live metrics deliberately: no metrics-server needed, so the figure is there on a default install. Utilization is left empty rather than reported as `0%` when allocatable is unknown.
- **A fix clamped by the 2x guardrail says so.** `report-worker` needed ~120M; the plan proposed 48Mi to 96Mi at confidence **0.88** with a summary asserting it resolves the OOM, and the pod went straight back to `OOMKilled`. 96Mi was not a judgement, it was the ceiling. When a proposed value lands at the blast-radius cap, the plan summary, the explanation and the step rationale now say "Clamped by the 2x blast-radius guardrail: ... may be insufficient and a second increase may be required", and the confidence is damped by 0.15 (0.88 becomes 0.75). This applies to both the rule-based proposer (where a critical OOM is capped at 2x in code) and the AI planner (whose prompt asks for changes within ~2x).
- Resolve an `ApplicationPersona` to its `Deployment` without requiring a label on the Deployment object. Helm, kustomize and most hand-written YAML label the pod template only, so personas for pre-existing apps sat `Pending` forever with `No Deployment with label app.kubernetes.io/name=<app>`. Resolution now walks an ordered chain: `app.kubernetes.io/name` label, `app` label, `metadata.name`, then `spec.selector.matchLabels`. When several Deployments match the same rung the persona reports `AmbiguousDeployment` and names the candidates instead of patching one at random; when none match, the message lists every rung that was tried.
- Validating webhook checks Deployments labelled on the pod template only, instead of exempting them with "no app.kubernetes.io/name label; skipping persona validation".

## [0.7.3] - 2026-08-07

### Upgrade note

> **Clusters running `selfHealing.mode: observe` will stop receiving remediation proposals after this upgrade.** The mode is now enforced (see below). Before 0.7.3 the field was inert, so an `observe` cluster still got `RemediationAction`s. Bootstrap versions before 0.7.3 hardcoded `mode: observe` on the auto-created `dorgu-cluster` persona, so most existing clusters are in exactly this state. Upgrading is safe but silent: detection and diagnosis keep running and `IncidentMemory` records keep appearing, while zero remediations are proposed.
>
> Check the current mode and restore the previous behavior with:
>
> ```bash
> kubectl get clusterpersona dorgu-cluster -o jsonpath='{.spec.policies.selfHealing.mode}'
> kubectl patch clusterpersona dorgu-cluster --type=merge \
>   -p '{"spec":{"policies":{"selfHealing":{"mode":"propose"}}}}'
> ```
>
> New installs are unaffected: the bootstrap and the CRD default both use `propose`. If a persona was created by a clone install of the 0.7.2 chart, its `mode` may have been pruned entirely (the bundled `ClusterPersona` CRD was missing `policies.selfHealing`), in which case the CRD default `propose` applies once the 0.7.3 CRDs are installed.

### Fixed

- **`selfHealing.mode: observe` is now enforced — the safety switch works** — the CRD advertised `observe | propose | auto-approve`, but no code branched on the mode: `observe` still created `RemediationAction`s, so the API made a safety promise it did not keep. The proposer now honors `spec.policies.selfHealing.mode` from the `ClusterPersona` before doing any work: **`observe`** detects, diagnoses, and records an `IncidentMemory` but creates **zero** `RemediationAction`s, logging `selfHealing.mode=observe — proposal suppressed` with a hint for switching to `propose`; **`propose`** (the default) keeps the existing behavior; **`auto-approve`** is **not implemented** and is accepted but degraded to `propose` with a prominent warning rather than silently auto-approving — approval is always required today. An unrecognized mode is treated as `propose` and logged. The gate runs ahead of the AI planner, so `observe` spends no API calls. The auto-created `dorgu-cluster` persona and the controller's default-fill now use `propose`, matching the CRD's kubebuilder default — they previously said `observe`, which would have suppressed every remediation out of the box once the mode was enforced. `approval.autoApproveRule` and `selfHealing.enabled` are marked **not yet implemented** on the Go types and in the CRD schema so they stop reading as promises.
- **`helm install ./charts/dorgu-operator` from a clone installed the wrong image and an incomplete CRD set** — release CI restamps `Chart.yaml` from the git tag at publish time, so the committed `version`/`appVersion` sat at `0.6.1` even at tag `v0.7.2`; since `templates/deployment.yaml` defaults the image tag to `appVersion`, a contributor install ran the **0.6.1 image — an operator with no AI planner**. The bundled `crds/` had drifted the same way: only **2 of 5** CRDs were committed, and the `ClusterPersona` CRD was missing the whole `policies.selfHealing` block, so a clone install silently pruned `mode`, `excludeNamespaces`, and `maxRemediationsPerHour`, and could not create `IncidentMemory`, `RemediationAction`, or `DorguEvent` at all. `Chart.yaml` is now pinned to `0.7.2`, all five CRDs are synced from `config/crd/bases`, and two chart tests guard both invariants: `appVersion` may not lag the latest git tag, and the bundled CRDs must match the generated ones byte for byte. `/release` gained an explicit chart-bump step. Published OCI installs were never affected.

## [0.7.2] - 2026-07-23

### Fixed

- **Remediation dedup per persona + target — one OOM → one remediation** — a single OOM spawns two incidents (`…-oomkilled` and `…-crashloopbackoff`), and the WS8 dedup keyed only on the incident, so each incident still produced its own AI `RemediationAction` (two RAs for one root cause). The proposer now also stands down when an active (`Pending`/`Approved`/`Applying`/`Verifying`) `RemediationAction` for the **same persona already remediates the same target** — keyed on the persona-spec patch path (e.g. `spec.resources.limits.memory`), read from both the rule-based `Action.Patch` and every AI `Steps[].Patch`. So the trailing CrashLoopBackOff incident finds the OOM incident's memory fix and skips. Terminal-phase actions (`Completed`/`Rejected`/`RolledBack`/`Failed`/`Expired`) never block a fresh recurrence; a different persona or a different target (e.g. CPU vs memory) still proposes.
- **RBAC gap — `replicasets.apps is forbidden`** — added the missing ClusterRole rule so the manager can read ReplicaSets (`apps/replicasets`, `get`/`list`/`watch`), used by the event correlator and the pod→ReplicaSet→Deployment ownership walk. Regenerated `config/rbac/role.yaml` and synced the bundled chart RBAC, quieting the recurring `cannot list resource "replicasets" in API group "apps"` error.

## [0.7.1] - 2026-07-22

### Fixed

- **Remediation multiplicity — one remediation per incident** — the proposer now dedups: it skips proposing when an active (`Pending`/`Approved`/`Applying`/`Verifying`) `RemediationAction` already targets the incident, so the 60s health-check loop no longer creates a fresh `RemediationAction` every cycle. Additionally, the legacy rule-based OOM path in the ApplicationPersona reconciler stands down when the health-check reconciler is active (which owns unified detection→diagnosis→remediation), so a single OOM no longer yields both a rule-based and an AI action. One incident → at most one remediation.
- **RBAC gaps broke saturation/metrics/event detection** — added the missing ClusterRole rules so these signals work on managed clusters (e.g. EKS): core `events` (`get`/`list`/`watch`, for the event watcher) and `pods` in the `metrics.k8s.io` API group (`get`/`list`, for the metrics-usage checker). Regenerated `config/rbac/role.yaml` and the bundled chart RBAC.
- **Missing `spec.nodeName` pod field index** — the manager now registers the `spec.nodeName` field index in its cache at startup, so the resource-saturation checker can list pods-by-node instead of failing with `Index with name field:spec.nodeName does not exist`.
- **Status-update conflict noise** — wrapped the remaining IncidentMemory/ApplicationPersona status writes (incident conditions, persona-status sync, incident auto-resolution) in `retry.RetryOnConflict` with a re-fetch, quieting the frequent `object has been modified` log noise from racing reconcilers.

## [0.7.0] - 2026-07-09

### Added

- **AI-generated ordered remediation plans (Anthropic BYOK)** — new `internal/remediation/planner` produces ordered, multi-step remediation plans from diagnosed incidents using Claude. The AI proposer gathers cluster context, prompts the model, and emits a validated ordered plan; falls back to the deterministic proposer when no key is configured. Bring-your-own-key, gated by the LLM provider/API-key configuration.
- **`RemediationAction.Steps[]` — ordered remediation plans** — the `RemediationAction` CRD now carries an ordered `steps` array, letting a single remediation express a sequenced plan (each step with its own action, target, and parameters) instead of a single flat proposal. Schema regenerated into the `remediationactions` CRD.
- **Secure Helm AI-key injection + `values-local` workflow** — the Helm chart now injects the Anthropic API key via a managed `llm-secret` referenced through `secretKeyRef` (never rendered into the Deployment spec), plus a `values-local.example.yaml` workflow for supplying the key locally without committing it.

### Fixed

- **Reliable ClusterPersona auto-create** — the startup bootstrap that auto-creates the default `dorgu-cluster` ClusterPersona is now reliable, correctly gating on existing personas and applying the bootstrap/cluster-uid annotations under race conditions.

## [0.6.1] - 2026-04-17

### Added

- Operator now auto-creates a default ClusterPersona named `dorgu-cluster` on startup if none exists. Gated behind `--auto-create-cluster-persona` (default `true`); disable with `--set operator.autoCreateClusterPersona=false` for GitOps-managed clusters. The persona carries `dorgu.io/bootstrap: "true"` and `dorgu.io/cluster-uid` annotations for multi-cluster traceability.

### Fixed

- Remediation skip reasons are now logged at INFO level (previously logged at verbose level, invisible at default operator log level).
- AI diagnosis can no longer suppress `resource-adjustment` proposals by returning a non-proposable action (e.g., `investigate`). The guard allows the AI to freely change non-proposable base actions, but blocks downgrades from proposable to non-proposable.

## [0.6.0] - 2026-04-13

### Added

- **Helm chart: Phase 2a/2b values** — `healthCheck.enabled` and `healthCheck.interval` now expose the `--enable-health-check` and `--health-check-interval` operator flags via Helm. Previously, detection, diagnosis, and remediation were inaccessible through standard `helm install`.
- **Helm chart: metrics-server toggle** — `metricsServer.enabled` (default true) controls `--enable-metrics-server` flag.
- **Helm chart: LLM / AI diagnosis values** — `llm.provider`, `llm.apiKey`, and `llm.model` expose BYOK AI-enhanced diagnosis (Claude or Gemini) without requiring raw flag overrides.
- Helm chart version bumped to `0.6.0` to align with operator release.

## [0.5.3] - 2026-04-09

### Fixed

- Fix Helm chart ClusterRole missing RBAC rules for `incidentmemories`, `remediationactions`, `dorguevents`, and their `/status` subresources. Operators deployed via Helm were silently unable to create or update incident and remediation CRDs.
- Fix "object has been modified" status update conflicts in `HealthCheckReconciler` and `RemediationController`. Status updates are now retried with `retry.RetryOnConflict` and a re-fetch before each attempt, preventing concurrent reconciler races from failing incident updates silently.
- Fix `RemediationAction` lifecycle events not broadcasting over WebSocket. The `RemediationController` now calls `BroadcastRemediation` at each phase transition (created, approved, completed, rolledback, rejected, failed).
- Fix WebSocket `request` handler returning `unknown_topic` error for `incidents` and `remediations` topics. Clients can now request an initial snapshot of active incidents and pending remediations on connect.

## [0.5.2] - 2026-04-07

### Fixed

- Fix ApplicationPersona reconciler only matching deployments with `app.kubernetes.io/name` label. Now falls back to common `app` label, matching the pattern already used by the events correlator.

## [0.5.1] - 2026-04-07

### Fixed

- Fix operator crash (`panic: close of closed channel`) when starting with `websocket.enabled=true`. Signal handler is now called once and shared between the WebSocket server and controller manager.
- Fix OOM workloads not producing IncidentMemory or RemediationAction CRDs. Added persona correlator to the detection engine that matches pod signals to ApplicationPersonas by namespace and name, enabling the full detect-diagnose-incident pipeline.
- Fix addon version reporting showing "latest" for OpenObserve. Added `helm.sh/chart` label parsing as fallback when `app.kubernetes.io/version` is missing. Image tags of "latest" are now reported as "unknown".
- Fix ClusterPersona reconciler not applying `selfHealing` defaults. Missing `mode` and `trustLevel` fields are now filled in during reconciliation (mode "observe", trustLevel 2).

## [0.5.0] - 2026-04-05

### Added

- **Remediation engine** — generates RemediationAction CRDs from diagnosed incidents with resource adjustment proposals (memory/CPU increases for OOM and saturation). Integrated into the health check reconciler's detect→diagnose→propose loop.
- **Safety guardrails** — rate limiting (5 per persona per hour, 1 concurrent), blast radius caps (max 2x resource increase), dry-run default (all proposals require approval), and namespace deny list (kube-system excluded).
- **Remediation controller** — watches RemediationAction CRDs through the full lifecycle: Pending → Approved → Applying → Verifying → Completed (or RolledBack/Failed). Applies JSON merge patches to ApplicationPersona CRDs. Auto-rollback on degradation using pre-patch state snapshots.
- **Post-apply verification** — re-runs detection engine after configurable wait period (default 10m) to confirm remediation improved health. Updates IncidentMemory with resolution details (action, outcome, duration).
- **AI-enhanced diagnosis (BYOK)** — `AIProvider` wraps rule-based diagnosis with LLM-generated explanations. Supports Anthropic Claude and Google Gemini via `--llm-provider` flag and `ANTHROPIC_API_KEY`/`GEMINI_API_KEY` env vars. Graceful degradation when no key configured.
- **WebSocket broadcast** — broadcasts incident, remediation, and health update events to connected CLI clients for real-time streaming (`dorgu health --watch`).
- **CloudNativePG addon discovery** — CNPG now appears in ClusterPersona addon list after blessed stack installation.
- `--llm-provider` flag for AI diagnosis provider selection (claude, gemini).
- `--llm-api-key` flag for API key override (prefers env vars).
- `--llm-model` flag for model override.

### Fixed

- Apply gofmt formatting to remediation controller files.

## [0.4.1] - 2026-03-31

### Fixed

- Add GitHub Release creation to release workflow — new releases now appear on the GitHub Releases page with release notes and Helm chart attached.

## [0.4.0] - 2026-03-29

### Added

- **IncidentMemory CRD** — namespaced CRD for tracking cluster incidents with detection signals, root cause analysis, confidence scoring, and resolution tracking. Supports cross-namespace relationship tracking via `relatedResources` field.
- **RemediationAction CRD** — namespaced CRD for remediation proposals with YAML diff, approval workflow, rollback spec, and progressive trust levels. Type definitions ready for Phase 2b execution.
- **DorguEvent CRD** — lightweight event persistence with TTL-based cleanup. Hybrid architecture: CRD-backed storage with in-memory LRU cache for fast reads.
- **Detection engine** with pluggable signal collectors: node health (Ready, MemoryPressure, DiskPressure, PIDPressure, NetworkUnavailable), pod failures (OOMKilled, CrashLoopBackOff, ImagePullBackOff, Evicted, long-Pending, high restarts), resource saturation (CPU/memory request vs allocatable with configurable thresholds), and control plane health (healthz/readyz endpoints, ComponentStatus, Lease freshness).
- **Optional metrics-server integration** — detects actual CPU/memory usage when metrics-server is available; graceful degradation without it.
- **Diagnosis engine** with deterministic rule-based provider covering 8 failure patterns: OOM root cause, CrashLoop correlation, node pressure, node down, resource saturation, control plane issues, image pull failures, and long-pending pods. Confidence scoring with signal clarity and time proximity factors.
- **Event processing pipeline** — K8s Event watcher via informers, event classifier (severity/category mapping), persona correlator (pod→deployment→ApplicationPersona matching), and K8s Event emitter for `kubectl describe` visibility.
- **Health check reconciler** — timer-based reconciler (configurable interval, default 60s) that runs the detect→diagnose→incident loop. Creates/updates IncidentMemory CRDs with deduplication via label-based matching. Auto-resolves incidents when triggering signals clear.
- **Incident controller** — watches IncidentMemory CRDs for lifecycle management, label maintenance, condition updates, and ApplicationPersona status synchronization (`status.activeIncidents`, `status.lastIncidentTime`).
- **SelfHealing policy** fields on ClusterPersona spec: `mode` (observe/propose/auto-approve), `trustLevel` (L0-L5, default L2), `maxRemediationsPerHour`, `excludeNamespaces`, and rollback configuration.
- `--enable-health-check` flag to opt into the health check reconciler and event pipeline.
- `--health-check-interval` flag for configurable reconciliation interval.
- `--enable-metrics-server` flag for metrics-server integration.

### Fixed

- Correct Confidence printcolumn type from number to string in CRD manifests.
- Resolve TOCTOU race in event dedup and startupTime data race in event watcher.
- Prefer pod version label over image digest for addon discovery.

## [0.3.0] - 2026-03-23

### Added

- Claude Code project configuration files for better project management.

### Fixed

- Handle JSON unmarshal errors and use server context in WebSocket handlers.

### Changed

- Extracted flag parsing from `cmd/main.go` into `cmd/config.go` with `operatorConfig` struct, removing `nolint:gocyclo` suppression.
- Refactored webhook validators to return slices instead of mutating pointer arguments.
- Extracted controller helpers: `setCondition`, validation, and status helpers into dedicated files.
- Extracted ClusterPersona discovery and addon helpers into dedicated files.
- Extracted WebSocket message handlers into `handlers.go` and replaced magic numbers with named constants.

## [0.2.5] - 2026-03-11

### Added

- OpenObserve addon discovery in ClusterPersona controller.
- Go reviewer command and agent.

### Fixed

- Correct NODES printer column and prevent phase regression.
- Naming changes for consistency.
- Resolved lint issues (ginkgo-linter, goconst, staticcheck).

## [0.2.x]

### Added

- ApplicationPersona and ClusterPersona CRD controllers with validation and lifecycle management.
- WebSocket server for real-time CLI communication.
- Prometheus metrics endpoint with custom persona metrics.
- Helm chart for operator deployment.
