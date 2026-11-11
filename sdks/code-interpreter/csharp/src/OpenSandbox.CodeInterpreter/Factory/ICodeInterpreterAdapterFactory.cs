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

using OpenSandbox.CodeInterpreter.Services;
using OpenSandbox.Config;
using OpenSandbox;
using Microsoft.Extensions.Logging;

namespace OpenSandbox.CodeInterpreter.Factory;

/// <summary>
/// Options for creating a codes service stack.
/// </summary>
public class CreateCodesStackOptions
{
    /// <summary>
    /// Gets or sets the connection configuration.
    /// </summary>
    public required ConnectionConfig ConnectionConfig { get; set; }

    /// <summary>
    /// Gets or sets the execd API base URL.
    /// </summary>
    public required string ExecdBaseUrl { get; set; }

    /// <summary>
    /// Gets or sets headers to apply to execd requests.
    /// </summary>
    public required IReadOnlyDictionary<string, string> ExecdHeaders { get; set; }

    /// <summary>
    /// Gets or sets the HTTP client provider for this SDK instance.
    /// </summary>
    public required HttpClientProvider HttpClientProvider { get; set; }

    /// <summary>
    /// Gets or sets the logger factory for this SDK instance.
    /// </summary>
    public required ILoggerFactory LoggerFactory { get; set; }
}

/// <summary>
/// Factory interface for creating code interpreter adapters.
/// </summary>
public interface ICodeInterpreterAdapterFactory
{
    /// <summary>
    /// Creates a codes service instance.
    /// </summary>
    /// <param name="options">The creation options.</param>
    /// <returns>The codes service.</returns>
    ICodes CreateCodes(CreateCodesStackOptions options);
}
