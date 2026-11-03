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
using OpenSandbox.Internal;
using OpenSandbox.Models;
using Xunit;

namespace OpenSandbox.Tests;

public class EgressAdapterCredentialVaultTests
{
    [Fact]
    public async Task CreateAsync_ShouldSendCredentialVaultPayloadAndEndpointHeaders()
    {
        var handler = new CaptureHandler(_ => CredentialVaultStateResponse());
        var adapter = CreateAdapter(handler, new Dictionary<string, string>
        {
            ["X-Global"] = "global",
            ["OPENSANDBOX-EGRESS-AUTH"] = "egress-token"
        });

        var state = await adapter.CreateAsync(
            [
                new Credential
                {
                    Name = "api-token",
                    Source = new InlineCredentialSource { Value = "test-token" }
                }
            ],
            [
                new CredentialBinding
                {
                    Name = "api-binding",
                    Match = new CredentialMatch
                    {
                        Hosts = ["api.example.com"],
                        Schemes = ["https"],
                        Methods = ["GET"],
                        Paths = ["/v1/*"]
                    },
                    Auth = new CredentialAuth
                    {
                        Type = "apiKey",
                        Name = "X-Test-Key",
                        Credential = "api-token",
                        Substitutions =
                        [
                            new CredentialSubstitution
                            {
                                Credential = "api-token",
                                Placeholder = "__api_token__",
                                In = ["query", "body"]
                            }
                        ]
                    }
                }
            ]);

        handler.Requests.Should().ContainSingle();
        var request = handler.Requests[0];
        request.Method.Should().Be(HttpMethod.Post);
        request.PathAndQuery.Should().Be("/credential-vault");
        request.Headers.Should().Contain("X-Global", "global");
        request.Headers.Should().Contain("OPENSANDBOX-EGRESS-AUTH", "egress-token");

        using var json = JsonDocument.Parse(request.Body!);
        var root = json.RootElement;
        root.GetProperty("credentials")[0].GetProperty("name").GetString().Should().Be("api-token");
        root.GetProperty("credentials")[0].GetProperty("source").GetProperty("type").GetString().Should().Be("inline");
        root.GetProperty("credentials")[0].GetProperty("source").GetProperty("value").GetString().Should().Be("test-token");
        root.GetProperty("bindings")[0].GetProperty("auth").GetProperty("type").GetString().Should().Be("apiKey");
        root.GetProperty("bindings")[0].GetProperty("auth").GetProperty("name").GetString().Should().Be("X-Test-Key");
        root.GetProperty("bindings")[0].GetProperty("auth").GetProperty("substitutions")[0].GetProperty("placeholder").GetString().Should().Be("__api_token__");
        root.GetProperty("bindings")[0].GetProperty("auth").GetProperty("substitutions")[0].GetProperty("in")[1].GetString().Should().Be("body");
        root.GetProperty("bindings")[0].GetProperty("match").GetProperty("hosts")[0].GetString().Should().Be("api.example.com");
        state.Revision.Should().Be(3);
    }

    [Fact]
    public async Task PatchAsync_ShouldSendExpectedRevisionAndMutationSets()
    {
        var handler = new CaptureHandler(_ => CredentialVaultStateResponse(revision: 4));
        var adapter = CreateAdapter(handler);

        var state = await adapter.PatchAsync(new CredentialVaultPatchRequest
        {
            ExpectedRevision = 3,
            Credentials = new CredentialMutationSet
            {
                Add =
                [
                    new Credential
                    {
                        Name = "replacement-token",
                        Source = new InlineCredentialSource { Value = "replacement-value" }
                    }
                ],
                Delete = ["old-token"]
            },
            Bindings = new CredentialBindingMutationSet
            {
                Delete = ["old-binding"]
            }
        });

        handler.Requests.Should().ContainSingle();
        var request = handler.Requests[0];
        request.Method.Should().Be(HttpMethod.Patch);
        request.PathAndQuery.Should().Be("/credential-vault");

        using var json = JsonDocument.Parse(request.Body!);
        var root = json.RootElement;
        root.GetProperty("expectedRevision").GetInt32().Should().Be(3);
        root.GetProperty("credentials").GetProperty("add")[0].GetProperty("name").GetString().Should().Be("replacement-token");
        root.GetProperty("credentials").GetProperty("delete")[0].GetString().Should().Be("old-token");
        root.GetProperty("bindings").GetProperty("delete")[0].GetString().Should().Be("old-binding");
        state.Revision.Should().Be(4);
    }

    [Fact]
    public async Task ListGetAndDeleteAsync_ShouldUseCredentialVaultRoutes()
    {
        var handler = new CaptureHandler(request =>
        {
            return request.Method.Method switch
            {
                "GET" when request.RequestUri!.PathAndQuery == "/credential-vault/credentials" => """
                {
                  "revision": 5,
                  "credentials": [
                    { "name": "api-token", "sourceType": "inline", "revision": 1 }
                  ]
                }
                """,
                "GET" when request.RequestUri!.PathAndQuery == "/credential-vault/credentials/api%2Ftoken" => """
                { "name": "api/token", "sourceType": "inline", "revision": 2 }
                """,
                "GET" when request.RequestUri!.PathAndQuery == "/credential-vault/bindings" => """
                {
                  "revision": 5,
                  "bindings": [
                    { "name": "api-binding", "revision": 1, "auth": { "type": "bearer" } }
                  ]
                }
                """,
                "GET" when request.RequestUri!.PathAndQuery == "/credential-vault/bindings/api%20binding" => """
                { "name": "api binding", "revision": 2, "auth": { "type": "bearer" } }
                """,
                _ => "{}"
            };
        });
        handler.StatusCodeSelector = request =>
            request.Method == HttpMethod.Delete ? HttpStatusCode.NoContent : HttpStatusCode.OK;
        var adapter = CreateAdapter(handler);

        var credentials = await adapter.ListCredentialsAsync();
        var credential = await adapter.GetCredentialAsync("api/token");
        var bindings = await adapter.ListBindingsAsync();
        var binding = await adapter.GetBindingAsync("api binding");
        await adapter.DeleteAsync();

        credentials.Should().ContainSingle().Which.Name.Should().Be("api-token");
        credential.Name.Should().Be("api/token");
        bindings.Should().ContainSingle().Which.Auth!.Type.Should().Be("bearer");
        binding.Name.Should().Be("api binding");
        handler.Requests.Select(r => r.PathAndQuery).Should().Equal(
            "/credential-vault/credentials",
            "/credential-vault/credentials/api%2Ftoken",
            "/credential-vault/bindings",
            "/credential-vault/bindings/api%20binding",
            "/credential-vault");
        handler.Requests.Last().Method.Should().Be(HttpMethod.Delete);
    }

    [Fact]
    public async Task GetAsync_ShouldParseSanitizedStateWithoutCredentialValues()
    {
        var handler = new CaptureHandler(_ => """
        {
          "revision": 7,
          "credentials": [
            {
              "name": "api-token",
              "sourceType": "inline",
              "revision": 3,
              "source": { "type": "inline", "value": "server-should-not-return-values" }
            }
          ],
          "bindings": [
            {
              "name": "api-binding",
              "revision": 4,
              "match": { "hosts": ["api.example.com"] },
              "auth": { "type": "apiKey", "name": "X-Test-Key" }
            }
          ]
        }
        """);
        var adapter = CreateAdapter(handler);

        var state = await adapter.GetAsync();

        state.Revision.Should().Be(7);
        state.Credentials.Should().ContainSingle().Which.SourceType.Should().Be("inline");
        state.Bindings.Should().ContainSingle().Which.Auth!.Name.Should().Be("X-Test-Key");
        JsonSerializer.Serialize(state).Should().NotContain("server-should-not-return-values");
    }

    [Fact]
    public async Task CreateAsync_ShouldSendExchangeSourceAndTypelessAuthAndParseSanitizedState()
    {
        var handler = new CaptureHandler(_ => """
        {
          "revision": 3,
          "credentials": [
            {
              "name": "spi-signer",
              "sourceType": "exchange",
              "revision": 3,
              "source": { "type": "exchange", "endpoint": "unix:///run/spi/spi.sock" }
            },
            { "name": "corp-resolver", "sourceType": "exchange", "revision": 3 }
          ],
          "bindings": [
            {
              "name": "internal-api",
              "revision": 3,
              "match": { "hosts": ["*.internal.example.com"] },
              "auth": { "credential": "spi-signer" }
            }
          ]
        }
        """);
        var adapter = CreateAdapter(handler);

        var state = await adapter.CreateAsync(
            [
                new Credential
                {
                    Name = "spi-signer",
                    Source = new ExchangeCredentialSource("unix:///run/spi/spi.sock")
                },
                new Credential
                {
                    Name = "corp-resolver",
                    Source = new ExchangeCredentialSource { Endpoint = "https://resolver.internal:8443/prefix" }
                }
            ],
            [
                new CredentialBinding
                {
                    Name = "internal-api",
                    Match = new CredentialMatch { Hosts = ["*.internal.example.com"] },
                    Auth = CredentialAuth.Exchange("spi-signer")
                }
            ]);

        handler.Requests.Should().ContainSingle();
        var body = handler.Requests[0].Body!;
        body.Should().NotContain("\"type\":null");
        using var json = JsonDocument.Parse(body);
        var root = json.RootElement;
        var sources = root.GetProperty("credentials").EnumerateArray().Select(c => c.GetProperty("source")).ToList();
        JsonSerializer.Serialize(sources[0], (JsonSerializerOptions?)null).Should().Be(
            "{\"type\":\"exchange\",\"endpoint\":\"unix:///run/spi/spi.sock\"}");
        JsonSerializer.Serialize(sources[1], (JsonSerializerOptions?)null).Should().Be(
            "{\"type\":\"exchange\",\"endpoint\":\"https://resolver.internal:8443/prefix\"}");
        var auth = root.GetProperty("bindings")[0].GetProperty("auth");
        auth.EnumerateObject().Select(p => p.Name).Should().Equal("credential");
        auth.GetProperty("credential").GetString().Should().Be("spi-signer");

        state.Revision.Should().Be(3);
        state.Credentials.Should().HaveCount(2).And.OnlyContain(c => c.SourceType == "exchange");
        state.Bindings.Should().ContainSingle().Which.Auth!.Type.Should().BeNull();
        state.Bindings[0].Auth!.Credential.Should().Be("spi-signer");
        JsonSerializer.Serialize(state).Should().NotContain("spi.sock");
    }

    [Fact]
    public async Task GetAsync_ShouldRecoverLegacyTypeNullSkewWithoutSurfacingTypeNull()
    {
        var handler = new CaptureHandler(_ => """
        {
          "revision": 3,
          "credentials": [],
          "bindings": [
            {
              "name": "internal-api",
              "revision": 3,
              "match": { "hosts": ["*.internal.example.com"] },
              "auth": { "type": null, "credential": "spi-signer" }
            }
          ]
        }
        """);
        var adapter = CreateAdapter(handler);

        var state = await adapter.GetAsync();

        state.Bindings.Should().ContainSingle().Which.Auth!.Type.Should().BeNull();
        state.Bindings[0].Auth!.Credential.Should().Be("spi-signer");
        JsonSerializer.Serialize(state).Should().NotContain("\"type\":null");
    }

    [Fact]
    public async Task CreateAsync_ShouldRejectIllegalMixedVaultShapes()
    {
        var handler = new CaptureHandler(_ => CredentialVaultStateResponse());
        var adapter = CreateAdapter(handler);
        var match = new CredentialMatch { Hosts = ["*.internal.example.com"] };

        var exchangeWithValue = new Credential
        {
            Name = "spi-signer",
            Source = new CredentialSource { Type = "exchange", Endpoint = "unix:///run/spi/spi.sock", Value = "secret" }
        };
        var act = () => adapter.CreateAsync([exchangeWithValue], []);
        await act.Should().ThrowAsync<ArgumentException>();

        var inlineWithEndpoint = new Credential
        {
            Name = "tok",
            Source = new CredentialSource { Type = "inline", Value = "secret", Endpoint = "unix:///x" }
        };
        act = () => adapter.CreateAsync([inlineWithEndpoint], []);
        await act.Should().ThrowAsync<ArgumentException>();

        var unknownSourceType = new Credential
        {
            Name = "tok",
            Source = new CredentialSource { Type = "voucher", Value = "secret" }
        };
        act = () => adapter.CreateAsync([unknownSourceType], []);
        await act.Should().ThrowAsync<ArgumentException>();

        var typelessWithName = new CredentialBinding
        {
            Name = "internal-api",
            Match = match,
            Auth = new CredentialAuth { Credential = "spi-signer", Name = "X-Api-Key" }
        };
        act = () => adapter.CreateAsync([], [typelessWithName]);
        await act.Should().ThrowAsync<ArgumentException>();

        var emptyAuth = new CredentialBinding
        {
            Name = "internal-api",
            Match = match,
            Auth = new CredentialAuth()
        };
        act = () => adapter.CreateAsync([], [emptyAuth]);
        await act.Should().ThrowAsync<ArgumentException>();

        handler.Requests.Should().BeEmpty("illegal shapes must fail before reaching the wire");
    }

    [Fact]
    public async Task PatchAsync_ShouldRejectIllegalMixedVaultShapes()
    {
        var handler = new CaptureHandler(_ => CredentialVaultStateResponse());
        var adapter = CreateAdapter(handler);

        var act = () => adapter.PatchAsync(new CredentialVaultPatchRequest
        {
            Bindings = new CredentialBindingMutationSet
            {
                Add =
                [
                    new CredentialBinding
                    {
                        Name = "internal-api",
                        Match = new CredentialMatch { Hosts = ["*.internal.example.com"] },
                        Auth = new CredentialAuth { Credential = "spi-signer", Substitutions = [] }
                    }
                ]
            }
        });

        await act.Should().ThrowAsync<ArgumentException>();
        handler.Requests.Should().BeEmpty();
    }

    [Fact]
    public void VaultSourceAndAuthSerialization_ShouldKeepStaticBytesAndOmitNullType()
    {
        JsonSerializer.Serialize(new InlineCredentialSource { Value = "dummy-inline-value" }).Should().Be(
            "{\"type\":\"inline\",\"value\":\"dummy-inline-value\"}");
        JsonSerializer.Serialize(CredentialSource.Inline("dummy-inline-value")).Should().Be(
            "{\"type\":\"inline\",\"value\":\"dummy-inline-value\"}");
        JsonSerializer.Serialize(new ExchangeCredentialSource { Endpoint = "unix:///run/spi/spi.sock" }).Should().Be(
            "{\"type\":\"exchange\",\"endpoint\":\"unix:///run/spi/spi.sock\"}");
        JsonSerializer.Serialize(CredentialSource.Exchange("unix:///run/spi/spi.sock")).Should().Be(
            "{\"type\":\"exchange\",\"endpoint\":\"unix:///run/spi/spi.sock\"}");

        JsonSerializer.Serialize(CredentialAuth.Exchange("spi-signer")).Should().Be(
            "{\"credential\":\"spi-signer\"}");
        JsonSerializer.Serialize(new CredentialAuth { Type = "bearer", Credential = "tok" }).Should().Be(
            "{\"type\":\"bearer\",\"credential\":\"tok\"}");
        JsonSerializer.Serialize(new CredentialAuthMetadata { Type = null, Credential = "spi-signer" }).Should().Be(
            "{\"credential\":\"spi-signer\"}");
        JsonSerializer.Serialize(new CredentialAuthMetadata { Type = "apiKey", Name = "X-Test-Key" }).Should().Be(
            "{\"type\":\"apiKey\",\"name\":\"X-Test-Key\"}");
    }

    private static EgressAdapter CreateAdapter(
        HttpMessageHandler handler,
        IReadOnlyDictionary<string, string>? headers = null)
    {
        var client = new HttpClient(handler);
        var wrapper = new HttpClientWrapper(client, "http://egress.local", headers);
        return new EgressAdapter(wrapper);
    }

    private static string CredentialVaultStateResponse(int revision = 3)
    {
        return $$"""
        {
          "revision": {{revision}},
          "credentials": [
            { "name": "api-token", "sourceType": "inline", "revision": 1 }
          ],
          "bindings": [
            {
              "name": "api-binding",
              "revision": 1,
              "match": { "hosts": ["api.example.com"] },
              "auth": { "type": "apiKey", "name": "X-Test-Key" }
            }
          ]
        }
        """;
    }

    private sealed class CaptureHandler(Func<HttpRequestMessage, string> payloadSelector) : HttpMessageHandler
    {
        public List<CapturedRequest> Requests { get; } = [];

        public Func<HttpRequestMessage, HttpStatusCode> StatusCodeSelector { get; set; } = _ => HttpStatusCode.OK;

        protected override async Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken cancellationToken)
        {
            var body = request.Content == null
                ? null
                : await request.Content.ReadAsStringAsync().ConfigureAwait(false);
            Requests.Add(new CapturedRequest(
                request.Method,
                request.RequestUri?.PathAndQuery,
                request.Headers.ToDictionary(header => header.Key, header => string.Join(",", header.Value)),
                body));

            var statusCode = StatusCodeSelector(request);
            var response = new HttpResponseMessage(statusCode);
            if (statusCode != HttpStatusCode.NoContent)
            {
                response.Content = new StringContent(payloadSelector(request), Encoding.UTF8, "application/json");
            }

            return response;
        }
    }

    private sealed record CapturedRequest(
        HttpMethod Method,
        string? PathAndQuery,
        IReadOnlyDictionary<string, string> Headers,
        string? Body);
}
