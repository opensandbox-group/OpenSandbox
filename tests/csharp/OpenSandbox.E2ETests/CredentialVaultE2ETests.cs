// Copyright 2026 Alibaba Group Holding Ltd.
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

using System.Text.Json;
using OpenSandbox.Models;
using Xunit;

namespace OpenSandbox.E2ETests;

[Collection("CSharp E2E Tests")]
public class CredentialVaultE2ETests : IClassFixture<E2ETestFixture>
{
    private const string DefaultTargetHost = "credential-vault-e2e.opensandbox.test";

    private static readonly IReadOnlyDictionary<string, string> SecretValues = new Dictionary<string, string>
    {
        ["bearer-token"] = "vault-bearer-token",
        ["basic-token"] = "dXNlcjpwYXNz",
        ["api-key-token"] = "vault-api-key-token",
        ["client-id"] = "vault-client-id",
        ["client-secret"] = "vault-client-secret",
        ["runtime-token"] = "vault-runtime-token",
        ["runtime-token-replaced"] = "vault-runtime-token-replaced"
    };

    private readonly E2ETestFixture _fixture;

    public CredentialVaultE2ETests(E2ETestFixture fixture)
    {
        _fixture = fixture;
    }

    [Fact(Timeout = 5 * 60 * 1000)]
    public async Task CredentialVault_Injects_All_Auth_Types()
    {
        var targetIp = CredentialVaultTargetIp();
        if (targetIp is null)
        {
            return;
        }

        var sandbox = await CreateCredentialVaultSandboxAsync(targetIp);

        try
        {
            var state = await sandbox.CreateCredentialVaultAsync(
                Credentials(
                    "bearer-token",
                    "basic-token",
                    "api-key-token",
                    "client-id",
                    "client-secret",
                    "runtime-token",
                    "runtime-token-replaced"),
                new[]
                {
                    Binding("bearer", "/bearer", BearerAuth("bearer-token")),
                    Binding("basic", "/basic", BasicAuth("basic-token")),
                    Binding("api-key", "/api-key", ApiKeyAuth("X-Api-Key", "api-key-token")),
                    Binding(
                        "custom-headers",
                        "/custom-headers",
                        CustomHeadersAuth(
                            new CustomHeaderEntry { Name = "X-Client-Id", Credential = "client-id" },
                            new CustomHeaderEntry { Name = "X-Client-Secret", Credential = "client-secret" }))
                });

            var authTypes = state.Bindings
                .Select(binding => binding.Auth?.Type)
                .Where(type => type is not null)
                .Select(type => type!)
                .ToHashSet(StringComparer.Ordinal);
            Assert.True(
                new HashSet<string>(StringComparer.Ordinal)
                {
                    "bearer",
                    "basic",
                    "apiKey",
                    "customHeaders"
                }.SetEquals(authTypes));
            AssertStateDoesNotContainSecrets(state);

            foreach (var path in new[] { "/bearer", "/basic", "/api-key", "/custom-headers" })
            {
                using var response = await CurlJsonAsync(sandbox, targetIp, path);
                AssertJsonCase(response, path.TrimStart('/'), expectedOk: true, Array.Empty<string>());
            }
        }
        finally
        {
            await KillSandboxAsync(sandbox);
        }
    }

    [Fact(Timeout = 5 * 60 * 1000)]
    public async Task CredentialVault_Runtime_Mutation_Adds_Replaces_And_Deletes_Binding()
    {
        var targetIp = CredentialVaultTargetIp();
        if (targetIp is null)
        {
            return;
        }

        var sandbox = await CreateCredentialVaultSandboxAsync(targetIp);

        try
        {
            var state = await sandbox.CreateCredentialVaultAsync(Array.Empty<Credential>(), Array.Empty<CredentialBinding>());
            Assert.Equal(1, state.Revision);
            Assert.Empty(state.Credentials);
            Assert.Empty(state.Bindings);

            state = await sandbox.PatchCredentialVaultAsync(new CredentialVaultPatchRequest
            {
                ExpectedRevision = state.Revision,
                Credentials = new CredentialMutationSet
                {
                    Add = new[] { Credential("runtime-token", "runtime-token") }
                },
                Bindings = new CredentialBindingMutationSet
                {
                    Add = new[]
                    {
                        Binding(
                            "runtime-added",
                            "/runtime-added",
                            ApiKeyAuth("X-Runtime-Token", "runtime-token"))
                    }
                }
            });
            Assert.Equal(2, state.Revision);
            Assert.Equal(new[] { "runtime-token" }, state.Credentials.Select(credential => credential.Name));
            Assert.Equal(new[] { "runtime-added" }, state.Bindings.Select(binding => binding.Name));
            AssertStateDoesNotContainSecrets(state);

            using (var response = await CurlJsonAsync(sandbox, targetIp, "/runtime-added"))
            {
                AssertJsonCase(response, "runtime-added", expectedOk: true, Array.Empty<string>());
            }

            state = await sandbox.PatchCredentialVaultAsync(new CredentialVaultPatchRequest
            {
                ExpectedRevision = state.Revision,
                Bindings = new CredentialBindingMutationSet
                {
                    Delete = new[] { "runtime-added" }
                }
            });
            Assert.Equal(3, state.Revision);
            Assert.Empty(state.Bindings);

            state = await sandbox.PatchCredentialVaultAsync(new CredentialVaultPatchRequest
            {
                ExpectedRevision = state.Revision,
                Credentials = new CredentialMutationSet
                {
                    Replace = new[] { Credential("runtime-token", "runtime-token-replaced") }
                },
                Bindings = new CredentialBindingMutationSet
                {
                    Add = new[]
                    {
                        Binding(
                            "runtime-replaced",
                            "/runtime-replaced",
                            ApiKeyAuth("X-Runtime-Token", "runtime-token"))
                    }
                }
            });
            Assert.Equal(4, state.Revision);
            Assert.Equal(new[] { "runtime-token" }, state.Credentials.Select(credential => credential.Name));
            Assert.Equal(new[] { "runtime-replaced" }, state.Bindings.Select(binding => binding.Name));
            AssertStateDoesNotContainSecrets(state);

            using (var response = await CurlJsonAsync(sandbox, targetIp, "/runtime-replaced"))
            {
                AssertJsonCase(response, "runtime-replaced", expectedOk: true, Array.Empty<string>());
            }

            using (var response = await CurlJsonAsync(sandbox, targetIp, "/runtime-added", failOnHttpError: false))
            {
                AssertJsonCase(response, "runtime-added", expectedOk: false, new[] { "x-runtime-token" });
            }

            state = await sandbox.PatchCredentialVaultAsync(new CredentialVaultPatchRequest
            {
                ExpectedRevision = state.Revision,
                Bindings = new CredentialBindingMutationSet
                {
                    Delete = new[] { "runtime-replaced" }
                }
            });
            Assert.Equal(5, state.Revision);
            Assert.Empty(state.Bindings);

            state = await sandbox.PatchCredentialVaultAsync(new CredentialVaultPatchRequest
            {
                ExpectedRevision = state.Revision,
                Credentials = new CredentialMutationSet
                {
                    Delete = new[] { "runtime-token" }
                }
            });
            Assert.Equal(6, state.Revision);
            Assert.Empty(state.Credentials);
        }
        finally
        {
            await KillSandboxAsync(sandbox);
        }
    }

    private async Task<Sandbox> CreateCredentialVaultSandboxAsync(string targetIp)
    {
        return await Sandbox.CreateAsync(new SandboxCreateOptions
        {
            ConnectionConfig = _fixture.ConnectionConfig,
            Image = Environment.GetEnvironmentVariable("OPENSANDBOX_CREDENTIAL_VAULT_E2E_SANDBOX_IMAGE")
                ?? _fixture.DefaultImage,
            Resource = new Dictionary<string, string>
            {
                ["cpu"] = Environment.GetEnvironmentVariable("OPENSANDBOX_E2E_SANDBOX_CPU") ?? "1",
                ["memory"] = Environment.GetEnvironmentVariable("OPENSANDBOX_E2E_SANDBOX_MEMORY") ?? "2Gi"
            },
            ReadyTimeoutSeconds = 90,
            TimeoutSeconds = 5 * 60,
            NetworkPolicy = new NetworkPolicy
            {
                DefaultAction = NetworkRuleAction.Deny,
                Egress = new List<NetworkRule>
                {
                    new() { Action = NetworkRuleAction.Allow, Target = CredentialVaultTargetHost() },
                    new() { Action = NetworkRuleAction.Allow, Target = targetIp }
                }
            },
            CredentialProxy = new CredentialProxyConfig { Enabled = true },
            Metadata = new Dictionary<string, string>
            {
                [Environment.GetEnvironmentVariable("OPENSANDBOX_CREDENTIAL_VAULT_E2E_LABEL_KEY") ?? "opensandbox.e2e"] =
                    Environment.GetEnvironmentVariable("OPENSANDBOX_CREDENTIAL_VAULT_E2E_LABEL_VALUE") ?? "credential-vault"
            }
        });
    }

    private static IReadOnlyList<Credential> Credentials(params string[] names)
    {
        return names.Select(name => Credential(name, name)).ToList();
    }

    private static Credential Credential(string name, string valueName)
    {
        return new Credential
        {
            Name = name,
            Source = new InlineCredentialSource { Value = SecretValues[valueName] }
        };
    }

    private static CredentialBinding Binding(string name, string path, CredentialAuth auth)
    {
        return new CredentialBinding
        {
            Name = name,
            Match = new CredentialMatch
            {
                Schemes = new[] { "http" },
                Ports = new[] { 80 },
                Hosts = new[] { CredentialVaultTargetHost() },
                Methods = new[] { "GET" },
                Paths = new[] { path }
            },
            Auth = auth
        };
    }

    private static CredentialAuth BearerAuth(string credential)
    {
        return new CredentialAuth { Type = "bearer", Credential = credential };
    }

    private static CredentialAuth BasicAuth(string credential)
    {
        return new CredentialAuth { Type = "basic", Credential = credential };
    }

    private static CredentialAuth ApiKeyAuth(string name, string credential)
    {
        return new CredentialAuth { Type = "apiKey", Name = name, Credential = credential };
    }

    private static CredentialAuth CustomHeadersAuth(params CustomHeaderEntry[] headers)
    {
        return new CredentialAuth { Type = "customHeaders", Headers = headers };
    }

    private static async Task<JsonDocument> CurlJsonAsync(
        Sandbox sandbox,
        string targetIp,
        string path,
        bool failOnHttpError = true)
    {
        var failFlag = failOnHttpError ? "--fail " : "";
        var command =
            $"curl {failFlag}--silent --show-error --connect-timeout 5 --max-time 20 " +
            $"--resolve {CredentialVaultTargetHost()}:80:{targetIp} " +
            $"http://{CredentialVaultTargetHost()}{path}";
        foreach (var secret in SecretValues.Values)
        {
            Assert.DoesNotContain(secret, command, StringComparison.Ordinal);
        }

        var result = await sandbox.Commands.RunAsync(command);
        Assert.Null(result.Error);
        Assert.Equal(0, result.ExitCode);
        var stdout = string.Join("", result.Logs.Stdout.Select(output => output.Text));
        Assert.False(string.IsNullOrWhiteSpace(stdout));
        return JsonDocument.Parse(stdout);
    }

    private static void AssertJsonCase(
        JsonDocument payload,
        string expectedCase,
        bool expectedOk,
        IReadOnlyList<string> expectedMissingOrInvalid)
    {
        var root = payload.RootElement;
        Assert.Equal(expectedOk, root.GetProperty("ok").GetBoolean());
        Assert.Equal(expectedCase, root.GetProperty("case").GetString());
        var missingOrInvalid = root
            .GetProperty("missingOrInvalid")
            .EnumerateArray()
            .Select(item => item.GetString() ?? string.Empty)
            .ToArray();
        Assert.Equal(expectedMissingOrInvalid, missingOrInvalid);
    }

    private static void AssertStateDoesNotContainSecrets(CredentialVaultState state)
    {
        var payload = JsonSerializer.Serialize(state);
        foreach (var secret in SecretValues.Values)
        {
            Assert.DoesNotContain(secret, payload, StringComparison.Ordinal);
        }
    }

    private static string CredentialVaultTargetHost()
    {
        return Environment.GetEnvironmentVariable("OPENSANDBOX_CREDENTIAL_VAULT_E2E_TARGET_HOST")
            ?? DefaultTargetHost;
    }

    private static string? CredentialVaultTargetIp()
    {
        var targetIp = Environment.GetEnvironmentVariable("OPENSANDBOX_CREDENTIAL_VAULT_E2E_TARGET_IP");
        return string.IsNullOrWhiteSpace(targetIp) ? null : targetIp;
    }

    private static async Task KillSandboxAsync(Sandbox sandbox)
    {
        try
        {
            await sandbox.KillAsync();
        }
        catch (Exception ex)
        {
            Console.Error.WriteLine($"KillSandboxAsync: sandbox.KillAsync() failed during cleanup: {ex}");
        }

        await sandbox.DisposeAsync();
    }
}
