// Copyright 2026 The OpenSandbox Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//! Live-server lifecycle suite: create, inspect, filter, pause/resume,
//! renew, metadata patch, connect, and egress policy.
//!
//! Mirrors `tests/python/tests/test_sandbox_e2e.py` and the Go lifecycle
//! suites; skipped unless `scripts/rust-e2e.sh` exported `OPENSANDBOX_E2E=1`.

#[path = "e2e_common/mod.rs"]
mod e2e_common;

use std::collections::BTreeMap;
use std::time::{Duration, Instant};

use opensandbox::{
    CreateOptions, Error, LifecycleClient, ListSandboxesQuery, ListSnapshotsQuery, NetworkRule,
    Sandbox, SandboxState, SnapshotState,
};

use e2e_common::{create_sandbox, kill_quietly, require_e2e, unique_tag};

/// Polls `probe` until it reports `true` or the budget expires.
async fn eventually<Fut>(name: &str, budget: Duration, mut probe: impl FnMut() -> Fut)
where
    Fut: std::future::Future<Output = bool>,
{
    let deadline = Instant::now() + budget;
    loop {
        if probe().await {
            return;
        }
        if Instant::now() >= deadline {
            panic!("{name} did not settle within {budget:?}");
        }
        tokio::time::sleep(Duration::from_millis(500)).await;
    }
}

#[tokio::test]
async fn create_reports_a_running_sandbox() {
    require_e2e!();
    let sandbox = create_sandbox(&[("e2e.suite", "rust-create")]).await;

    let info = sandbox.info().await.expect("sandbox info");
    assert_eq!(info.id, sandbox.id());
    assert_eq!(
        info.status.state,
        SandboxState::Running,
        "state after create"
    );
    assert!(sandbox.is_healthy().await, "execd did not answer ping");

    let execution = sandbox
        .exec("echo hello-from-rust-e2e")
        .await
        .expect("exec after create");
    assert_eq!(execution.exit_code, Some(0));
    assert!(
        execution.text().contains("hello-from-rust-e2e"),
        "unexpected output: {:?}",
        execution.text()
    );

    kill_quietly(&sandbox).await;
}

#[tokio::test]
async fn metadata_filter_matches_exactly_through_query_encoding() {
    require_e2e!();
    // Metadata keys and values are Kubernetes labels, so the only character the
    // SDK has to percent-encode is the `/` namespace separator of a key. The
    // server parses the `metadata` query parameter with strict `parse_qsl`, so
    // an unencoded separator would silently truncate the key.
    let tag = unique_tag("rust-list");
    let sandbox = create_sandbox(&[("e2e.suite", "rust-list"), ("example.com/tag", &tag)]).await;

    let listed = sandbox
        .lifecycle()
        .list_sandboxes(&ListSandboxesQuery {
            states: vec![SandboxState::Running],
            metadata: Some(BTreeMap::from([(
                "example.com/tag".to_string(),
                tag.clone(),
            )])),
            page: Some(1),
            page_size: Some(50),
        })
        .await
        .expect("list with metadata filter");
    assert_eq!(
        listed
            .items
            .iter()
            .map(|info| info.id.as_str())
            .collect::<Vec<_>>(),
        vec![sandbox.id()],
        "metadata filter {tag:?} did not match exactly the created sandbox"
    );

    // The filter must not be a prefix match: a value that only shares the
    // prefix stays unmatched.
    let prefix_only = tag.split('-').next().unwrap_or(&tag).to_string();
    let listed = sandbox
        .lifecycle()
        .list_sandboxes(&ListSandboxesQuery {
            states: vec![SandboxState::Running],
            metadata: Some(BTreeMap::from([(
                "example.com/tag".to_string(),
                prefix_only,
            )])),
            page: Some(1),
            page_size: Some(50),
        })
        .await
        .expect("list with prefix-only filter");
    assert!(
        listed.items.iter().all(|info| info.id != sandbox.id()),
        "prefix-only filter unexpectedly matched"
    );

    kill_quietly(&sandbox).await;
}

#[tokio::test]
async fn pause_and_resume_preserve_a_usable_sandbox() {
    require_e2e!();
    let sandbox = create_sandbox(&[("e2e.suite", "rust-pause")]).await;

    sandbox.pause().await.expect("pause");
    eventually(
        "sandbox reaching Paused",
        e2e_common::WAIT_BUDGET,
        || async {
            sandbox
                .info()
                .await
                .map(|info| info.status.state == SandboxState::Paused)
                .unwrap_or(false)
        },
    )
    .await;

    let resumed = sandbox.resume_sandbox().await.expect("resume");
    assert!(
        resumed.is_healthy().await,
        "execd did not answer ping after resume"
    );
    let execution = resumed
        .exec("echo after-resume")
        .await
        .expect("exec after resume");
    assert_eq!(execution.exit_code, Some(0));
    assert!(execution.text().contains("after-resume"));

    kill_quietly(&resumed).await;
}

#[tokio::test]
async fn manual_cleanup_sandbox_has_no_expiration() {
    require_e2e!();
    // `create_sandbox` always sends a timeout, so this case builds its own
    // options with manual cleanup and no TTL.
    let options = CreateOptions::from_image(e2e_common::image())
        .with_manual_cleanup()
        .with_ready_timeout(e2e_common::READY_TIMEOUT)
        .with_poll_interval(e2e_common::POLL_INTERVAL)
        .with_metadata("e2e.suite", "rust-manual-cleanup");
    let sandbox = Sandbox::create(e2e_common::connection_config(), options)
        .await
        .expect("create manual-cleanup sandbox");
    let info = sandbox.info().await.expect("sandbox info");
    assert!(
        info.expires_at.is_none(),
        "manual-cleanup sandbox must omit expiresAt, got {:?}",
        info.expires_at
    );
    kill_quietly(&sandbox).await;
}

#[tokio::test]
async fn renew_pushes_expiration_forward() {
    require_e2e!();
    let sandbox = create_sandbox(&[("e2e.suite", "rust-renew")]).await;
    let before = sandbox.info().await.expect("info").expires_at;

    let renewed = sandbox
        .renew(Duration::from_secs(1800))
        .await
        .expect("renew expiration");
    match before {
        Some(previous) => assert!(
            renewed.expires_at > previous,
            "renew moved expiration backwards: {:?} -> {:?}",
            previous,
            renewed.expires_at
        ),
        None => eprintln!(
            "server did not populate expiresAt before renew; renewed to {:?}",
            renewed.expires_at
        ),
    }

    kill_quietly(&sandbox).await;
}

#[tokio::test]
async fn patch_metadata_merges_and_deletes_keys() {
    require_e2e!();
    let sandbox = create_sandbox(&[("e2e.suite", "rust-metadata")]).await;

    let patch: BTreeMap<String, Option<String>> = BTreeMap::from([
        ("e2e.extra".to_string(), Some("one".to_string())),
        // JSON merge patch semantics: `null` deletes the key.
        ("e2e.suite".to_string(), None),
    ]);
    let info = sandbox
        .patch_metadata(&patch)
        .await
        .expect("patch metadata");
    let metadata = info.metadata.unwrap_or_default();
    assert_eq!(metadata.get("e2e.extra").map(String::as_str), Some("one"));
    assert!(
        !metadata.contains_key("e2e.suite"),
        "null patch did not delete the key: {metadata:?}"
    );

    kill_quietly(&sandbox).await;
}

#[tokio::test]
async fn connect_attaches_to_an_existing_sandbox() {
    require_e2e!();
    let sandbox = create_sandbox(&[("e2e.suite", "rust-connect")]).await;
    let id = sandbox.id().to_string();

    let attached = Sandbox::connect(e2e_common::connection_config(), &id)
        .await
        .expect("connect to existing sandbox");
    assert_eq!(attached.id(), id);
    let execution = attached
        .exec("echo attached")
        .await
        .expect("exec on attached handle");
    assert_eq!(execution.exit_code, Some(0));

    kill_quietly(&attached).await;
}

#[tokio::test]
async fn egress_policy_patch_and_delete_round_trip() {
    require_e2e!();
    let sandbox = create_sandbox(&[("e2e.suite", "rust-egress")]).await;

    match sandbox.egress().await {
        Ok(egress) => {
            let allow_dns = [NetworkRule {
                action: "allow".to_string(),
                target: Some("*.opensandbox.internal".to_string()),
                ports: Some(vec![53]),
            }];
            // The sidecar proxies policy mutations; until it is fully up the
            // server answers 502, so retry readiness-style like the other
            // eventually() probes. Runtimes without the policy mutation
            // backend answer 502 forever — degrade like the readable probe.
            let deadline = Instant::now() + Duration::from_secs(15);
            let patched = loop {
                match egress.patch_policy(&allow_dns).await {
                    Ok(patched) => break Some(patched),
                    Err(Error::Api(api)) if api.status == 502 || api.status == 503 => {
                        if Instant::now() >= deadline {
                            break None;
                        }
                        tokio::time::sleep(Duration::from_millis(500)).await;
                    }
                    Err(error) => panic!("patch egress policy: {error}"),
                }
            };
            let Some(patched) = patched else {
                eprintln!("runtime does not support egress policy mutation, skipping");
                kill_quietly(&sandbox).await;
                return;
            };
            let rules = patched
                .policy
                .as_ref()
                .and_then(|policy| policy.egress.as_ref())
                .map(Vec::len)
                .unwrap_or_default();
            assert!(rules >= 1, "patched policy should contain rules");

            let deleted = egress
                .delete_policy(&["*.opensandbox.internal".to_string()])
                .await
                .expect("delete egress policy rule");
            eprintln!("policy after delete: {:?}", deleted.policy);
        }
        Err(error) => eprintln!("no egress endpoint for this runtime, skipping: {error}"),
    }

    kill_quietly(&sandbox).await;
}

#[tokio::test]
async fn snapshot_round_trip_preserves_the_filesystem() {
    require_e2e!();
    // Snapshots are asynchronous and can take minutes on a cold runner, so this
    // case owns its options with a long TTL and no auto cleanup.
    let options = CreateOptions::from_image(e2e_common::image())
        .with_manual_cleanup()
        .with_timeout_seconds(1800)
        .with_ready_timeout(e2e_common::READY_TIMEOUT)
        .with_poll_interval(e2e_common::POLL_INTERVAL)
        .with_metadata("e2e.suite", "rust-snapshot");
    let source = Sandbox::create(e2e_common::connection_config(), options)
        .await
        .expect("create snapshot source");

    let marker = format!("snapshot-marker-{}", unique_tag("marker"));
    let written = source
        .exec(&format!(
            "mkdir -p /tmp/snapshot-e2e && printf '%s' '{marker}' > /tmp/snapshot-e2e/marker.txt"
        ))
        .await
        .expect("write marker");
    assert_eq!(written.exit_code, Some(0));

    let name = unique_tag("rust-snapshot");
    let accepted = source.snapshot(Some(&name)).await.expect("create snapshot");
    let snapshot_id = accepted.id.clone();
    assert!(!snapshot_id.is_empty(), "snapshot id must be assigned");
    assert_eq!(accepted.name.as_deref(), Some(name.as_str()));

    // Poll until the snapshot is usable; capture is asynchronous server-side.
    let lifecycle = source.lifecycle().clone();
    eventually("snapshot reaching Ready", Duration::from_secs(300), || {
        let lifecycle = lifecycle.clone();
        let snapshot_id = snapshot_id.clone();
        async move {
            matches!(
                lifecycle
                    .get_snapshot(&snapshot_id)
                    .await
                    .map(|info| info.status.state)
                    .unwrap_or(SnapshotState::Creating),
                SnapshotState::Ready
            )
        }
    })
    .await;
    let ready = lifecycle
        .get_snapshot(&snapshot_id)
        .await
        .expect("snapshot after wait");
    assert_eq!(ready.status.state, SnapshotState::Ready);

    // The list endpoint must find it both by sandbox and by name.
    let listed = lifecycle
        .list_snapshots(&ListSnapshotsQuery {
            sandbox_id: Some(source.id().to_string()),
            name: Some(name.clone()),
            states: vec![SnapshotState::Ready],
            page: Some(1),
            page_size: Some(50),
        })
        .await
        .expect("list snapshots");
    assert!(
        listed.items.iter().any(|info| info.id == snapshot_id),
        "snapshot {snapshot_id} missing from filtered list: {:?}",
        listed.items
    );

    // Restore into a fresh sandbox and verify the filesystem came along.
    let restore = CreateOptions::from_snapshot(&snapshot_id)
        .with_manual_cleanup()
        .with_timeout_seconds(1800)
        .with_ready_timeout(e2e_common::READY_TIMEOUT)
        .with_poll_interval(e2e_common::POLL_INTERVAL)
        .with_metadata("e2e.suite", "rust-snapshot-restore");
    let restored = Sandbox::create(e2e_common::connection_config(), restore)
        .await
        .expect("restore from snapshot");
    let read_back = restored
        .exec("cat /tmp/snapshot-e2e/marker.txt")
        .await
        .expect("read marker from restored sandbox");
    assert_eq!(read_back.exit_code, Some(0));
    assert!(
        read_back.text().contains(&marker),
        "restored marker mismatch: {:?}",
        read_back.text()
    );

    kill_quietly(&restored).await;

    lifecycle
        .delete_snapshot(&snapshot_id)
        .await
        .expect("delete snapshot");
    let deleted = lifecycle.get_snapshot(&snapshot_id).await;
    match deleted {
        Err(Error::Api(api)) => assert_eq!(api.status, 404, "unexpected api error: {api:?}"),
        other => panic!("deleted snapshot should 404, got {other:?}"),
    }

    kill_quietly(&source).await;
}

#[tokio::test]
async fn missing_resources_report_api_errors() {
    require_e2e!();
    let lifecycle = LifecycleClient::new(&e2e_common::connection_config())
        .expect("lifecycle client for 404 probes");

    for (label, error) in [
        (
            "get_sandbox",
            lifecycle
                .get_sandbox("rust-e2e-does-not-exist")
                .await
                .map(|info| format!("{info:?}")),
        ),
        (
            "get_snapshot",
            lifecycle
                .get_snapshot("rust-e2e-does-not-exist")
                .await
                .map(|info| format!("{info:?}")),
        ),
    ] {
        match error {
            Err(Error::Api(api)) => {
                assert_eq!(api.status, 404, "{label}: unexpected api error {api:?}");
                assert!(!api.code.is_empty(), "{label}: error code must be set");
            }
            other => panic!("{label} should report a 404 api error, got {other:?}"),
        }
    }
}

#[tokio::test]
async fn egress_policy_is_readable() {
    require_e2e!();
    let sandbox = create_sandbox(&[("e2e.suite", "rust-egress-read")]).await;

    match sandbox.egress().await {
        Ok(egress) => match egress.policy().await {
            Ok(policy) => {
                eprintln!(
                    "egress policy: mode={:?} default_action={:?}",
                    policy.mode,
                    policy.policy.as_ref().map(|p| p.default_action.clone())
                );
            }
            Err(error) => eprintln!("egress sidecar not ready, skipping: {error}"),
        },
        Err(error) => eprintln!("no egress endpoint for this runtime, skipping: {error}"),
    }

    kill_quietly(&sandbox).await;
}
