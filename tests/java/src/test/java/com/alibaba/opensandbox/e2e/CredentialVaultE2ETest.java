/*
 * Copyright 2026 Alibaba Group Holding Ltd.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package com.alibaba.opensandbox.e2e;

import static org.junit.jupiter.api.Assertions.*;

import com.alibaba.opensandbox.sandbox.Sandbox;
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.Execution;
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.OutputMessage;
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.RunCommandRequest;
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.Credential;
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialAuth;
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialBinding;
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialBindingMetadata;
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialBindingMutationSet;
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialMatch;
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialMetadata;
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialMutationSet;
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialProxyConfig;
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialVaultCreateRequest;
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialVaultPatchRequest;
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialVaultState;
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CustomHeaderEntry;
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.NetworkPolicy;
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.NetworkRule;
import java.time.Duration;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.concurrent.TimeUnit;
import java.util.stream.Collectors;
import org.junit.jupiter.api.Assumptions;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Tag;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.Timeout;

@Tag("e2e")
@DisplayName("Credential Vault E2E Tests (JVM/Kotlin SDK)")
class CredentialVaultE2ETest extends BaseE2ETest {

    private static final String DEFAULT_TARGET_HOST = "credential-vault-e2e.opensandbox.test";
    private static final Map<String, String> SECRET_VALUES =
            Map.of(
                    "bearer-token", "vault-bearer-token",
                    "basic-token", "dXNlcjpwYXNz",
                    "api-key-token", "vault-api-key-token",
                    "client-id", "vault-client-id",
                    "client-secret", "vault-client-secret",
                    "runtime-token", "vault-runtime-token",
                    "runtime-token-replaced", "vault-runtime-token-replaced");

    @Test
    @Timeout(value = 5, unit = TimeUnit.MINUTES)
    void credentialVaultInjectsAllAuthTypes() {
        String targetIp = credentialVaultTargetIp();
        Sandbox sandbox = createCredentialVaultSandbox(targetIp);

        try {
            CredentialVaultState state =
                    sandbox.credentialVault()
                            .create(
                                    CredentialVaultCreateRequest.builder()
                                            .credentials(
                                                    credentials(
                                                            "bearer-token",
                                                            "basic-token",
                                                            "api-key-token",
                                                            "client-id",
                                                            "client-secret",
                                                            "runtime-token",
                                                            "runtime-token-replaced"))
                                            .bindings(
                                                    List.of(
                                                            binding(
                                                                    "bearer",
                                                                    "/bearer",
                                                                    CredentialAuth.bearer(
                                                                            "bearer-token")),
                                                            binding(
                                                                    "basic",
                                                                    "/basic",
                                                                    CredentialAuth.basic(
                                                                            "basic-token")),
                                                            binding(
                                                                    "api-key",
                                                                    "/api-key",
                                                                    CredentialAuth.apiKey(
                                                                            "X-Api-Key",
                                                                            "api-key-token")),
                                                            binding(
                                                                    "custom-headers",
                                                                    "/custom-headers",
                                                                    CredentialAuth.customHeaders(
                                                                            List.of(
                                                                                    CustomHeaderEntry
                                                                                            .builder()
                                                                                            .name(
                                                                                                    "X-Client-Id")
                                                                                            .credential(
                                                                                                    "client-id")
                                                                                            .build(),
                                                                                    CustomHeaderEntry
                                                                                            .builder()
                                                                                            .name(
                                                                                                    "X-Client-Secret")
                                                                                            .credential(
                                                                                                    "client-secret")
                                                                                            .build())))))
                                            .build());

            Set<String> authTypes =
                    state.getBindings().stream()
                            .map(CredentialBindingMetadata::getAuth)
                            .filter(auth -> auth != null)
                            .map(auth -> auth.getType())
                            .collect(Collectors.toSet());
            assertEquals(Set.of("bearer", "basic", "apiKey", "customHeaders"), authTypes);

            for (String path : List.of("/bearer", "/basic", "/api-key", "/custom-headers")) {
                String response = curlJson(sandbox, targetIp, path, true);
                assertJsonCase(response, path.substring(1), true, "[]");
            }
        } finally {
            killSandbox(sandbox);
        }
    }

    @Test
    @Timeout(value = 5, unit = TimeUnit.MINUTES)
    void credentialVaultRuntimeMutationAddsReplacesAndDeletesBinding() {
        String targetIp = credentialVaultTargetIp();
        Sandbox sandbox = createCredentialVaultSandbox(targetIp);

        try {
            CredentialVaultState state =
                    sandbox.credentialVault()
                            .create(
                                    CredentialVaultCreateRequest.builder()
                                            .credentials(List.of())
                                            .bindings(List.of())
                                            .build());
            assertEquals(1, state.getRevision());
            assertTrue(state.getCredentials().isEmpty());
            assertTrue(state.getBindings().isEmpty());

            state =
                    sandbox.credentialVault()
                            .patch(
                                    CredentialVaultPatchRequest.builder()
                                            .expectedRevision(state.getRevision())
                                            .credentials(
                                                    CredentialMutationSet.builder()
                                                            .add(
                                                                    List.of(
                                                                            credential(
                                                                                    "runtime-token",
                                                                                    "runtime-token")))
                                                            .build())
                                            .bindings(
                                                    CredentialBindingMutationSet.builder()
                                                            .add(
                                                                    List.of(
                                                                            binding(
                                                                                    "runtime-added",
                                                                                    "/runtime-added",
                                                                                    CredentialAuth
                                                                                            .apiKey(
                                                                                                    "X-Runtime-Token",
                                                                                                    "runtime-token"))))
                                                            .build())
                                            .build());
            assertEquals(2, state.getRevision());
            assertEquals(List.of("runtime-token"), credentialNames(state.getCredentials()));
            assertEquals(List.of("runtime-added"), bindingNames(state.getBindings()));

            String response = curlJson(sandbox, targetIp, "/runtime-added", true);
            assertJsonCase(response, "runtime-added", true, "[]");

            state =
                    sandbox.credentialVault()
                            .patch(
                                    CredentialVaultPatchRequest.builder()
                                            .expectedRevision(state.getRevision())
                                            .bindings(
                                                    CredentialBindingMutationSet.builder()
                                                            .delete("runtime-added")
                                                            .build())
                                            .build());
            assertEquals(3, state.getRevision());
            assertTrue(state.getBindings().isEmpty());

            state =
                    sandbox.credentialVault()
                            .patch(
                                    CredentialVaultPatchRequest.builder()
                                            .expectedRevision(state.getRevision())
                                            .credentials(
                                                    CredentialMutationSet.builder()
                                                            .replace(
                                                                    List.of(
                                                                            credential(
                                                                                    "runtime-token",
                                                                                    "runtime-token-replaced")))
                                                            .build())
                                            .bindings(
                                                    CredentialBindingMutationSet.builder()
                                                            .add(
                                                                    List.of(
                                                                            binding(
                                                                                    "runtime-replaced",
                                                                                    "/runtime-replaced",
                                                                                    CredentialAuth
                                                                                            .apiKey(
                                                                                                    "X-Runtime-Token",
                                                                                                    "runtime-token"))))
                                                            .build())
                                            .build());
            assertEquals(4, state.getRevision());
            assertEquals(List.of("runtime-token"), credentialNames(state.getCredentials()));
            assertEquals(List.of("runtime-replaced"), bindingNames(state.getBindings()));

            response = curlJson(sandbox, targetIp, "/runtime-replaced", true);
            assertJsonCase(response, "runtime-replaced", true, "[]");

            response = curlJson(sandbox, targetIp, "/runtime-added", false);
            assertJsonCase(response, "runtime-added", false, "[\"x-runtime-token\"]");

            state =
                    sandbox.credentialVault()
                            .patch(
                                    CredentialVaultPatchRequest.builder()
                                            .expectedRevision(state.getRevision())
                                            .bindings(
                                                    CredentialBindingMutationSet.builder()
                                                            .delete("runtime-replaced")
                                                            .build())
                                            .build());
            assertEquals(5, state.getRevision());
            assertTrue(state.getBindings().isEmpty());

            state =
                    sandbox.credentialVault()
                            .patch(
                                    CredentialVaultPatchRequest.builder()
                                            .expectedRevision(state.getRevision())
                                            .credentials(
                                                    CredentialMutationSet.builder()
                                                            .delete("runtime-token")
                                                            .build())
                                            .build());
            assertEquals(6, state.getRevision());
            assertTrue(state.getCredentials().isEmpty());
        } finally {
            killSandbox(sandbox);
        }
    }

    private static Sandbox createCredentialVaultSandbox(String targetIp) {
        String image =
                envOrDefault(
                        "OPENSANDBOX_CREDENTIAL_VAULT_E2E_SANDBOX_IMAGE", getSandboxImage());
        Map<String, String> resource = new HashMap<>();
        resource.put("cpu", envOrDefault("OPENSANDBOX_E2E_SANDBOX_CPU", "1"));
        resource.put("memory", envOrDefault("OPENSANDBOX_E2E_SANDBOX_MEMORY", "2Gi"));

        return Sandbox.builder()
                .connectionConfig(createConnectionConfig(false))
                .image(image)
                .resource(resource)
                .timeout(Duration.ofMinutes(5))
                .readyTimeout(Duration.ofSeconds(90))
                .networkPolicy(
                        NetworkPolicy.builder()
                                .defaultAction(NetworkPolicy.DefaultAction.DENY)
                                .addEgress(
                                        NetworkRule.builder()
                                                .action(NetworkRule.Action.ALLOW)
                                                .target(credentialVaultTargetHost())
                                                .build())
                                .addEgress(
                                        NetworkRule.builder()
                                                .action(NetworkRule.Action.ALLOW)
                                                .target(targetIp)
                                                .build())
                                .build())
                .credentialProxy(CredentialProxyConfig.enabled())
                .metadata(
                        Map.of(
                                envOrDefault(
                                        "OPENSANDBOX_CREDENTIAL_VAULT_E2E_LABEL_KEY",
                                        "opensandbox.e2e"),
                                envOrDefault(
                                        "OPENSANDBOX_CREDENTIAL_VAULT_E2E_LABEL_VALUE",
                                        "credential-vault")))
                .build();
    }

    private static List<Credential> credentials(String... names) {
        return java.util.Arrays.stream(names)
                .map(name -> credential(name, name))
                .collect(Collectors.toList());
    }

    private static Credential credential(String name, String valueName) {
        return Credential.builder().name(name).inlineSource(SECRET_VALUES.get(valueName)).build();
    }

    private static CredentialBinding binding(String name, String path, CredentialAuth auth) {
        return CredentialBinding.builder()
                .name(name)
                .match(
                        CredentialMatch.builder()
                                .schemes(CredentialMatch.Scheme.HTTP)
                                .ports(80)
                                .hosts(credentialVaultTargetHost())
                                .methods("GET")
                                .paths(path)
                                .build())
                .auth(auth)
                .build();
    }

    private static String curlJson(
            Sandbox sandbox, String targetIp, String path, boolean failOnHttpError) {
        String failFlag = failOnHttpError ? "--fail " : "";
        String command =
                "curl "
                        + failFlag
                        + "--silent --show-error --connect-timeout 5 --max-time 20 "
                        + "--resolve "
                        + credentialVaultTargetHost()
                        + ":80:"
                        + targetIp
                        + " http://"
                        + credentialVaultTargetHost()
                        + path;
        for (String secret : SECRET_VALUES.values()) {
            assertFalse(command.contains(secret), "command must not contain secret material");
        }

        Execution execution =
                sandbox.commands().run(RunCommandRequest.builder().command(command).build());
        assertNull(execution.getError(), "curl command failed");
        assertEquals(0, execution.getExitCode());
        String stdout =
                execution.getLogs().getStdout().stream()
                        .map(OutputMessage::getText)
                        .collect(Collectors.joining());
        assertFalse(stdout.isBlank(), "curl response must not be blank");
        return stdout;
    }

    private static void assertJsonCase(
            String payload, String expectedCase, boolean expectedOk, String expectedMissing) {
        assertTrue(payload.contains("\"ok\":" + expectedOk), payload);
        assertTrue(payload.contains("\"case\":\"" + expectedCase + "\""), payload);
        assertTrue(payload.contains("\"missingOrInvalid\":" + expectedMissing), payload);
    }

    private static List<String> credentialNames(List<CredentialMetadata> credentials) {
        return credentials.stream().map(CredentialMetadata::getName).collect(Collectors.toList());
    }

    private static List<String> bindingNames(List<CredentialBindingMetadata> bindings) {
        return bindings.stream().map(CredentialBindingMetadata::getName).collect(Collectors.toList());
    }

    private static String credentialVaultTargetHost() {
        return envOrDefault("OPENSANDBOX_CREDENTIAL_VAULT_E2E_TARGET_HOST", DEFAULT_TARGET_HOST);
    }

    private static String credentialVaultTargetIp() {
        String targetIp = System.getenv("OPENSANDBOX_CREDENTIAL_VAULT_E2E_TARGET_IP");
        Assumptions.assumeTrue(
                targetIp != null && !targetIp.isBlank(),
                "Set OPENSANDBOX_CREDENTIAL_VAULT_E2E_TARGET_IP to run Credential Vault E2E");
        return targetIp;
    }

    private static String envOrDefault(String name, String defaultValue) {
        String value = System.getenv(name);
        return value == null || value.isBlank() ? defaultValue : value;
    }

    private static void killSandbox(Sandbox sandbox) {
        try {
            sandbox.kill();
        } catch (Exception ignored) {
        }
        try {
            sandbox.close();
        } catch (Exception ignored) {
        }
    }
}
