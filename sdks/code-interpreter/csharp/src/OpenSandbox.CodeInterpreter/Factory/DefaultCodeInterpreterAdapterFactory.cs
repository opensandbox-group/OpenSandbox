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

using OpenSandbox.CodeInterpreter.Adapters;
using OpenSandbox.CodeInterpreter.Services;
using OpenSandbox.Core;
using OpenSandbox.Internal;
using Microsoft.Extensions.Logging;

namespace OpenSandbox.CodeInterpreter.Factory;

/// <summary>
/// Default implementation of the code interpreter adapter factory.
/// </summary>
public class DefaultCodeInterpreterAdapterFactory : ICodeInterpreterAdapterFactory
{
    /// <summary>
    /// Creates a new instance of the default adapter factory.
    /// </summary>
    /// <returns>A new factory instance.</returns>
    public static DefaultCodeInterpreterAdapterFactory Create() => new();

    /// <inheritdoc />
    public ICodes CreateCodes(CreateCodesStackOptions options)
    {
        if (options == null)
        {
            throw new InvalidArgumentException("options cannot be null");
        }

        if (options.ConnectionConfig == null)
        {
            throw new InvalidArgumentException("options.ConnectionConfig cannot be null");
        }

        if (string.IsNullOrWhiteSpace(options.ExecdBaseUrl))
        {
            throw new InvalidArgumentException("options.ExecdBaseUrl cannot be null or empty");
        }

        if (options.ExecdHeaders == null)
        {
            throw new InvalidArgumentException("options.ExecdHeaders cannot be null");
        }

        if (options.HttpClientProvider == null)
        {
            throw new InvalidArgumentException("options.HttpClientProvider cannot be null");
        }

        if (options.LoggerFactory == null)
        {
            throw new InvalidArgumentException("options.LoggerFactory cannot be null");
        }

        var client = new HttpClientWrapper(
            options.HttpClientProvider.HttpClient,
            options.ExecdBaseUrl,
            options.ExecdHeaders,
            options.LoggerFactory.CreateLogger("OpenSandbox.HttpClientWrapper"));

        return new CodesAdapter(
            client,
            options.HttpClientProvider.SseHttpClient,
            options.ExecdBaseUrl,
            options.ExecdHeaders,
            options.LoggerFactory.CreateLogger("OpenSandbox.CodeInterpreter.CodesAdapter"));
    }
}
