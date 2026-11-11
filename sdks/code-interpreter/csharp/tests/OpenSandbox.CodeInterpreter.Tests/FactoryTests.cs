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

using OpenSandbox.CodeInterpreter.Factory;
using OpenSandbox.Core;
using OpenSandbox;
using Microsoft.Extensions.Logging.Abstractions;
using Xunit;

namespace OpenSandbox.CodeInterpreter.Tests;

public class FactoryTests
{
    [Fact]
    public void DefaultCodeInterpreterAdapterFactory_Create_ReturnsInstance()
    {
        var factory = DefaultCodeInterpreterAdapterFactory.Create();

        Assert.NotNull(factory);
        Assert.IsType<DefaultCodeInterpreterAdapterFactory>(factory);
    }

    [Fact]
    public void DefaultCodeInterpreterAdapterFactory_CreateCodes_ThrowsOnNullOptions()
    {
        var factory = DefaultCodeInterpreterAdapterFactory.Create();

        Assert.Throws<InvalidArgumentException>(() => factory.CreateCodes(null!));
    }

    [Fact]
    public void DefaultCodeInterpreterAdapterFactory_CreateCodes_ThrowsOnNullConnectionConfig()
    {
        var factory = DefaultCodeInterpreterAdapterFactory.Create();
        var options = new CreateCodesStackOptions
        {
            ConnectionConfig = null!,
            ExecdBaseUrl = "http://localhost:44772",
            ExecdHeaders = new Dictionary<string, string>(),
            HttpClientProvider = new HttpClientProvider(new OpenSandbox.Config.ConnectionConfig(), NullLoggerFactory.Instance),
            LoggerFactory = NullLoggerFactory.Instance
        };

        Assert.Throws<InvalidArgumentException>(() => factory.CreateCodes(options));
    }

    [Fact]
    public void DefaultCodeInterpreterAdapterFactory_CreateCodes_ThrowsOnEmptyBaseUrl()
    {
        var factory = DefaultCodeInterpreterAdapterFactory.Create();

        var options = new CreateCodesStackOptions
        {
            ConnectionConfig = new OpenSandbox.Config.ConnectionConfig(),
            ExecdBaseUrl = "",
            ExecdHeaders = new Dictionary<string, string>(),
            HttpClientProvider = new HttpClientProvider(new OpenSandbox.Config.ConnectionConfig(), NullLoggerFactory.Instance),
            LoggerFactory = NullLoggerFactory.Instance
        };

        Assert.Throws<InvalidArgumentException>(() => factory.CreateCodes(options));
    }

    [Fact]
    public void CreateCodesStackOptions_RequiredProperties()
    {
        var options = new CreateCodesStackOptions
        {
            ConnectionConfig = new OpenSandbox.Config.ConnectionConfig(),
            ExecdBaseUrl = "http://test:8080",
            ExecdHeaders = new Dictionary<string, string> { ["X-Test"] = "value" },
            HttpClientProvider = new HttpClientProvider(new OpenSandbox.Config.ConnectionConfig(), NullLoggerFactory.Instance),
            LoggerFactory = NullLoggerFactory.Instance
        };

        Assert.NotNull(options.ConnectionConfig);
        Assert.Equal("http://test:8080", options.ExecdBaseUrl);
        Assert.Equal("value", options.ExecdHeaders["X-Test"]);
        Assert.NotNull(options.HttpClientProvider);
        Assert.NotNull(options.LoggerFactory);
    }
}
