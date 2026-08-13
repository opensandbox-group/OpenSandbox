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
using FluentAssertions;
using OpenSandbox.Adapters;
using OpenSandbox.Core;
using OpenSandbox.Models;
using Xunit;

namespace OpenSandbox.Tests;

public class SseParserTests
{
    [Fact]
    public async Task ParseJsonEventStreamAsync_WithSseFormat_ShouldParseEvents()
    {
        // Arrange
        var sseContent = @"data: {""type"":""init"",""text"":""session-123""}

data: {""type"":""stdout"",""text"":""Hello World""}

data: {""type"":""execution_complete"",""execution_time"":100}

";
        var response = CreateMockResponse(HttpStatusCode.OK, sseContent);

        // Act
        var events = new List<ServerStreamEvent>();
        await foreach (var ev in SseParser.ParseJsonEventStreamAsync<ServerStreamEvent>(response))
        {
            events.Add(ev);
        }

        // Assert
        events.Should().HaveCount(3);
        events[0].Type.Should().Be("init");
        events[0].Text.Should().Be("session-123");
        events[1].Type.Should().Be("stdout");
        events[1].Text.Should().Be("Hello World");
        events[2].Type.Should().Be("execution_complete");
        events[2].ExecutionTime.Should().Be(100);
    }

    [Fact]
    public async Task ParseJsonEventStreamAsync_WithNdjsonFormat_ShouldParseEvents()
    {
        // Arrange
        var ndjsonContent = @"{""type"":""init"",""text"":""session-456""}
{""type"":""stderr"",""text"":""Error message""}
{""type"":""execution_complete"",""execution_time"":50}
";
        var response = CreateMockResponse(HttpStatusCode.OK, ndjsonContent);

        // Act
        var events = new List<ServerStreamEvent>();
        await foreach (var ev in SseParser.ParseJsonEventStreamAsync<ServerStreamEvent>(response))
        {
            events.Add(ev);
        }

        // Assert
        events.Should().HaveCount(3);
        events[0].Type.Should().Be("init");
        events[1].Type.Should().Be("stderr");
        events[1].Text.Should().Be("Error message");
        events[2].Type.Should().Be("execution_complete");
    }

    [Fact]
    public async Task ParseJsonEventStreamAsync_WithSseComments_ShouldSkipComments()
    {
        // Arrange
        var sseContent = @": this is a comment
data: {""type"":""stdout"",""text"":""output""}
: another comment
";
        var response = CreateMockResponse(HttpStatusCode.OK, sseContent);

        // Act
        var events = new List<ServerStreamEvent>();
        await foreach (var ev in SseParser.ParseJsonEventStreamAsync<ServerStreamEvent>(response))
        {
            events.Add(ev);
        }

        // Assert
        events.Should().HaveCount(1);
        events[0].Type.Should().Be("stdout");
    }

    [Fact]
    public async Task ParseJsonEventStreamAsync_WithSseMetadata_ShouldSkipMetadata()
    {
        // Arrange
        var sseContent = @"event: message
id: 123
retry: 5000
data: {""type"":""stdout"",""text"":""output""}
";
        var response = CreateMockResponse(HttpStatusCode.OK, sseContent);

        // Act
        var events = new List<ServerStreamEvent>();
        await foreach (var ev in SseParser.ParseJsonEventStreamAsync<ServerStreamEvent>(response))
        {
            events.Add(ev);
        }

        // Assert
        events.Should().HaveCount(1);
        events[0].Type.Should().Be("stdout");
    }

    [Fact]
    public async Task ParseJsonEventStreamAsync_WithEmptyLines_ShouldSkipEmptyLines()
    {
        // Arrange
        var sseContent = @"

data: {""type"":""stdout"",""text"":""output""}


";
        var response = CreateMockResponse(HttpStatusCode.OK, sseContent);

        // Act
        var events = new List<ServerStreamEvent>();
        await foreach (var ev in SseParser.ParseJsonEventStreamAsync<ServerStreamEvent>(response))
        {
            events.Add(ev);
        }

        // Assert
        events.Should().HaveCount(1);
    }

    [Fact]
    public async Task ParseJsonEventStreamAsync_WithInvalidJson_ShouldSkipInvalidLines()
    {
        // Arrange
        var sseContent = @"data: {""type"":""stdout"",""text"":""valid""}
data: not valid json
data: {""type"":""stderr"",""text"":""also valid""}
";
        var response = CreateMockResponse(HttpStatusCode.OK, sseContent);

        // Act
        var events = new List<ServerStreamEvent>();
        await foreach (var ev in SseParser.ParseJsonEventStreamAsync<ServerStreamEvent>(response))
        {
            events.Add(ev);
        }

        // Assert
        events.Should().HaveCount(2);
        events[0].Type.Should().Be("stdout");
        events[1].Type.Should().Be("stderr");
    }

    [Fact]
    public async Task ParseJsonEventStreamAsync_WithErrorResponse_ShouldThrowSandboxApiException()
    {
        // Arrange
        var errorContent = @"{""message"":""Not found"",""code"":""NOT_FOUND""}";
        var response = CreateMockResponse(HttpStatusCode.NotFound, errorContent);

        // Act & Assert
        var exception = await Assert.ThrowsAsync<SandboxApiException>(async () =>
        {
            await foreach (var _ in SseParser.ParseJsonEventStreamAsync<ServerStreamEvent>(response))
            {
                // Should not reach here
            }
        });

        exception.StatusCode.Should().Be(404);
        exception.Message.Should().Be("Not found");
        exception.Error.Code.Should().Be("NOT_FOUND");
    }

    [Fact]
    public async Task ParseJsonEventStreamAsync_WithErrorResponseNoJson_ShouldUseFallbackMessage()
    {
        // Arrange
        var response = CreateMockResponse(HttpStatusCode.InternalServerError, "Internal Server Error");

        // Act & Assert
        var exception = await Assert.ThrowsAsync<SandboxApiException>(async () =>
        {
            await foreach (var _ in SseParser.ParseJsonEventStreamAsync<ServerStreamEvent>(response, "Custom fallback"))
            {
                // Should not reach here
            }
        });

        exception.StatusCode.Should().Be(500);
        // The raw body is spliced into the message so logs carry the server's reason.
        exception.Message.Should().Be("Custom fallback: Internal Server Error");
    }

    [Fact]
    public async Task ParseJsonEventStreamAsync_WithUnstructuredJsonBody_ShouldSpliceBodyIntoMessage()
    {
        // Arrange
        var errorContent = @"{""error"":""invalid parameter""}";
        var response = CreateMockResponse(HttpStatusCode.BadRequest, errorContent);

        // Act & Assert
        var exception = await Assert.ThrowsAsync<SandboxApiException>(async () =>
        {
            await foreach (var _ in SseParser.ParseJsonEventStreamAsync<ServerStreamEvent>(response, "Run code failed"))
            {
                // Should not reach here
            }
        });

        exception.StatusCode.Should().Be(400);
        exception.Message.Should().Be(@"Run code failed: {""error"":""invalid parameter""}");
    }

    [Fact]
    public async Task ParseJsonEventStreamAsync_WithCancellation_ShouldStopParsing()
    {
        // Arrange
        var sseContent = @"data: {""type"":""stdout"",""text"":""line1""}
data: {""type"":""stdout"",""text"":""line2""}
data: {""type"":""stdout"",""text"":""line3""}
";
        var response = CreateMockResponse(HttpStatusCode.OK, sseContent);
        var cts = new CancellationTokenSource();

        // Act
        var events = new List<ServerStreamEvent>();
        await Assert.ThrowsAsync<OperationCanceledException>(async () =>
        {
            await foreach (var ev in SseParser.ParseJsonEventStreamAsync<ServerStreamEvent>(response, cancellationToken: cts.Token))
            {
                events.Add(ev);
                if (events.Count == 1)
                {
                    cts.Cancel();
                }
            }
        });

        // Assert
        events.Should().HaveCount(1);
    }

    private static HttpResponseMessage CreateMockResponse(HttpStatusCode statusCode, string content)
    {
        return new HttpResponseMessage(statusCode)
        {
            Content = new StringContent(content, Encoding.UTF8, "text/event-stream")
        };
    }
}
