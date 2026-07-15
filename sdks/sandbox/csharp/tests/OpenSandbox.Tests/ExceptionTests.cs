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

using FluentAssertions;
using OpenSandbox.Core;
using Xunit;

namespace OpenSandbox.Tests;

public class ExceptionTests
{
    [Fact]
    public void SandboxError_ShouldStoreCodeAndMessage()
    {
        // Arrange & Act
        var error = new SandboxError("TEST_CODE", "Test message");

        // Assert
        error.Code.Should().Be("TEST_CODE");
        error.Message.Should().Be("Test message");
    }

    [Fact]
    public void SandboxError_ToString_WithMessage_ShouldFormatCorrectly()
    {
        // Arrange
        var error = new SandboxError("TEST_CODE", "Test message");

        // Act
        var result = error.ToString();

        // Assert
        result.Should().Be("[TEST_CODE] Test message");
    }

    [Fact]
    public void SandboxError_ToString_WithoutMessage_ShouldFormatCorrectly()
    {
        // Arrange
        var error = new SandboxError("TEST_CODE");

        // Act
        var result = error.ToString();

        // Assert
        result.Should().Be("[TEST_CODE]");
    }

    [Fact]
    public void SandboxException_ShouldContainError()
    {
        // Arrange
        var error = new SandboxError("TEST_CODE", "Test message");

        // Act
        var exception = new SandboxException("Exception message", error: error);

        // Assert
        exception.Message.Should().Be("Exception message");
        exception.Error.Should().Be(error);
    }

    [Fact]
    public void SandboxException_ShouldContainRequestId()
    {
        // Arrange & Act
        var exception = new SandboxException("Exception message", requestId: "req-base-123");

        // Assert
        exception.RequestId.Should().Be("req-base-123");
    }

    [Fact]
    public void SandboxException_ShouldDeclareLegacyConstructor_ForBinaryCompatibility()
    {
        var constructor = typeof(SandboxException).GetConstructor(
            new[]
            {
                typeof(string),
                typeof(Exception),
                typeof(SandboxError)
            });

        constructor.Should().NotBeNull();
    }

    [Fact]
    public void SandboxException_WithoutError_ShouldCreateDefaultError()
    {
        // Arrange & Act
        var exception = new SandboxException("Exception message");

        // Assert
        exception.Error.Should().NotBeNull();
        exception.Error.Code.Should().Be(SandboxErrorCodes.InternalUnknownError);
    }

    [Fact]
    public void SandboxApiException_ShouldContainStatusCodeAndRequestId()
    {
        // Arrange & Act
        var exception = new SandboxApiException(
            message: "API error",
            statusCode: 404,
            requestId: "req-123",
            rawBody: "Not found");

        // Assert
        exception.Message.Should().Be("API error");
        exception.StatusCode.Should().Be(404);
        exception.RequestId.Should().Be("req-123");
        exception.RawBody.Should().Be("Not found");
        exception.Error.Code.Should().Be(SandboxErrorCodes.UnexpectedResponse);
    }

    [Fact]
    public void SandboxApiException_ShouldDeclareRequestIdProperty_ForBinaryCompatibility()
    {
        var requestIdProperty = typeof(SandboxApiException).GetProperty(
            "RequestId",
            System.Reflection.BindingFlags.Public |
            System.Reflection.BindingFlags.Instance |
            System.Reflection.BindingFlags.DeclaredOnly);

        requestIdProperty.Should().NotBeNull();
    }

    [Fact]
    public void SandboxApiException_WithCustomError_ShouldUseProvidedError()
    {
        // Arrange
        var error = new SandboxError("CUSTOM_CODE", "Custom message");

        // Act
        var exception = new SandboxApiException(
            message: "API error",
            statusCode: 500,
            error: error);

        // Assert
        exception.Error.Should().Be(error);
        exception.Error.Code.Should().Be("CUSTOM_CODE");
    }

    [Fact]
    public void SandboxReadyTimeoutException_ShouldHaveCorrectErrorCode()
    {
        // Arrange & Act
        var exception = new SandboxReadyTimeoutException("Timeout waiting for sandbox");

        // Assert
        exception.Error.Code.Should().Be(SandboxErrorCodes.ReadyTimeout);
        exception.Message.Should().Be("Timeout waiting for sandbox");
    }

    [Fact]
    public void SandboxUnhealthyException_ShouldHaveCorrectErrorCode()
    {
        // Arrange & Act
        var exception = new SandboxUnhealthyException("Sandbox is unhealthy");

        // Assert
        exception.Error.Code.Should().Be(SandboxErrorCodes.Unhealthy);
        exception.Message.Should().Be("Sandbox is unhealthy");
    }

    [Fact]
    public void InvalidArgumentException_ShouldHaveCorrectErrorCode()
    {
        // Arrange & Act
        var exception = new InvalidArgumentException("Invalid argument provided");

        // Assert
        exception.Error.Code.Should().Be(SandboxErrorCodes.InvalidArgument);
        exception.Message.Should().Be("Invalid argument provided");
    }

    [Fact]
    public void SandboxInternalException_ShouldHaveCorrectErrorCode()
    {
        // Arrange & Act
        var exception = new SandboxInternalException("Internal error occurred");

        // Assert
        exception.Error.Code.Should().Be(SandboxErrorCodes.InternalUnknownError);
        exception.Message.Should().Be("Internal error occurred");
    }

    [Fact]
    public void SandboxException_WithInnerException_ShouldPreserveInnerException()
    {
        // Arrange
        var innerException = new InvalidOperationException("Inner error");

        // Act
        var exception = new SandboxException("Outer error", innerException);

        // Assert
        exception.InnerException.Should().Be(innerException);
    }
}
