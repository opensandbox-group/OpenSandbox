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

using System.Net;
using System.Text;
using System.Text.Json;
using FluentAssertions;
using OpenSandbox.Adapters;
using OpenSandbox.Core;
using OpenSandbox.Internal;
using OpenSandbox.Models;
using Xunit;

namespace OpenSandbox.Tests;

public class NetworkPolicyAdapterTests
{
    [Fact]
    public async Task GetPolicyAsync_ShouldParseControlPlanePolicy()
    {
        const string payload = """
        {
          "policy": {
            "defaultAction": "deny",
            "egress": [
              { "action": "allow", "target": "pypi.org" }
            ]
          },
          "enforcementMode": "enforce"
        }
        """;
        var handler = new CapturingHandler(payload);
        var adapter = CreateAdapter(handler, "fsb-1");

        var policy = await adapter.GetPolicyAsync();

        handler.Method.Should().Be(HttpMethod.Get);
        handler.PathAndQuery.Should().Be("/v1/sandboxes/fsb-1/networkpolicy");
        policy.DefaultAction.Should().Be(NetworkRuleAction.Deny);
        policy.Egress.Should().ContainSingle();
        policy.Egress![0].Action.Should().Be(NetworkRuleAction.Allow);
        policy.Egress[0].Target.Should().Be("pypi.org");
    }

    [Fact]
    public async Task PatchRulesAsync_ShouldSendRulesBody()
    {
        var handler = new CapturingHandler("{}");
        var adapter = CreateAdapter(handler, "fsb-1");

        await adapter.PatchRulesAsync(
        [
            new NetworkRule { Action = NetworkRuleAction.Allow, Target = "www.github.com" }
        ]);

        handler.Method.Should().Be(HttpMethod.Patch);
        handler.PathAndQuery.Should().Be("/v1/sandboxes/fsb-1/networkpolicy");
        handler.RequestBody.Should().NotBeNullOrEmpty();
        using var json = JsonDocument.Parse(handler.RequestBody!);
        var rule = json.RootElement[0];
        rule.GetProperty("action").GetString().Should().Be("allow");
        rule.GetProperty("target").GetString().Should().Be("www.github.com");
    }

    [Fact]
    public async Task DeleteRulesAsync_ShouldSendTargetsBody()
    {
        var handler = new CapturingHandler("{}");
        var adapter = CreateAdapter(handler, "fsb-1");

        await adapter.DeleteRulesAsync(["www.github.com", "*.blocked.org"]);

        handler.Method.Should().Be(HttpMethod.Delete);
        handler.PathAndQuery.Should().Be("/v1/sandboxes/fsb-1/networkpolicy");
        handler.RequestBody.Should().NotBeNullOrEmpty();
        using var json = JsonDocument.Parse(handler.RequestBody!);
        json.RootElement.GetArrayLength().Should().Be(2);
        json.RootElement[0].GetString().Should().Be("www.github.com");
        json.RootElement[1].GetString().Should().Be("*.blocked.org");
    }

    [Fact]
    public async Task CreateVaultAsync_ShouldPostToLifecycleVaultPath()
    {
        var handler = new CapturingHandler("""{"revision":1,"credentials":[],"bindings":[]}""");
        var adapter = CreateAdapter(handler, "fsb-1");

        var state = await adapter.CreateAsync(
        [
            new Credential
            {
                Name = "tok",
                Source = new InlineCredentialSource { Type = "inline", Value = "sekrit" }
            }
        ],
        []);

        handler.Method.Should().Be(HttpMethod.Post);
        handler.PathAndQuery.Should().Be("/v1/sandboxes/fsb-1/credential-vault");
        handler.RequestBody.Should().Contain("\"source\":{\"type\":\"inline\",\"value\":\"sekrit\"}");
        state.Revision.Should().Be(1);
    }

    [Fact]
    public async Task GetVaultAsync_ShouldGetSanitizedState()
    {
        // A non-conforming upstream echoes the write-only value inside the
        // credential metadata; the parsed model must drop it.
        const string payload = """{"revision":2,"credentials":[{"name":"tok","sourceType":"inline","revision":2,"source":{"type":"inline","value":"sekrit"}}],"bindings":[]}""";
        var handler = new CapturingHandler(payload);
        var adapter = CreateAdapter(handler, "fsb-1");

        var state = await adapter.GetAsync();

        handler.Method.Should().Be(HttpMethod.Get);
        handler.PathAndQuery.Should().Be("/v1/sandboxes/fsb-1/credential-vault");
        state.Revision.Should().Be(2);
        state.Credentials.Should().ContainSingle();
        state.Credentials[0].SourceType.Should().Be("inline");
        System.Text.Json.JsonSerializer.Serialize(state).Should().NotContain("sekrit");
    }

    [Fact]
    public async Task PatchVaultAsync_ShouldSurface412WithStatusCode()
    {
        var handler = new CapturingHandler(
            """{"code":"FSB::API_ERROR","message":"credential vault revision conflict"}""",
            statusCode: 412);
        var adapter = CreateAdapter(handler, "fsb-1");

        var act = async () => await adapter.PatchAsync(new CredentialVaultPatchRequest { ExpectedRevision = 2 });

        var exception = (SandboxApiException)(await act.Should()
            .ThrowAsync<SandboxApiException>()).Which;
        exception.StatusCode.Should().Be(412);
        handler.Method.Should().Be(HttpMethod.Patch);
        handler.PathAndQuery.Should().Be("/v1/sandboxes/fsb-1/credential-vault");
    }

    [Fact]
    public async Task ListCredentialsAsync_ShouldGetCredentialsSubresource()
    {
        const string payload = """{"revision":2,"credentials":[{"name":"tok","sourceType":"inline","revision":2}]}""";
        var handler = new CapturingHandler(payload);
        var adapter = CreateAdapter(handler, "fsb-1");

        var credentials = await adapter.ListCredentialsAsync();

        handler.Method.Should().Be(HttpMethod.Get);
        handler.PathAndQuery.Should().Be("/v1/sandboxes/fsb-1/credential-vault/credentials");
        credentials.Should().ContainSingle();
        credentials[0].Name.Should().Be("tok");
    }

    [Fact]
    public async Task GetBindingAsync_ShouldEncodeNameAndParseMetadata()
    {
        const string payload = """{"name":"ext","revision":2,"match":{"hosts":["ext.test"]},"auth":{"type":"apiKey","name":"X-Api-Key"}}""";
        var handler = new CapturingHandler(payload);
        var adapter = CreateAdapter(handler, "fsb-1");

        var binding = await adapter.GetBindingAsync("ext/primary");

        handler.Method.Should().Be(HttpMethod.Get);
        handler.PathAndQuery.Should().Be("/v1/sandboxes/fsb-1/credential-vault/bindings/ext%2Fprimary");
        binding.Name.Should().Be("ext");
        binding.Match!.Hosts.Should().ContainSingle().Which.Should().Be("ext.test");
        binding.Auth!.Type.Should().Be("apiKey");
        binding.Auth.Name.Should().Be("X-Api-Key");
    }

    [Fact]
    public async Task PatchVaultAsync_ShouldSendMutationBody()
    {
        var handler = new CapturingHandler("""{"revision":3,"credentials":[],"bindings":[]}""");
        var adapter = CreateAdapter(handler, "fsb-1");

        var request = new CredentialVaultPatchRequest
        {
            ExpectedRevision = 2,
            Credentials = new CredentialMutationSet
            {
                Delete = ["tok"]
            }
        };
        var state = await adapter.PatchAsync(request);

        handler.Method.Should().Be(HttpMethod.Patch);
        handler.PathAndQuery.Should().Be("/v1/sandboxes/fsb-1/credential-vault");
        using var json = JsonDocument.Parse(handler.RequestBody!);
        json.RootElement.GetProperty("expectedRevision").GetInt32().Should().Be(2);
        json.RootElement.GetProperty("credentials").GetProperty("delete")[0].GetString().Should().Be("tok");
        state.Revision.Should().Be(3);
    }

    [Fact]
    public async Task DeleteVaultAsync_ShouldDeleteVaultPath()
    {
        var handler = new CapturingHandler("");
        var adapter = CreateAdapter(handler, "fsb-1");

        await adapter.DeleteAsync();

        handler.Method.Should().Be(HttpMethod.Delete);
        handler.PathAndQuery.Should().Be("/v1/sandboxes/fsb-1/credential-vault");
    }

    [Theory]
    [InlineData("tok/primary", "/v1/sandboxes/fsb-1/credential-vault/credentials/tok%2Fprimary")]
    [InlineData("api key", "/v1/sandboxes/fsb-1/credential-vault/credentials/api%20key")]
    [InlineData("100%", "/v1/sandboxes/fsb-1/credential-vault/credentials/100%25")]
    public async Task GetCredentialAsync_ShouldEncodeSubresourceNames(
        string name,
        string expectedPath)
    {
        var handler = new CapturingHandler("""{"name":"x","sourceType":"inline","revision":1}""");
        var adapter = CreateAdapter(handler, "fsb-1");

        await adapter.GetCredentialAsync(name);

        handler.Method.Should().Be(HttpMethod.Get);
        handler.PathAndQuery.Should().Be(expectedPath);
    }

    [Fact]
    public async Task ListBindingsAsync_ShouldGetBindingsSubresource()
    {
        const string payload = """{"revision":2,"bindings":[{"name":"b","revision":2}]}""";
        var handler = new CapturingHandler(payload);
        var adapter = CreateAdapter(handler, "fsb-1");

        var bindings = await adapter.ListBindingsAsync();

        handler.Method.Should().Be(HttpMethod.Get);
        handler.PathAndQuery.Should().Be("/v1/sandboxes/fsb-1/credential-vault/bindings");
        bindings.Should().ContainSingle();
        bindings[0].Name.Should().Be("b");
    }

    private static NetworkPolicyAdapter CreateAdapter(CapturingHandler handler, string sandboxId)
    {
        var client = new HttpClient(handler);
        var wrapper = new HttpClientWrapper(client, "http://localhost:8080/v1");
        return new NetworkPolicyAdapter(wrapper, sandboxId);
    }

    private sealed class CapturingHandler(string payload, int statusCode = 200) : HttpMessageHandler
    {
        public HttpMethod? Method { get; private set; }

        public string? PathAndQuery { get; private set; }

        public string? RequestBody { get; private set; }

        protected override async Task<HttpResponseMessage> SendAsync(
            HttpRequestMessage request,
            CancellationToken cancellationToken)
        {
            Method = request.Method;
            PathAndQuery = request.RequestUri?.PathAndQuery;
            RequestBody = request.Content is null
                ? null
                : await request.Content.ReadAsStringAsync();
            var response = new HttpResponseMessage((HttpStatusCode)statusCode)
            {
                Content = new StringContent(payload, Encoding.UTF8, "application/json")
            };
            return response;
        }
    }
}
