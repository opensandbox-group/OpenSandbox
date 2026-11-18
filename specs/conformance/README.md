# Conformance Profiles

A conformance profile is a machine-readable index of documented behavior claims. Each claim states one observable behavior in reviewable prose and points at the test that enforces it, so the promise and its executable form cannot drift apart silently.

Profiles are internal to this repository's test process. They do not define a new wire contract, a formal v1 conformance profile, certification, or capability negotiation; [Implementation compatibility](../api/implementation-compatibility.md) remains the document that scopes those topics, and the OpenAPI files under [`specs/`](../) remain the wire contracts. A profile interprets promises the project already documents and links them to tests of the reference implementation.

## Layout

```
specs/conformance/
├── README.md
└── profiles/
    └── execd-file-api.yaml
```

One profile per API surface. A profile is added when a surface has both reviewable claims and tests that enforce them; partial surfaces wait until they do.

## Profile format

```yaml
apiVersion: opensandbox.io/v1alpha1
kind: ConformanceProfile
metadata:
  name: execd-file-api
  version: 0.1.0
  description: Behavioral claims for the execd file upload and info surface.
contract:
  - docs/api/implementation-compatibility.md
  - specs/execd-api.yaml
claims:
  - id: FILE-PATH-ENV-001
    clause: >-
      A file path that references an undefined environment variable is rejected
      with an error status and an error message naming the undefined variables;
      nothing is created on disk.
    status: enforced
    tests:
      - components/execd/pkg/web/controller/filesystem_api_boundaries_test.go::TestUploadRejectsUndefinedEnvVarPath
```

| Field | Required | Meaning |
| --- | --- | --- |
| `apiVersion` | yes | Format version; currently `opensandbox.io/v1alpha1`. |
| `kind` | yes | Must be `ConformanceProfile`. |
| `metadata.name` | yes | Profile identity; one profile per API surface. Matches the file name. |
| `metadata.version` | yes | Profile revision. Bump it when claims are added, removed, promoted, or reworded. |
| `metadata.description` | no | One sentence about the covered surface. |
| `contract` | yes | Repository-relative references to the documents the claims interpret. These are pointers, not new normative text. |
| `claims` | yes | Non-empty list of behavior claims. |
| `claims[].id` | yes | Stable identifier matching `^[A-Z0-9][A-Z0-9-]{2,63}$`. Ids are append-only: links in issues and PRs depend on them. |
| `claims[].clause` | yes | The normative statement: one observable behavior, written so an API user could verify it. |
| `claims[].status` | yes | `enforced` or `pending`. |
| `claims[].tests` | yes | Non-empty list of `path/to/file.go::TestName` references. |
| `claims[].pending_ref` | for `pending` | URL of the open PR or issue that proposes the behavior. |

## Status semantics

| Status | Expected test state today | Rule enforced by the checker |
| --- | --- | --- |
| `enforced` | passes | The test must run and pass. A failure, a skip, a missing test, or an unresolvable reference is a violation. |
| `pending` | skipped while the behavior has not landed, passing once it has | The test must exist. A failure or missing result is a violation. A pass is a stale-profile violation: the behavior landed, so the claim must be promoted. |

A pending test is a probe, not an unconditional skip. It exercises the operation, verifies the parts that must stay stable, and skips only while the observed behavior still matches the pre-change behavior, naming the claim id and `pending_ref` in the skip message. The moment the referenced change lands, the same test passes, and the checker fails the stale claim, which is the prompt to promote it: set the status to `enforced`, replace the probe's skip branch with the enforced assertion the test documents (deleting only the `t.Skip` call would leave a test that still accepts the old behavior), drop `pending_ref`, and bump `metadata.version`. This keeps the skip hand-off observable in CI even when the referenced PR comes from a different author and never touches the test.

## Test references

- Only Go tests are executable by the current checker. A reference to another language fails validation instead of running unenforced; extend `scripts/conformance/run_profile.py` before adding such a reference.
- Mapped tests must be hermetic: handler-level, temporary directories, no sandbox, daemon, or network dependency. The profile runs in the normal unit-test job.
- A claim may map to multiple tests, and a test may enforce more than one claim.
- Do not map claims to undocumented extension routes (for example `/pty`, which [Implementation compatibility](../api/implementation-compatibility.md) notes is absent from `execd-api.yaml`).

## Running a profile

```bash
# Validate the profile and execute its mapped tests.
uv run scripts/conformance/run_profile.py specs/conformance/profiles/execd-file-api.yaml

# Validate every profile and execute their mapped tests.
uv run scripts/conformance/run_profile.py --all

# Validate structure and test references only, without running Go tests.
uv run scripts/conformance/run_profile.py --check-only specs/conformance/profiles/execd-file-api.yaml
```

The checker prints one line per claim pairing the expected state with the observed test result, and exits non-zero on any violation, quoting the claim id and clause. Exit codes: `0` all claims hold, `1` a claim violation, `2` an invalid or unusable profile.

CI runs every profile (`--all`) from the execd test workflow, so a newly added profile is picked up without workflow changes. Changes under `specs/conformance/`, `scripts/conformance/`, or the referenced contract documents trigger that workflow.

## Adding a claim

1. Pick the surface and add the claim to its profile, or create `profiles/<surface>.yaml` from the example above.
2. Write the `clause` so it describes observable behavior, not an implementation mechanism.
3. Add the test that enforces it. If the behavior is still only proposed, open the test as a skipped sample, set `status: pending`, and set `pending_ref`.
4. Run the checker locally with and without `--check-only`, then open the PR.
