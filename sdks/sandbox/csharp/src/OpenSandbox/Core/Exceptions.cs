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

namespace OpenSandbox.Core;

/// <summary>
/// Error codes used by the OpenSandbox SDK.
/// </summary>
public static class SandboxErrorCodes
{
    /// <summary>
    /// An internal unknown error occurred.
    /// </summary>
    public const string InternalUnknownError = "INTERNAL_UNKNOWN_ERROR";

    /// <summary>
    /// Timeout waiting for sandbox to become ready.
    /// </summary>
    public const string ReadyTimeout = "READY_TIMEOUT";

    /// <summary>
    /// Sandbox is unhealthy.
    /// </summary>
    public const string Unhealthy = "UNHEALTHY";

    /// <summary>
    /// Invalid argument provided.
    /// </summary>
    public const string InvalidArgument = "INVALID_ARGUMENT";

    /// <summary>
    /// Unexpected response from the server.
    /// </summary>
    public const string UnexpectedResponse = "UNEXPECTED_RESPONSE";
}

/// <summary>
/// Structured error payload carried by <see cref="SandboxException"/>.
/// </summary>
public sealed class SandboxError
{
    /// <summary>
    /// Gets the stable programmatic error code.
    /// </summary>
    public string Code { get; }

    /// <summary>
    /// Gets the optional human-readable error message.
    /// </summary>
    public string? Message { get; }

    /// <summary>
    /// Initializes a new instance of the <see cref="SandboxError"/> class.
    /// </summary>
    /// <param name="code">The error code.</param>
    /// <param name="message">The optional error message.</param>
    public SandboxError(string code, string? message = null)
    {
        Code = code ?? throw new ArgumentNullException(nameof(code));
        Message = message;
    }

    /// <inheritdoc />
    public override string ToString() => Message != null ? $"[{Code}] {Message}" : $"[{Code}]";
}

/// <summary>
/// Base exception class for all OpenSandbox SDK errors.
/// </summary>
public class SandboxException : Exception
{
    /// <summary>
    /// Gets the structured error information.
    /// </summary>
    public SandboxError Error { get; }

    /// <summary>
    /// Gets the request ID from the server response when available.
    /// </summary>
    public string? RequestId { get; }

    /// <summary>
    /// Initializes a new instance of the <see cref="SandboxException"/> class.
    /// Kept for binary compatibility with previous SDK versions.
    /// </summary>
    /// <param name="message">The error message.</param>
    /// <param name="innerException">The inner exception.</param>
    /// <param name="error">The structured error information.</param>
    public SandboxException(
        string? message,
        Exception? innerException,
        SandboxError? error)
        : this(message, innerException, error, null)
    {
    }

    /// <summary>
    /// Initializes a new instance of the <see cref="SandboxException"/> class.
    /// </summary>
    /// <param name="message">The error message.</param>
    /// <param name="innerException">The inner exception.</param>
    /// <param name="error">The structured error information.</param>
    /// <param name="requestId">The request ID.</param>
    public SandboxException(
        string? message = null,
        Exception? innerException = null,
        SandboxError? error = null,
        string? requestId = null)
        : base(message ?? error?.Message, innerException)
    {
        Error = error ?? new SandboxError(SandboxErrorCodes.InternalUnknownError, message);
        RequestId = requestId;
    }
}

/// <summary>
/// Exception thrown when an API request fails.
/// </summary>
public class SandboxApiException : SandboxException
{
    /// <summary>
    /// Gets the HTTP status code of the failed request.
    /// </summary>
    public int? StatusCode { get; }

    /// <summary>
    /// Gets the request ID from the server response when available.
    /// Kept on the derived type for binary compatibility with older releases.
    /// </summary>
    public new string? RequestId => base.RequestId;

    /// <summary>
    /// Gets the raw response body.
    /// </summary>
    public object? RawBody { get; }

    /// <summary>
    /// Initializes a new instance of the <see cref="SandboxApiException"/> class.
    /// </summary>
    /// <param name="message">The error message.</param>
    /// <param name="statusCode">The HTTP status code.</param>
    /// <param name="requestId">The request ID.</param>
    /// <param name="rawBody">The raw response body.</param>
    /// <param name="innerException">The inner exception.</param>
    /// <param name="error">The structured error information.</param>
    public SandboxApiException(
        string? message = null,
        int? statusCode = null,
        string? requestId = null,
        object? rawBody = null,
        Exception? innerException = null,
        SandboxError? error = null)
        : base(message, innerException, error ?? new SandboxError(SandboxErrorCodes.UnexpectedResponse, message), requestId)
    {
        StatusCode = statusCode;
        RawBody = rawBody;
    }
}

/// <summary>
/// Exception thrown when an internal SDK error occurs.
/// </summary>
public class SandboxInternalException : SandboxException
{
    /// <summary>
    /// Initializes a new instance of the <see cref="SandboxInternalException"/> class.
    /// </summary>
    /// <param name="message">The error message.</param>
    /// <param name="innerException">The inner exception.</param>
    public SandboxInternalException(string? message = null, Exception? innerException = null)
        : base(message, innerException, new SandboxError(SandboxErrorCodes.InternalUnknownError, message))
    {
    }
}

/// <summary>
/// Exception thrown when a sandbox is unhealthy.
/// </summary>
public class SandboxUnhealthyException : SandboxException
{
    /// <summary>
    /// Initializes a new instance of the <see cref="SandboxUnhealthyException"/> class.
    /// </summary>
    /// <param name="message">The error message.</param>
    /// <param name="innerException">The inner exception.</param>
    public SandboxUnhealthyException(string? message = null, Exception? innerException = null)
        : base(message, innerException, new SandboxError(SandboxErrorCodes.Unhealthy, message))
    {
    }
}

/// <summary>
/// Exception thrown when waiting for sandbox readiness times out.
/// </summary>
public class SandboxReadyTimeoutException : SandboxException
{
    /// <summary>
    /// Initializes a new instance of the <see cref="SandboxReadyTimeoutException"/> class.
    /// </summary>
    /// <param name="message">The error message.</param>
    /// <param name="innerException">The inner exception.</param>
    public SandboxReadyTimeoutException(string? message = null, Exception? innerException = null)
        : base(message, innerException, new SandboxError(SandboxErrorCodes.ReadyTimeout, message))
    {
    }
}

/// <summary>
/// Exception thrown when an invalid argument is provided.
/// </summary>
public class InvalidArgumentException : SandboxException
{
    /// <summary>
    /// Initializes a new instance of the <see cref="InvalidArgumentException"/> class.
    /// </summary>
    /// <param name="message">The error message.</param>
    /// <param name="innerException">The inner exception.</param>
    public InvalidArgumentException(string? message = null, Exception? innerException = null)
        : base(message, innerException, new SandboxError(SandboxErrorCodes.InvalidArgument, message))
    {
    }
}
