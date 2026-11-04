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

//! Live-server credential-vault suite: every injection strategy, placeholder
//! substitution, runtime mutation with revision checks, and metadata
//! endpoints.
//!
//! Mirrors `tests/go/credential_vault_e2e_test.go`; skipped unless
//! `scripts/rust-e2e.sh` exported `OPENSANDBOX_E2E=1` and the echo target IP.

#[path = "e2e_common/mod.rs"]
mod e2e_common;

use std::collections::BTreeSet;

use opensandbox::{
    Credential, CredentialAuth, CredentialBinding, CredentialBindingMutationSet, CredentialMatch,
    CredentialMutationSet, CredentialSubstitution, CredentialVaultCreateRequest,
    CredentialVaultPatchRequest, CustomHeaderEntry, Sandbox,
};

use e2e_common::{
    create_vault_sandbox, kill_quietly, require_vault, vault_target_host, vault_target_ip,
};

fn secret(name: &str) -> &'static str {
    match name {
        "bearer-token" => "vault-bearer-token",
        "basic-token" => "dXNlcjpwYXNz",
        "api-key-token" => "vault-api-key-token",
        "client-id" => "vault-client-id",
        "client-secret" => "vault-client-secret",
        "query-secret" => "vault-query-secret",
        "path-secret" => "vault-path-secret",
        "body-secret" => "vault-body-secret",
        "runtime-token" => "vault-runtime-token",
        "runtime-token-replaced" => "vault-runtime-token-replaced",
        other => panic!("unknown vault secret {other:?}"),
    }
}

fn all_secrets() -> Vec<&'static str> {
    [
        "bearer-token",
        "basic-token",
        "api-key-token",
        "client-id",
        "client-secret",
        "query-secret",
        "path-secret",
        "body-secret",
        "runtime-token",
        "runtime-token-replaced",
    ]
    .into_iter()
    .map(secret)
    .collect()
}

fn credential(name: &str) -> Credential {
    credential_with_value(name, secret(name))
}

fn credential_with_value(name: &str, value: &str) -> Credential {
    Credential {
        name: name.to_string(),
        source: opensandbox::InlineCredentialSource {
            source_type: "inline".to_string(),
            value: value.to_string(),
        },
    }
}

fn binding(name: &str, path: &str, auth: CredentialAuth) -> CredentialBinding {
    binding_with_methods(name, path, auth, &["GET"])
}

fn binding_with_methods(
    name: &str,
    path: &str,
    auth: CredentialAuth,
    methods: &[&str],
) -> CredentialBinding {
    CredentialBinding {
        name: name.to_string(),
        r#match: CredentialMatch {
            schemes: Some(vec!["http".to_string()]),
            ports: None,
            hosts: vec![vault_target_host()],
            methods: Some(methods.iter().map(|m| m.to_string()).collect()),
            paths: Some(vec![path.to_string()]),
        },
        auth,
    }
}

fn assert_no_seleak(payload: &str, context: &str) {
    for value in all_secrets() {
        assert!(
            !payload.contains(value),
            "{context} leaked secret {value:?}: {payload}"
        );
    }
}

/// Runs a request from inside the sandbox against the echo target through the
/// egress proxy and returns the echoed JSON.
async fn vault_curl(sandbox: &Sandbox, path: &str, extra: &str) -> serde_json::Value {
    let target_ip = vault_target_ip().expect("vault suite requires the echo target IP");
    let host = vault_target_host();
    let command = format!(
        "curl --silent --show-error --connect-timeout 5 --max-time 30 {extra} \
         --resolve {host}:80:{target_ip} http://{host}{path}"
    );
    for value in all_secrets() {
        assert!(!command.contains(value), "command leaked secret {value:?}");
    }
    let execution = sandbox.exec(&command).await.expect("vault curl exec");
    assert_eq!(
        execution.exit_code,
        Some(0),
        "curl failed for {path}: {:?}",
        execution.text()
    );
    serde_json::from_str(execution.text().trim())
        .unwrap_or_else(|error| panic!("echo target returned invalid JSON for {path}: {error}"))
}

async fn assert_binding_ok(sandbox: &Sandbox, path: &str) {
    let response = vault_curl(sandbox, path, "").await;
    assert_eq!(response["ok"], true, "binding {path}: {response}");
    assert_eq!(response["case"], path.trim_start_matches('/'));
    assert!(
        response["missingOrInvalid"]
            .as_array()
            .is_some_and(Vec::is_empty),
        "binding {path} reported problems: {response}"
    );
}

#[tokio::test]
async fn vault_injects_all_auth_types() {
    require_vault!();
    let Some(sandbox) = create_vault_sandbox(&[("e2e.suite", "rust-vault")]).await else {
        return;
    };

    let state = sandbox
        .egress()
        .await
        .expect("egress client")
        .create_credential_vault(&CredentialVaultCreateRequest {
            credentials: vec![
                credential("bearer-token"),
                credential("basic-token"),
                credential("api-key-token"),
                credential("client-id"),
                credential("client-secret"),
            ],
            bindings: vec![
                binding(
                    "bearer",
                    "/bearer",
                    CredentialAuth {
                        auth_type: "bearer".to_string(),
                        credential: Some("bearer-token".to_string()),
                        name: None,
                        headers: None,
                        substitutions: None,
                    },
                ),
                binding(
                    "basic",
                    "/basic",
                    CredentialAuth {
                        auth_type: "basic".to_string(),
                        credential: Some("basic-token".to_string()),
                        name: None,
                        headers: None,
                        substitutions: None,
                    },
                ),
                binding(
                    "api-key",
                    "/api-key",
                    CredentialAuth {
                        auth_type: "apiKey".to_string(),
                        credential: Some("api-key-token".to_string()),
                        name: Some("X-Api-Key".to_string()),
                        headers: None,
                        substitutions: None,
                    },
                ),
                binding(
                    "custom-headers",
                    "/custom-headers",
                    CredentialAuth {
                        auth_type: "customHeaders".to_string(),
                        credential: None,
                        name: None,
                        headers: Some(vec![
                            CustomHeaderEntry {
                                name: "X-Client-Id".to_string(),
                                credential: "client-id".to_string(),
                            },
                            CustomHeaderEntry {
                                name: "X-Client-Secret".to_string(),
                                credential: "client-secret".to_string(),
                            },
                        ]),
                        substitutions: None,
                    },
                ),
            ],
        })
        .await
        .expect("create credential vault");

    // The sidecar must never echo credential material back.
    let payload = serde_json::to_string(&state).expect("serialize vault state");
    assert_no_seleak(&payload, "vault state");

    let auth_types = state
        .bindings
        .iter()
        .filter_map(|binding| binding.auth.as_ref())
        .map(|auth| auth.auth_type.clone())
        .collect::<BTreeSet<_>>();
    assert_eq!(
        auth_types,
        BTreeSet::from([
            "bearer".to_string(),
            "basic".to_string(),
            "apiKey".to_string(),
            "customHeaders".to_string(),
        ])
    );

    // Metadata endpoints expose names only, never secrets.
    let egress = sandbox.egress().await.expect("egress client");
    let credentials = egress.list_credentials().await.expect("list credentials");
    assert_eq!(credentials.credentials.len(), 5);
    assert!(credentials
        .credentials
        .iter()
        .any(|meta| meta.name == "bearer-token"));
    let meta = egress
        .credential("api-key-token")
        .await
        .expect("credential metadata");
    assert_eq!(meta.source_type, "inline");
    let bindings = egress.list_bindings().await.expect("list bindings");
    assert_eq!(bindings.bindings.len(), 4);
    let binding_meta = egress.binding("api-key").await.expect("binding metadata");
    assert_eq!(
        binding_meta.auth.as_ref().map(|auth| auth.name.clone()),
        Some(Some("X-Api-Key".to_string()))
    );
    let metadata_payload = serde_json::to_string(&bindings).expect("serialize bindings");
    assert_no_seleak(&metadata_payload, "binding metadata");

    for path in ["/bearer", "/basic", "/api-key", "/custom-headers"] {
        assert_binding_ok(&sandbox, path).await;
    }

    kill_quietly(&sandbox).await;
}

#[tokio::test]
async fn vault_substitutes_placeholders_in_query_path_and_body() {
    require_vault!();
    let Some(sandbox) = create_vault_sandbox(&[("e2e.suite", "rust-vault-subst")]).await else {
        return;
    };

    let substitution =
        |credential_name: &str, placeholder: &str, surface: &str| CredentialSubstitution {
            credential: credential_name.to_string(),
            placeholder: placeholder.to_string(),
            r#in: vec![surface.to_string()],
        };
    let state = sandbox
        .egress()
        .await
        .expect("egress client")
        .create_credential_vault(&CredentialVaultCreateRequest {
            credentials: vec![
                credential("query-secret"),
                credential("path-secret"),
                credential("body-secret"),
            ],
            bindings: vec![
                binding(
                    "query-substitution",
                    "/query-substitution",
                    CredentialAuth {
                        auth_type: "passthrough".to_string(),
                        credential: None,
                        name: None,
                        headers: None,
                        substitutions: Some(vec![substitution(
                            "query-secret",
                            "__query_secret__",
                            "query",
                        )]),
                    },
                ),
                binding(
                    "path-substitution",
                    "/tenant/*",
                    CredentialAuth {
                        auth_type: "passthrough".to_string(),
                        credential: None,
                        name: None,
                        headers: None,
                        substitutions: Some(vec![substitution(
                            "path-secret",
                            "__path_secret__",
                            "path",
                        )]),
                    },
                ),
                binding_with_methods(
                    "body-substitution",
                    "/body-substitution",
                    CredentialAuth {
                        auth_type: "passthrough".to_string(),
                        credential: None,
                        name: None,
                        headers: None,
                        substitutions: Some(vec![substitution(
                            "body-secret",
                            "__body_secret__",
                            "body",
                        )]),
                    },
                    &["POST"],
                ),
            ],
        })
        .await
        .expect("create credential vault");
    assert_eq!(state.revision, 1);

    let query_response = vault_curl(&sandbox, "/query-substitution?v=__query_secret__", "").await;
    assert_eq!(query_response["ok"], true, "{query_response}");

    let path_response = vault_curl(&sandbox, "/tenant/__path_secret__", "").await;
    assert_eq!(path_response["ok"], true, "{path_response}");

    let body_response = vault_curl(
        &sandbox,
        "/body-substitution",
        "--request POST --header 'content-type: application/json' \
         --data '{\"client_secret\":\"__body_secret__\"}'",
    )
    .await;
    assert_eq!(body_response["ok"], true, "{body_response}");

    kill_quietly(&sandbox).await;
}

#[tokio::test]
async fn vault_runtime_mutation_adds_replaces_and_deletes() {
    require_vault!();
    let Some(sandbox) = create_vault_sandbox(&[("e2e.suite", "rust-vault-mutate")]).await else {
        return;
    };
    let egress = sandbox.egress().await.expect("egress client");

    let empty = egress
        .create_credential_vault(&CredentialVaultCreateRequest {
            credentials: Vec::new(),
            bindings: Vec::new(),
        })
        .await
        .expect("create empty vault");
    assert_eq!(empty.revision, 1);
    assert!(empty.credentials.is_empty() && empty.bindings.is_empty());

    // Add a runtime credential plus binding and verify injection end to end.
    let state = egress
        .patch_credential_vault(&CredentialVaultPatchRequest {
            expected_revision: Some(empty.revision),
            credentials: Some(CredentialMutationSet {
                add: Some(vec![credential("runtime-token")]),
                replace: None,
                delete: None,
            }),
            bindings: Some(CredentialBindingMutationSet {
                add: Some(vec![binding(
                    "runtime-added",
                    "/runtime-added",
                    CredentialAuth {
                        auth_type: "apiKey".to_string(),
                        credential: Some("runtime-token".to_string()),
                        name: Some("X-Runtime-Token".to_string()),
                        headers: None,
                        substitutions: None,
                    },
                )]),
                replace: None,
                delete: None,
            }),
        })
        .await
        .expect("patch add");
    assert_eq!(state.revision, 2);
    assert_eq!(state.credentials.len(), 1);
    assert_eq!(state.bindings.len(), 1);
    assert_binding_ok(&sandbox, "/runtime-added").await;

    // Deleting the binding stops the injection immediately.
    let state = egress
        .patch_credential_vault(&CredentialVaultPatchRequest {
            expected_revision: Some(state.revision),
            credentials: None,
            bindings: Some(CredentialBindingMutationSet {
                add: None,
                replace: None,
                delete: Some(vec!["runtime-added".to_string()]),
            }),
        })
        .await
        .expect("patch delete binding");
    assert_eq!(state.revision, 3);
    assert!(state.bindings.is_empty());
    let stale = vault_curl(&sandbox, "/runtime-added", "").await;
    assert_eq!(
        stale["ok"], false,
        "deleted binding still injected: {stale}"
    );
    assert_eq!(
        stale["missingOrInvalid"],
        serde_json::json!(["x-runtime-token"]),
        "{stale}"
    );

    // Replacing the credential swaps the secret under the same name.
    let state = egress
        .patch_credential_vault(&CredentialVaultPatchRequest {
            expected_revision: Some(state.revision),
            credentials: Some(CredentialMutationSet {
                add: None,
                replace: Some(vec![credential_with_value(
                    "runtime-token",
                    secret("runtime-token-replaced"),
                )]),
                delete: None,
            }),
            bindings: Some(CredentialBindingMutationSet {
                add: Some(vec![binding(
                    "runtime-replaced",
                    "/runtime-replaced",
                    CredentialAuth {
                        auth_type: "apiKey".to_string(),
                        credential: Some("runtime-token".to_string()),
                        name: Some("X-Runtime-Token".to_string()),
                        headers: None,
                        substitutions: None,
                    },
                )]),
                replace: None,
                delete: None,
            }),
        })
        .await
        .expect("patch replace");
    assert_eq!(state.revision, 4);
    let payload = serde_json::to_string(&state).expect("serialize state");
    assert_no_seleak(&payload, "replaced vault state");
    assert_binding_ok(&sandbox, "/runtime-replaced").await;

    // Tear the vault back down through the patch endpoints.
    let state = egress
        .patch_credential_vault(&CredentialVaultPatchRequest {
            expected_revision: Some(state.revision),
            credentials: None,
            bindings: Some(CredentialBindingMutationSet {
                add: None,
                replace: None,
                delete: Some(vec!["runtime-replaced".to_string()]),
            }),
        })
        .await
        .expect("patch delete replaced binding");
    assert_eq!(state.revision, 5);
    let state = egress
        .patch_credential_vault(&CredentialVaultPatchRequest {
            expected_revision: Some(state.revision),
            credentials: Some(CredentialMutationSet {
                add: None,
                replace: None,
                delete: Some(vec!["runtime-token".to_string()]),
            }),
            bindings: None,
        })
        .await
        .expect("patch delete credential");
    assert_eq!(state.revision, 6);
    assert!(state.credentials.is_empty());

    kill_quietly(&sandbox).await;
}

#[tokio::test]
async fn vault_delete_removes_the_vault() {
    require_vault!();
    let Some(sandbox) = create_vault_sandbox(&[("e2e.suite", "rust-vault-delete")]).await else {
        return;
    };
    let egress = sandbox.egress().await.expect("egress client");

    egress
        .create_credential_vault(&CredentialVaultCreateRequest {
            credentials: vec![credential("api-key-token")],
            bindings: Vec::new(),
        })
        .await
        .expect("create vault");
    egress
        .delete_credential_vault()
        .await
        .expect("delete vault");

    let listed = egress.list_credentials().await;
    match listed {
        Err(opensandbox::Error::Api(api)) => {
            assert_eq!(api.status, 404, "unexpected api error {api:?}");
        }
        other => panic!("vault list after delete should 404, got {other:?}"),
    }

    kill_quietly(&sandbox).await;
}
