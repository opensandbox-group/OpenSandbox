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
using OpenSandbox.Config;
using OpenSandbox.Core;
using OpenSandbox.Internal;
using OpenSandbox.Models;
using Xunit;

namespace OpenSandbox.Tests;

/// <summary>
/// OSEP-0024 wake retry contract and idle policy coverage.
/// </summary>
public class WakeRetryTests
{
    /// <summary>
    /// Handler that answers each request from a scripted queue and records
    /// every request body.
    /// </summary>
    private sealed class ScriptedHandler : HttpMessageHandler
    {
        private readonly Queue<Func<HttpRequestMessage, HttpResponseMessage>> _responses = new();
        public List<HttpRequestMessage> Requests { get; } = new();
        public List<string?> Bodies { get; } = new();

        public void Enqueue(Func<HttpRequestMessage, HttpResponseMessage> response) => _responses.Enqueue(response);

        public void EnqueueMarked(string retryAfter = "0", string marker = "expired")
        {
            Enqueue(_ =>
            {
                var response = new HttpResponseMessage(HttpStatusCode.ServiceUnavailable);
                if (marker != null)
                {
                    response.Headers.TryAddWithoutValidation("OpenSandbox-Wake", marker);
                }
                if (retryAfter != null)
                {
                    response.Headers.TryAddWithoutValidation("Retry-After", retryAfter);
                }
                response.Content = new StringContent(
                    "{\"error\":{\"code\":\"sandbox_wake_timeout\",\"message\":\"restore in progress\"}}",
                    Encoding.UTF8,
                    "application/json");
                return response;
            });
        }

        public void EnqueueOk()
        {
            Enqueue(_ => new HttpResponseMessage(HttpStatusCode.OK)
            {
                Content = new StringContent(
                    "{\"id\":\"sbx-1\",\"status\":{\"state\":\"Running\"},\"createdAt\":\"2026-03-14T12:00:00Z\",\"entrypoint\":[\"python\"]}",
                    Encoding.UTF8,
                    "application/json")
            });
        }

        protected override async Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken cancellationToken)
        {
            Requests.Add(request);
            Bodies.Add(request.Content == null ? null : await request.Content.ReadAsStringAsync(cancellationToken));
            var next = _responses.Dequeue();
            return next(request);
        }
    }

    private static HttpClientWrapper NewWrapper(ScriptedHandler handler, WakeRetryOptions? wake = null) =>
        new(new HttpClient(handler), "http://localhost:8080/v1", wakeRetry: wake);

    [Fact]
    public async Task Marked503OnPostIsRetriedAndSucceeds()
    {
        var handler = new ScriptedHandler();
        handler.EnqueueMarked();
        handler.EnqueueOk();
        var wrapper = NewWrapper(handler);

        var result = await wrapper.PostAsync<JsonElement>("/sandboxes", new { image = "python:3.12" });

        result.GetProperty("id").GetString().Should().Be("sbx-1");
        handler.Requests.Should().HaveCount(2);
        handler.Bodies[1].Should().Be(handler.Bodies[0]);
    }

    [Fact]
    public async Task ExhaustionThrowsSandboxWakeTimeoutExceptionWithAttemptsAndWaited()
    {
        var handler = new ScriptedHandler();
        handler.EnqueueMarked();
        handler.EnqueueMarked();
        handler.EnqueueMarked();
        var wrapper = NewWrapper(handler, new WakeRetryOptions(true, 3, TimeSpan.FromMinutes(1)));

        var act = async () => await wrapper.PostAsync<JsonElement>("/sandboxes", new { x = 1 });

        var exception = await act.Should().ThrowAsync<SandboxWakeTimeoutException>();
        exception.Which.Attempts.Should().Be(3);
        exception.Which.Waited.Should().BeGreaterThanOrEqualTo(TimeSpan.Zero);
        exception.Which.StatusCode.Should().Be(503);
        exception.Which.Error.Code.Should().Be(SandboxErrorCodes.WakeTimeout);
        handler.Requests.Should().HaveCount(3);
    }

    [Fact]
    public async Task RetryAfterExceedingRemainingBudgetEndsTheRetry()
    {
        var handler = new ScriptedHandler();
        handler.EnqueueMarked(retryAfter: "30");
        var wrapper = NewWrapper(handler, new WakeRetryOptions(true, 5, TimeSpan.FromSeconds(10)));

        var act = async () => await wrapper.PostAsync<JsonElement>("/sandboxes", new { x = 1 });

        var exception = await act.Should().ThrowAsync<SandboxWakeTimeoutException>();
        exception.Which.Attempts.Should().Be(1);
        handler.Requests.Should().HaveCount(1);
    }

    [Fact]
    public async Task Bare503IsReturnedUntouched()
    {
        var handler = new ScriptedHandler();
        handler.Enqueue(request => new HttpResponseMessage(HttpStatusCode.ServiceUnavailable)
        {
            Content = new StringContent("{}", Encoding.UTF8, "application/json")
        });
        var wrapper = NewWrapper(handler);

        var act = async () => await wrapper.PostAsync<JsonElement>("/sandboxes", new { x = 1 });

        var thrown = await act.Should().ThrowAsync<SandboxApiException>();
        thrown.Which.Should().NotBeOfType<SandboxWakeTimeoutException>();
        handler.Requests.Should().HaveCount(1);
    }

    [Fact]
    public async Task ForgedMarkerValueDoesNotTriggerRetry()
    {
        var handler = new ScriptedHandler();
        handler.EnqueueMarked(marker: "bogus");
        var wrapper = NewWrapper(handler);

        var act = async () => await wrapper.PostAsync<JsonElement>("/sandboxes", new { x = 1 });

        var thrown = await act.Should().ThrowAsync<SandboxApiException>();
        thrown.Which.Should().NotBeOfType<SandboxWakeTimeoutException>();
        handler.Requests.Should().HaveCount(1);
    }

    [Fact]
    public async Task DisabledPolicyReturnsMarked503Untouched()
    {
        var handler = new ScriptedHandler();
        handler.EnqueueMarked();
        var wrapper = NewWrapper(handler, new WakeRetryOptions(false, 5, TimeSpan.FromMinutes(1)));

        var act = async () => await wrapper.PostAsync<JsonElement>("/sandboxes", new { x = 1 });

        var thrown = await act.Should().ThrowAsync<SandboxApiException>();
        thrown.Which.Should().NotBeOfType<SandboxWakeTimeoutException>();
        handler.Requests.Should().HaveCount(1);
    }

    [Fact]
    public async Task HttpDateRetryAfterIsHonored()
    {
        var handler = new ScriptedHandler();
        handler.EnqueueMarked(retryAfter: DateTime.UtcNow.AddMilliseconds(50).ToString("R"));
        handler.EnqueueOk();
        var wrapper = NewWrapper(handler);

        var result = await wrapper.PostAsync<JsonElement>("/sandboxes", new { x = 1 });

        result.GetProperty("id").GetString().Should().Be("sbx-1");
        handler.Requests.Should().HaveCount(2);
    }

    [Fact]
    public async Task NullOverallDeadlineRemovesTheTimeBound()
    {
        var handler = new ScriptedHandler();
        handler.EnqueueMarked(retryAfter: "1");
        handler.EnqueueOk();
        var wrapper = NewWrapper(handler, new WakeRetryOptions(true, 5, null));

        var result = await wrapper.PostAsync<JsonElement>("/sandboxes", new { x = 1 });

        result.GetProperty("id").GetString().Should().Be("sbx-1");
    }

    [Fact]
    public void ConnectionConfigDefaultsEnableWakeRetry()
    {
        var config = new ConnectionConfig(new ConnectionConfigOptions { Domain = "http://127.0.0.1:8080" });
        config.WakeRetry.Enabled.Should().BeTrue();
        config.WakeRetry.MaxAttempts.Should().Be(5);
        config.WakeRetry.OverallDeadline.Should().Be(TimeSpan.FromSeconds(60));
    }

    [Fact]
    public void ConnectionConfigRejectsInvalidWakeOptions()
    {
        var act = () => new ConnectionConfig(new ConnectionConfigOptions { WakeMaxAttempts = 0 });
        act.Should().Throw<ArgumentException>().WithMessage("*WakeMaxAttempts*");

        var config = new ConnectionConfig(new ConnectionConfigOptions { WakeOverallDeadlineSeconds = 0 });
        config.WakeRetry.OverallDeadline.Should().BeNull();
    }

    [Fact]
    public void IdlePolicyValidateChecksPairingAndRanges()
    {
        new IdlePolicy { IdleTimeoutSeconds = 29, OnIdle = OnIdleAction.Pause }
            .Invoking(p => p.Validate()).Should().Throw<ArgumentException>();
        new IdlePolicy { IdleTimeoutSeconds = 86401, OnIdle = OnIdleAction.Pause }
            .Invoking(p => p.Validate()).Should().Throw<ArgumentException>();
        new IdlePolicy { OnIdle = OnIdleAction.Pause }
            .Invoking(p => p.Validate()).Should().Throw<InvalidOperationException>();
        new IdlePolicy { IdleTimeoutSeconds = 300 }
            .Invoking(p => p.Validate()).Should().Throw<InvalidOperationException>();
        new IdlePolicy { WakeOnAccess = false, ActiveProbe = new ActivityProbe { Port = 8080, Path = "/active" } }
            .Invoking(p => p.Validate()).Should().Throw<InvalidOperationException>();
        new IdlePolicy
            {
                IdleTimeoutSeconds = 300,
                OnIdle = OnIdleAction.Pause,
                ActiveProbe = new ActivityProbe { Port = 8080, Path = "/active", TimeoutSeconds = 2 }
            }
            .Invoking(p => p.Validate()).Should().NotThrow();
    }

    [Fact]
    public void ActivityProbeRejectsOutOfRangeTimeout()
    {
        var act = () => new ActivityProbe { Port = 8080, Path = "/active", TimeoutSeconds = 11 };
        act.Should().Throw<ArgumentOutOfRangeException>();
    }

    /// <summary>
    /// Records the JSON body of the create request and answers with a
    /// minimal valid create response.
    /// </summary>
    private sealed class CaptureCreateBodyHandler : HttpMessageHandler
    {
        public string? RequestBody { get; private set; }

        protected override async Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken cancellationToken)
        {
            RequestBody = request.Content == null
                ? null
                : await request.Content.ReadAsStringAsync(cancellationToken);
            return new HttpResponseMessage(HttpStatusCode.OK)
            {
                Content = new StringContent(
                    "{\"id\":\"sbx-3\",\"status\":{\"state\":\"Pending\"},\"createdAt\":\"2026-03-14T12:00:00Z\",\"entrypoint\":[\"python\"]}",
                    Encoding.UTF8,
                    "application/json")
            };
        }
    }

    [Fact]
    public async Task CreateSandboxSerializesLifecycleIdlePolicy()
    {
        var handler = new CaptureCreateBodyHandler();
        var client = new HttpClient(handler);
        var wrapper = new HttpClientWrapper(client, "http://localhost:8080/v1");
        var adapter = new SandboxesAdapter(wrapper);

        var request = new CreateSandboxRequest
        {
            Image = new ImageSpec { Uri = "python:3.12" },
            Entrypoint = new[] { "python" },
            ResourceLimits = new Dictionary<string, string> { ["cpu"] = "1", ["memory"] = "1Gi" },
            Lifecycle = new SandboxLifecycle
            {
                IdlePolicy = new IdlePolicy
                {
                    IdleTimeoutSeconds = 300,
                    OnIdle = OnIdleAction.Pause,
                    WakeOnAccess = true,
                    ActiveProbe = new ActivityProbe { Port = 8080, Path = "/active" }
                }
            }
        };
        await adapter.CreateSandboxAsync(request);

        using var doc = JsonDocument.Parse(handler.RequestBody!);
        var lifecycle = doc.RootElement.GetProperty("lifecycle");
        var idlePolicy = lifecycle.GetProperty("idlePolicy");
        idlePolicy.GetProperty("idleTimeoutSeconds").GetInt32().Should().Be(300);
        idlePolicy.GetProperty("onIdle").GetString().Should().Be("pause");
        idlePolicy.GetProperty("wakeOnAccess").GetBoolean().Should().BeTrue();
        idlePolicy.GetProperty("activeProbe").GetProperty("port").GetInt32().Should().Be(8080);
        idlePolicy.GetProperty("activeProbe").GetProperty("path").GetString().Should().Be("/active");
    }

    [Fact]
    public async Task TemplateModeCreateSerializesIdlePolicyInsideLifecycle()
    {
        // The template flow reuses CreateSandboxAsync: the facade passes a
        // TemplateId-carrying request whose lifecycle carries only the idle
        // policy (control-plane state, not workload shape).
        var handler = new CaptureCreateBodyHandler();
        var client = new HttpClient(handler);
        var wrapper = new HttpClientWrapper(client, "http://localhost:8080/v1");
        var adapter = new SandboxesAdapter(wrapper);

        await adapter.CreateSandboxAsync(new CreateSandboxRequest
        {
            TemplateId = "tpl_123",
            Timeout = 600,
            Lifecycle = new SandboxLifecycle
            {
                IdlePolicy = new IdlePolicy
                {
                    IdleTimeoutSeconds = 300,
                    OnIdle = OnIdleAction.Pause,
                    WakeOnAccess = true
                }
            }
        });

        using var doc = JsonDocument.Parse(handler.RequestBody!);
        doc.RootElement.GetProperty("templateId").GetString().Should().Be("tpl_123");
        var lifecycle = doc.RootElement.GetProperty("lifecycle");
        var idlePolicy = lifecycle.GetProperty("idlePolicy");
        idlePolicy.GetProperty("idleTimeoutSeconds").GetInt32().Should().Be(300);
        idlePolicy.GetProperty("onIdle").GetString().Should().Be("pause");
        lifecycle.TryGetProperty("preStart", out _).Should().BeFalse();
    }

    [Fact]
    public async Task WithBaseUrlCarriesWakeRetryOptions()
    {
        // FilesystemAdapter.WithIdentity derives its wrapper via WithBaseUrl;
        // derived wrappers must keep the configured wake policy instead of
        // silently falling back to defaults.
        var handler = new ScriptedHandler();
        handler.EnqueueMarked();
        handler.EnqueueOk();
        var wrapper = NewWrapper(handler, new WakeRetryOptions(true, 5, TimeSpan.FromMinutes(1)));
        var derived = wrapper.WithBaseUrl("http://localhost:8080/v1");

        var result = await derived.PostAsync<JsonElement>("/sandboxes", new { x = 1 });

        result.GetProperty("id").GetString().Should().Be("sbx-1");
        handler.Requests.Should().HaveCount(2);
    }

    [Fact]
    public void IdlePolicyValidateChecksProbePortAndPath()
    {
        new IdlePolicy
            {
                IdleTimeoutSeconds = 300,
                OnIdle = OnIdleAction.Pause,
                ActiveProbe = new ActivityProbe { Port = 0, Path = "/active" }
            }
            .Invoking(p => p.Validate()).Should().Throw<ArgumentOutOfRangeException>();
        new IdlePolicy
            {
                IdleTimeoutSeconds = 300,
                OnIdle = OnIdleAction.Pause,
                ActiveProbe = new ActivityProbe { Port = 8080, Path = "active" }
            }
            .Invoking(p => p.Validate()).Should().Throw<ArgumentException>();
        new IdlePolicy
            {
                IdleTimeoutSeconds = 300,
                OnIdle = OnIdleAction.Pause,
                ActiveProbe = new ActivityProbe { Port = 8080, Path = "/a b" }
            }
            .Invoking(p => p.Validate()).Should().Throw<ArgumentException>();
        new IdlePolicy
            {
                IdleTimeoutSeconds = 300,
                OnIdle = OnIdleAction.Pause,
                ActiveProbe = new ActivityProbe { Port = 65535, Path = "/active" }
            }
            .Invoking(p => p.Validate()).Should().NotThrow();
    }

    [Fact]
    public async Task HttpDateRetryAfterParsesUnderNonGregorianCulture()
    {
        // RFC1123 dates are culture-invariant; a host culture with a
        // non-Gregorian calendar (ar-SA) must not degrade parsing to the 1s
        // fallback in a way that breaks the retry decision.
        var originalCulture = System.Globalization.CultureInfo.CurrentCulture;
        try
        {
            System.Globalization.CultureInfo.CurrentCulture = new System.Globalization.CultureInfo("ar-SA");
            var handler = new ScriptedHandler();
            handler.EnqueueMarked(retryAfter: DateTime.UtcNow.AddMilliseconds(50).ToString("R"));
            handler.EnqueueOk();
            var wrapper = NewWrapper(handler);

            var result = await wrapper.PostAsync<JsonElement>("/sandboxes", new { x = 1 });

            result.GetProperty("id").GetString().Should().Be("sbx-1");
            handler.Requests.Should().HaveCount(2);
        }
        finally
        {
            System.Globalization.CultureInfo.CurrentCulture = originalCulture;
        }
    }

    [Fact]
    public async Task RetryAfterCapStopsTheRetryWhenItExceedsTheBudget()
    {
        // A pathological header (3600s) is capped at 60s; a 59.9s budget must
        // therefore end the wake retry without waiting.
        var handler = new ScriptedHandler();
        handler.EnqueueMarked(retryAfter: "3600");
        var wrapper = NewWrapper(handler, new WakeRetryOptions(true, 5, TimeSpan.FromSeconds(59.9)));

        var act = async () => await wrapper.PostAsync<JsonElement>("/sandboxes", new { x = 1 });

        var exception = await act.Should().ThrowAsync<SandboxWakeTimeoutException>();
        exception.Which.Attempts.Should().Be(1);
        handler.Requests.Should().HaveCount(1);
    }

    [Fact]
    public async Task CallerCancellationDuringWakeWaitStopsRetrying()
    {
        var handler = new ScriptedHandler();
        handler.EnqueueMarked(retryAfter: "1");
        var wrapper = NewWrapper(handler);

        using var cts = new CancellationTokenSource();
        var act = async () => await wrapper.PostAsync<JsonElement>("/sandboxes", new { x = 1 }, cts.Token);
        cts.CancelAfter(TimeSpan.FromMilliseconds(50));

        await act.Should().ThrowAsync<OperationCanceledException>();
        handler.Requests.Should().HaveCount(1);
    }

    [Fact]
    public async Task GetBytesAsyncGoesThroughTheWakeRetry()
    {
        // Buffered file downloads are trivially replayable and must wake a
        // paused sandbox like the JSON reads.
        var handler = new ScriptedHandler();
        handler.EnqueueMarked();
        handler.Enqueue(request => new HttpResponseMessage(HttpStatusCode.OK)
        {
            Content = new ByteArrayContent(new byte[] { 1, 2, 3 })
        });
        var wrapper = NewWrapper(handler);

        var bytes = await wrapper.GetBytesAsync("/files/download");

        bytes.Should().Equal(new byte[] { 1, 2, 3 });
        handler.Requests.Should().HaveCount(2);
    }

    [Fact]
    public async Task WakeDisabledClientDoesNotRetryThroughIsolatedSessionFiles()
    {
        // The isolated-sessions adapter must honor the client's wake policy:
        // a disabled client gets the marked 503 after exactly one attempt.
        var handler = new ScriptedHandler();
        handler.EnqueueMarked();
        var wake = new WakeRetryOptions(false, 5, TimeSpan.FromMinutes(1));
        var wrapper = new HttpClientWrapper(
            new HttpClient(handler), "http://localhost:8080/v1", wakeRetry: wake);
        var derived = wrapper.WithBaseUrl("http://localhost:9090/v1");

        var act = async () => await derived.PostAsync<JsonElement>("/session/f/files/x", null);

        var thrown = await act.Should().ThrowAsync<SandboxApiException>();
        thrown.Which.Should().NotBeOfType<SandboxWakeTimeoutException>();
        handler.Requests.Should().HaveCount(1);
    }
}
