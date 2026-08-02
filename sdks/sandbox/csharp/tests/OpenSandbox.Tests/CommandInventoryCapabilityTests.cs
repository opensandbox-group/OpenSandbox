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

using System.Net;
using System.Text;
using System.Text.Json;
using FluentAssertions;
using Microsoft.Extensions.Logging.Abstractions;
using Moq;
using OpenSandbox.Adapters;
using OpenSandbox.Config;
using OpenSandbox.Core;
using OpenSandbox.Factory;
using OpenSandbox.Internal;
using OpenSandbox.Models;
using OpenSandbox.Services;
using Xunit;

namespace OpenSandbox.Tests;

public class CommandInventoryCapabilityTests
{
    [Fact]
    public async Task ConnectAsync_ShouldKeepLegacyCustomCommandsStubCompatible_WhenFactoryDoesNotProvideInventory()
    {
        var commands = new LegacyCommandsStub();
        var factory = new LegacyCommandsAdapterFactory(commands);

        await using var sandbox = await Sandbox.ConnectAsync(new SandboxConnectOptions
        {
            SandboxId = "sbx-command-inventory",
            ConnectionConfig = new ConnectionConfig(new ConnectionConfigOptions
            {
                Domain = "localhost:8080"
            }),
            AdapterFactory = factory,
            SkipHealthCheck = true
        });

        sandbox.Commands.Should().BeSameAs(commands);
        sandbox.CommandInventory.Should().BeNull();
        factory.CreateExecdStackCallCount.Should().Be(1);
    }

    [Fact]
    public async Task ConnectAsync_ShouldUseInventoryCapabilityImplementedByCommandsWhenStackCapabilityIsOmitted()
    {
        var commands = new CommandsAndInventoryStub();
        var factory = new LegacyCommandsAdapterFactory(commands);

        await using var sandbox = await Sandbox.ConnectAsync(new SandboxConnectOptions
        {
            SandboxId = "sbx-command-inventory-fallback",
            ConnectionConfig = new ConnectionConfig(new ConnectionConfigOptions
            {
                Domain = "localhost:8080"
            }),
            AdapterFactory = factory,
            SkipHealthCheck = true
        });

        sandbox.Commands.Should().BeSameAs(commands);
        sandbox.CommandInventory.Should().BeSameAs(commands);
    }

    [Fact]
    public void DefaultAdapterFactory_ShouldExposeCommandInventoryCapability()
    {
        var connectionConfig = new ConnectionConfig(new ConnectionConfigOptions { Domain = "localhost:8080" });
        using var provider = new HttpClientProvider(connectionConfig, NullLoggerFactory.Instance);
        var factory = DefaultAdapterFactory.Create();

        var stack = factory.CreateExecdStack(new CreateExecdStackOptions
        {
            ConnectionConfig = connectionConfig,
            ExecdBaseUrl = "http://execd.local",
            HttpClientProvider = provider,
            LoggerFactory = NullLoggerFactory.Instance
        });

        stack.CommandInventory.Should().NotBeNull();
        stack.CommandInventory.Should().BeSameAs(stack.Commands);
    }

    [Fact]
    public async Task ListCommandsAsync_ShouldGetCommandEndpointWithoutQueryByDefault()
    {
        var handler = new StubHttpMessageHandler((request, _) =>
        {
            request.Method.Should().Be(HttpMethod.Get);
            request.RequestUri!.AbsolutePath.Should().Be("/command");
            request.RequestUri.Query.Should().BeEmpty();
            return JsonResponse(RunningPageJson());
        });
        IExecdCommandInventory inventory = CreateAdapter(handler);

        var page = await inventory.ListCommandsAsync();

        page.Commands.Should().ContainSingle().Which.Should().BeOfType<RunningCommandSummary>();
        handler.RequestUris.Should().ContainSingle().Which.Should().Be("http://execd.local/command");
    }

    [Theory]
    [InlineData(true, "?running=true")]
    [InlineData(false, "?running=false")]
    public async Task ListCommandsAsync_ShouldEncodeRunningFilter(bool running, string expectedQuery)
    {
        var handler = new StubHttpMessageHandler((request, _) =>
        {
            request.RequestUri!.Query.Should().Be(expectedQuery);
            return JsonResponse(RunningPageJson());
        });
        IExecdCommandInventory inventory = CreateAdapter(handler);

        await inventory.ListCommandsAsync(running);
    }

    [Fact]
    public async Task ListCommandsAsync_ShouldEncodeOpaqueCursorWithoutInterpretingIt()
    {
        var handler = new StubHttpMessageHandler((request, _) =>
        {
            request.RequestUri!.Query.Should().Be("?cursor=opaque%2B%2F%3Dtoken%3Fvalue");
            return JsonResponse(RunningPageJson());
        });
        IExecdCommandInventory inventory = CreateAdapter(handler);

        await inventory.ListCommandsAsync(cursor: "opaque+/=token?value");
    }

    [Theory]
    [InlineData("")]
    [InlineData(" \t ")]
    public async Task ListCommandsAsync_ShouldOmitBlankCursor(string cursor)
    {
        var handler = new StubHttpMessageHandler((request, _) =>
        {
            request.RequestUri!.Query.Should().BeEmpty();
            return JsonResponse(RunningPageJson());
        });
        IExecdCommandInventory inventory = CreateAdapter(handler);

        await inventory.ListCommandsAsync(cursor: cursor);
    }

    [Fact]
    public async Task ListCommandsAsync_ShouldIncludeNonDefaultLimit()
    {
        var handler = new StubHttpMessageHandler((request, _) =>
        {
            request.RequestUri!.Query.Should().Be("?limit=25");
            return JsonResponse(RunningPageJson());
        });
        IExecdCommandInventory inventory = CreateAdapter(handler);

        await inventory.ListCommandsAsync(limit: 25);
    }

    [Fact]
    public async Task ListCommandsAsync_ShouldPropagateCancellationToken()
    {
        CancellationToken received = default;
        var handler = new StubHttpMessageHandler((_, cancellationToken) =>
        {
            received = cancellationToken;
            return JsonResponse(RunningPageJson());
        });
        IExecdCommandInventory inventory = CreateAdapter(handler);
        using var cancellation = new CancellationTokenSource();

        await inventory.ListCommandsAsync(cancellationToken: cancellation.Token);

        received.Should().Be(cancellation.Token);
    }

    [Fact]
    public async Task ListCommandsAsync_ShouldPreserveHttpClientWrapperInvalidQueryErrorMapping()
    {
        var handler = new StubHttpMessageHandler((_, _) =>
        {
            var response = new HttpResponseMessage(HttpStatusCode.BadRequest)
            {
                Content = new StringContent("""
                    { "code": "INVALID_QUERY", "message": "invalid cursor" }
                    """, Encoding.UTF8, "application/json")
            };
            response.Headers.Add("x-request-id", "request-inventory-400");
            return Task.FromResult(response);
        });
        IExecdCommandInventory inventory = CreateAdapter(handler);

        var act = () => inventory.ListCommandsAsync(cursor: "invalid");

        var exception = await act.Should().ThrowAsync<SandboxApiException>();
        exception.Which.StatusCode.Should().Be(400);
        exception.Which.Error.Code.Should().Be("INVALID_QUERY");
        exception.Which.Message.Should().Be("invalid cursor");
        exception.Which.RequestId.Should().Be("request-inventory-400");
    }

    [Fact]
    public async Task ListCommandsAsync_ShouldAllowEnvelopeAndPaginationExtensions()
    {
        var handler = new StubHttpMessageHandler((_, _) => JsonResponse("""
            {
              "commands": [{
                "session": "cmd-running",
                "running": true,
                "background": true,
                "started_at": "2026-07-24T10:00:00Z"
              }],
              "pagination": {
                "limit": 50,
                "inventory_extension": { "enabled": true }
              },
              "page_extension": ["future"]
            }
            """));
        IExecdCommandInventory inventory = CreateAdapter(handler);

        var page = await inventory.ListCommandsAsync();

        page.Commands.Should().ContainSingle();
        page.Pagination.Limit.Should().Be(50);
    }

    [Fact]
    public async Task ListCommandsAsync_ShouldDeserializeMixedPageAndPreserveNonemptyCursor()
    {
        var handler = new StubHttpMessageHandler((_, _) => JsonResponse("""
            {
              "commands": [
                {
                  "session": "cmd-running",
                  "running": true,
                  "background": true,
                  "started_at": "2026-07-24T10:00:00Z"
                },
                {
                  "session": "cmd-terminal",
                  "running": false,
                  "background": false,
                  "started_at": "2026-07-24T10:00:00Z",
                  "finished_at": "2026-07-24T10:01:00Z",
                  "exit_code": 7
                }
              ],
              "pagination": { "limit": 2, "nextCursor": "opaque+/=cursor" }
            }
            """));
        IExecdCommandInventory inventory = CreateAdapter(handler);

        var page = await inventory.ListCommandsAsync();

        page.Commands.Should().HaveCount(2);
        page.Commands[0].Should().BeOfType<RunningCommandSummary>();
        page.Commands[1].Should().BeOfType<TerminalCommandSummary>();
        page.Pagination.NextCursor.Should().Be("opaque+/=cursor");
    }

    [Fact]
    public async Task ListCommandsAsync_ShouldPreserveNonemptyNextCursor()
    {
        var handler = new StubHttpMessageHandler((_, _) => JsonResponse("""
            {
              "commands": [],
              "pagination": { "limit": 50, "nextCursor": "opaque+/=cursor" }
            }
            """));
        IExecdCommandInventory inventory = CreateAdapter(handler);

        var page = await inventory.ListCommandsAsync();

        page.Pagination.NextCursor.Should().Be("opaque+/=cursor");
    }

    [Fact]
    public async Task ListCommandsAsync_ShouldRejectNullNextCursor()
    {
        var handler = new StubHttpMessageHandler((_, _) => JsonResponse("""
            {
              "commands": [],
              "pagination": { "limit": 50, "nextCursor": null }
            }
            """));
        IExecdCommandInventory inventory = CreateAdapter(handler);

        var act = () => inventory.ListCommandsAsync();

        await act.Should().ThrowAsync<SandboxApiException>();
    }

    [Theory]
    [InlineData("")]
    [InlineData("   ")]
    public async Task ListCommandsAsync_ShouldRejectBlankNextCursor(string nextCursor)
    {
        var serializedNextCursor = JsonSerializer.Serialize(nextCursor);
        var handler = new StubHttpMessageHandler((_, _) => JsonResponse($"""
            {{
              "commands": [],
              "pagination": {{ "limit": 50, "nextCursor": {serializedNextCursor} }}
            }}
            """));
        IExecdCommandInventory inventory = CreateAdapter(handler);

        var act = () => inventory.ListCommandsAsync();

        await act.Should().ThrowAsync<SandboxApiException>();
    }

    [Theory]
    [InlineData("{\"commands\":null,\"pagination\":{\"limit\":50}}")]
    [InlineData("{\"commands\":[],\"pagination\":null}")]
    [InlineData("{\"pagination\":{\"limit\":50}}")]
    [InlineData("{\"commands\":[],\"pagination\":{\"limit\":null}}")]
    [InlineData("{\"commands\":[],\"pagination\":{\"limit\":\"50\"}}")]
    public async Task ListCommandsAsync_ShouldRejectMissingNullAndWrongTypeKnownEnvelopeFields(string page)
    {
        var handler = new StubHttpMessageHandler((_, _) => JsonResponse(page));
        IExecdCommandInventory inventory = CreateAdapter(handler);

        var act = () => inventory.ListCommandsAsync();

        await act.Should().ThrowAsync<SandboxApiException>();
    }

    [Fact]
    public void ListCommandsPage_ShouldSerializeAsValidResponseShape()
    {
        var page = new ListCommandsPage
        {
            Commands =
            [
                new RunningCommandSummary
                {
                    Session = "cmd-running",
                    Background = true,
                    StartedAt = DateTimeOffset.Parse("2026-07-24T10:00:00Z")
                },
                new TerminalCommandSummary
                {
                    Session = "cmd-terminal",
                    Background = false,
                    StartedAt = DateTimeOffset.Parse("2026-07-24T10:00:00Z"),
                    FinishedAt = DateTimeOffset.Parse("2026-07-24T10:01:00Z"),
                    ExitCode = null,
                    Error = "interrupted"
                }
            ],
            Pagination = new CommandPagination { Limit = 2, NextCursor = "next" }
        };

        using var document = JsonDocument.Parse(JsonSerializer.Serialize(page));

        document.RootElement.GetProperty("commands")[0].GetProperty("running").GetBoolean().Should().BeTrue();
        document.RootElement.GetProperty("commands")[1].GetProperty("exit_code").ValueKind.Should().Be(JsonValueKind.Null);
        document.RootElement.GetProperty("pagination").GetProperty("nextCursor").GetString().Should().Be("next");
    }

    [Theory]
    [InlineData("{\"running\":false,\"background\":false,\"started_at\":\"2026-07-24T10:00:00Z\",\"finished_at\":\"2026-07-24T10:01:00Z\",\"exit_code\":0}")]
    [InlineData("{\"session\":\"cmd-terminal\",\"running\":false,\"background\":false,\"started_at\":\"2026-07-24T10:00:00Z\",\"exit_code\":0}")]
    [InlineData("{\"session\":\"cmd-terminal\",\"running\":false,\"background\":false,\"started_at\":\"2026-07-24T10:00:00Z\",\"finished_at\":\"2026-07-24T10:01:00Z\"}")]
    [InlineData("{\"session\":null,\"running\":true,\"background\":true,\"started_at\":\"2026-07-24T10:00:00Z\"}")]
    [InlineData("{\"session\":\"cmd\",\"running\":true,\"background\":\"true\",\"started_at\":\"2026-07-24T10:00:00Z\"}")]
    [InlineData("{\"session\":\"cmd\",\"running\":\"true\",\"background\":true,\"started_at\":\"2026-07-24T10:00:00Z\"}")]
    [InlineData("{\"session\":\"cmd\",\"session\":\"duplicate\",\"running\":true,\"background\":true,\"started_at\":\"2026-07-24T10:00:00Z\"}")]
    [InlineData("{\"session\":\"cmd\",\"running\":true,\"background\":true,\"started_at\":\"not-a-date\"}")]
    [InlineData("{\"session\":\"cmd\",\"running\":true,\"background\":true,\"started_at\":\"2026-07-24\"}")]
    [InlineData("{\"session\":\"cmd\",\"running\":true,\"background\":true,\"started_at\":\"07/24/2026 10:00:00\"}")]
    [InlineData("{\"session\":\"cmd\",\"running\":true,\"background\":true,\"started_at\":\"2026-07-24T10:00:00.Z\"}")]
    [InlineData("{\"session\":\"cmd\",\"running\":true,\"background\":true,\"started_at\":\"2026-07-24T10:00:00.1234567890Z\"}")]
    [InlineData("{\"session\":\"cmd\",\"running\":true,\"background\":true,\"started_at\":\"2026-07-24T10:00:00.12+0800\"}")]
    public async Task ListCommandsAsync_ShouldRejectMissingNullDuplicateAndMalformedCommonFields(string summary)
    {
        var handler = new StubHttpMessageHandler((_, _) => JsonResponse(PageWithSummary(summary)));
        IExecdCommandInventory inventory = CreateAdapter(handler);

        var act = () => inventory.ListCommandsAsync();

        await act.Should().ThrowAsync<SandboxApiException>();
    }

    [Fact]
    public async Task ListCommandsAsync_ShouldDeserializeRunningSummaryWithoutTerminalFields()
    {
        var handler = new StubHttpMessageHandler((_, _) => JsonResponse(RunningPageJson()));
        IExecdCommandInventory inventory = CreateAdapter(handler);

        var page = await inventory.ListCommandsAsync();

        var summary = page.Commands.Should().ContainSingle().Which.Should().BeOfType<RunningCommandSummary>().Subject;
        summary.Session.Should().Be("cmd-running");
        summary.Background.Should().BeTrue();
        summary.StartedAt.Should().Be(DateTimeOffset.Parse("2026-07-24T10:00:00Z"));
    }

    [Theory]
    [InlineData("2026-07-24T10:00:00.1Z")]
    [InlineData("2026-07-24T10:00:00.1234567Z")]
    [InlineData("2026-07-24T10:00:00.12345678Z")]
    [InlineData("2026-07-24T10:00:00.123456789Z")]
    [InlineData("2026-07-24T10:00:00.123456789+08:00")]
    public async Task ListCommandsAsync_ShouldAcceptRfc3339NanoTimestamps(string startedAt)
    {
        var handler = new StubHttpMessageHandler((_, _) => JsonResponse(PageWithSummary($"""
            {{
              "session": "cmd-running",
              "running": true,
              "background": true,
              "started_at": "{startedAt}"
            }}
            """)));
        IExecdCommandInventory inventory = CreateAdapter(handler);

        var page = await inventory.ListCommandsAsync();

        page.Commands.Should().ContainSingle().Which.Should().BeOfType<RunningCommandSummary>();
    }

    [Fact]
    public async Task ListCommandsAsync_ShouldTruncateRfc3339NanoToDateTimeOffsetPrecision()
    {
        var handler = new StubHttpMessageHandler((_, _) => JsonResponse(PageWithSummary("""
            {
              "session": "cmd-running",
              "running": true,
              "background": true,
              "started_at": "2026-07-24T10:00:00.123456789Z"
            }
            """)));
        IExecdCommandInventory inventory = CreateAdapter(handler);

        var page = await inventory.ListCommandsAsync();

        var summary = page.Commands.Should().ContainSingle().Which.Should().BeOfType<RunningCommandSummary>().Subject;
        summary.StartedAt.Should().Be(new DateTimeOffset(2026, 7, 24, 10, 0, 0, TimeSpan.Zero).AddTicks(1234567));
    }

    [Fact]
    public async Task ListCommandsAsync_ShouldDeserializeTerminalSummaryWithPresentNullExitCode()
    {
        var handler = new StubHttpMessageHandler((_, _) => JsonResponse(TerminalPageJson()));
        IExecdCommandInventory inventory = CreateAdapter(handler);

        var page = await inventory.ListCommandsAsync();

        var summary = page.Commands.Should().ContainSingle().Which.Should().BeOfType<TerminalCommandSummary>().Subject;
        summary.FinishedAt.Should().Be(DateTimeOffset.Parse("2026-07-24T10:01:00Z"));
        summary.ExitCode.Should().BeNull();
        summary.Error.Should().Be("interrupted");
    }

    [Fact]
    public async Task ListCommandsAsync_ShouldTreatOmittedNextCursorAsFinalPage()
    {
        var handler = new StubHttpMessageHandler((_, _) => JsonResponse(RunningPageJson()));
        IExecdCommandInventory inventory = CreateAdapter(handler);

        var page = await inventory.ListCommandsAsync();

        page.Pagination.Limit.Should().Be(50);
        page.Pagination.NextCursor.Should().BeNull();
    }

    [Theory]
    [InlineData("{\"session\":\"cmd-running\",\"running\":true,\"background\":true,\"started_at\":\"2026-07-24T10:00:00Z\",\"finished_at\":\"2026-07-24T10:01:00Z\"}")]
    [InlineData("{\"session\":\"cmd-terminal\",\"running\":false,\"background\":false,\"started_at\":\"2026-07-24T10:00:00Z\",\"finished_at\":\"2026-07-24T10:01:00Z\",\"exit_code\":null,\"error\":123}")]
    [InlineData("{\"session\":\"cmd-running\",\"running\":true,\"background\":true,\"started_at\":\"2026-07-24T10:00:00Z\",\"unexpected\":true}")]
    public async Task ListCommandsAsync_ShouldRejectInvalidSummaryWireShape(string summary)
    {
        var handler = new StubHttpMessageHandler((_, _) => JsonResponse($"""
            {{
              "commands": [{summary}],
              "pagination": {{ "limit": 50 }}
            }}
            """));
        IExecdCommandInventory inventory = CreateAdapter(handler);

        var act = () => inventory.ListCommandsAsync();

        await act.Should().ThrowAsync<SandboxApiException>();
    }

    private static CommandsAdapter CreateAdapter(HttpMessageHandler httpHandler)
    {
        const string baseUrl = "http://execd.local";
        var headers = new Dictionary<string, string> { ["X-Test"] = "true" };
        var client = new HttpClientWrapper(new HttpClient(httpHandler), baseUrl, headers);
        return new CommandsAdapter(
            client,
            new HttpClient(httpHandler),
            baseUrl,
            headers,
            NullLoggerFactory.Instance.CreateLogger("CommandInventoryCapabilityTests"));
    }

    private static Task<HttpResponseMessage> JsonResponse(string body)
    {
        return Task.FromResult(new HttpResponseMessage(HttpStatusCode.OK)
        {
            Content = new StringContent(body, Encoding.UTF8, "application/json")
        });
    }

    private static string RunningPageJson() => """
        {
          "commands": [
            {
              "session": "cmd-running",
              "running": true,
              "background": true,
              "started_at": "2026-07-24T10:00:00Z"
            }
          ],
          "pagination": { "limit": 50 }
        }
        """;

    private static string TerminalPageJson() => """
        {
          "commands": [
            {
              "session": "cmd-terminal",
              "running": false,
              "background": false,
              "started_at": "2026-07-24T10:00:00Z",
              "finished_at": "2026-07-24T10:01:00Z",
              "exit_code": null,
              "error": "interrupted"
            }
          ],
          "pagination": { "limit": 50 }
        }
        """;

    private static string PageWithSummary(string summary) => $"""
        {{
          "commands": [{summary}],
          "pagination": {{ "limit": 50 }}
        }}
        """;

    private sealed class LegacyCommandsAdapterFactory : IAdapterFactory
    {
        private readonly IExecdCommands _commands;

        public LegacyCommandsAdapterFactory(IExecdCommands commands)
        {
            _commands = commands;
        }

        public int CreateExecdStackCallCount { get; private set; }

        public LifecycleStack CreateLifecycleStack(CreateLifecycleStackOptions options)
        {
            return new LifecycleStack
            {
                Sandboxes = new LegacySandboxesStub()
            };
        }

        public ExecdStack CreateExecdStack(CreateExecdStackOptions options)
        {
            CreateExecdStackCallCount++;
            return new ExecdStack
            {
                Commands = _commands,
                Files = Mock.Of<ISandboxFiles>(),
                Health = Mock.Of<IExecdHealth>(),
                Metrics = Mock.Of<IExecdMetrics>(),
                Isolation = Mock.Of<IIsolatedSessions>()
            };
        }

        public EgressStack CreateEgressStack(CreateEgressStackOptions options)
        {
            return new EgressStack
            {
                Egress = Mock.Of<IEgress>()
            };
        }
    }

    private class LegacyCommandsStub : IExecdCommands
    {
        public IAsyncEnumerable<ServerStreamEvent> RunStreamAsync(string command, RunCommandOptions? options = null, CancellationToken cancellationToken = default) =>
            throw new NotSupportedException();

        public Task<Execution> RunAsync(string command, RunCommandOptions? options = null, ExecutionHandlers? handlers = null, CancellationToken cancellationToken = default) =>
            throw new NotSupportedException();

        public Task InterruptAsync(string sessionId, CancellationToken cancellationToken = default) =>
            throw new NotSupportedException();

        public Task<CommandStatus> GetCommandStatusAsync(string executionId, CancellationToken cancellationToken = default) =>
            throw new NotSupportedException();

        public Task<CommandLogs> GetBackgroundCommandLogsAsync(string executionId, long? cursor = null, CancellationToken cancellationToken = default) =>
            throw new NotSupportedException();

        public Task<string> CreateSessionAsync(CreateSessionOptions? options = null, CancellationToken cancellationToken = default) =>
            throw new NotSupportedException();

        public Task<Execution> RunInSessionAsync(string sessionId, string command, RunInSessionOptions? options = null, ExecutionHandlers? handlers = null, CancellationToken cancellationToken = default) =>
            throw new NotSupportedException();

        public Task DeleteSessionAsync(string sessionId, CancellationToken cancellationToken = default) =>
            throw new NotSupportedException();
    }

    private sealed class CommandsAndInventoryStub : LegacyCommandsStub, IExecdCommandInventory
    {
        public Task<ListCommandsPage> ListCommandsAsync(
            bool? running = null,
            int limit = 50,
            string? cursor = null,
            CancellationToken cancellationToken = default) => throw new NotSupportedException();
    }

    private sealed class LegacySandboxesStub : ISandboxes
    {
        public Task<CreateSandboxResponse> CreateSandboxAsync(CreateSandboxRequest request, CancellationToken cancellationToken = default) => throw new NotSupportedException();
        public Task<SandboxInfo> GetSandboxAsync(string sandboxId, CancellationToken cancellationToken = default) => throw new NotSupportedException();
        public Task<ListSandboxesResponse> ListSandboxesAsync(ListSandboxesParams? @params = null, CancellationToken cancellationToken = default) => throw new NotSupportedException();
        public Task<SandboxInfo> PatchSandboxMetadataAsync(string sandboxId, IReadOnlyDictionary<string, string?> patch, CancellationToken cancellationToken = default) => throw new NotSupportedException();
        public Task DeleteSandboxAsync(string sandboxId, CancellationToken cancellationToken = default) => throw new NotSupportedException();
        public Task PauseSandboxAsync(string sandboxId, CancellationToken cancellationToken = default) => throw new NotSupportedException();
        public Task ResumeSandboxAsync(string sandboxId, CancellationToken cancellationToken = default) => throw new NotSupportedException();
        public Task<RenewSandboxExpirationResponse> RenewSandboxExpirationAsync(string sandboxId, RenewSandboxExpirationRequest request, CancellationToken cancellationToken = default) => throw new NotSupportedException();
        public Task<SnapshotInfo> CreateSnapshotAsync(string sandboxId, CreateSnapshotRequest? request = null, CancellationToken cancellationToken = default) => throw new NotSupportedException();
        public Task<SnapshotInfo> GetSnapshotAsync(string snapshotId, CancellationToken cancellationToken = default) => throw new NotSupportedException();
        public Task<ListSnapshotsResponse> ListSnapshotsAsync(ListSnapshotsParams? @params = null, CancellationToken cancellationToken = default) => throw new NotSupportedException();
        public Task DeleteSnapshotAsync(string snapshotId, CancellationToken cancellationToken = default) => throw new NotSupportedException();

        public Task<Endpoint> GetSandboxEndpointAsync(string sandboxId, int port, bool useServerProxy = false, CancellationToken cancellationToken = default)
        {
            return Task.FromResult(new Endpoint
            {
                EndpointAddress = $"127.0.0.1:{port}",
                Headers = new Dictionary<string, string>()
            });
        }

        public Task<Endpoint> GetSignedSandboxEndpointAsync(string sandboxId, int port, long expires, bool useServerProxy = false, CancellationToken cancellationToken = default) =>
            throw new NotSupportedException();
    }

    private sealed class StubHttpMessageHandler : HttpMessageHandler
    {
        private readonly Func<HttpRequestMessage, CancellationToken, Task<HttpResponseMessage>> _handler;

        public StubHttpMessageHandler(Func<HttpRequestMessage, CancellationToken, Task<HttpResponseMessage>> handler)
        {
            _handler = handler;
        }

        public List<string> RequestUris { get; } = new();

        protected override async Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken cancellationToken)
        {
            RequestUris.Add(request.RequestUri?.ToString() ?? string.Empty);
            return await _handler(request, cancellationToken).ConfigureAwait(false);
        }
    }
}
