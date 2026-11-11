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

using System.Text.Json;
using OpenSandbox.CodeInterpreter.Models;
using Xunit;

namespace OpenSandbox.CodeInterpreter.Tests;

public class ModelsTests
{
    [Fact]
    public void SupportedLanguage_HasCorrectValues()
    {
        Assert.Equal("python", SupportedLanguage.Python);
        Assert.Equal("java", SupportedLanguage.Java);
        Assert.Equal("go", SupportedLanguage.Go);
        Assert.Equal("typescript", SupportedLanguage.TypeScript);
        Assert.Equal("javascript", SupportedLanguage.JavaScript);
        Assert.Equal("bash", SupportedLanguage.Bash);
    }

    [Fact]
    public void CodeContext_SerializesToJson()
    {
        var context = new CodeContext
        {
            Id = "ctx-123",
            Language = SupportedLanguage.Python
        };

        var json = JsonSerializer.Serialize(context);
        Assert.Contains("\"id\":\"ctx-123\"", json);
        Assert.Contains("\"language\":\"python\"", json);
    }

    [Fact]
    public void CodeContext_DeserializesFromJson()
    {
        var json = "{\"id\":\"ctx-456\",\"language\":\"javascript\"}";
        var context = JsonSerializer.Deserialize<CodeContext>(json);

        Assert.NotNull(context);
        Assert.Equal("ctx-456", context.Id);
        Assert.Equal("javascript", context.Language);
    }

    [Fact]
    public void CodeContext_DeserializesWithNullId()
    {
        var json = "{\"language\":\"python\"}";
        var context = JsonSerializer.Deserialize<CodeContext>(json);

        Assert.NotNull(context);
        Assert.Null(context.Id);
        Assert.Equal("python", context.Language);
    }

    [Fact]
    public void RunCodeRequest_SerializesToJson()
    {
        var request = new RunCodeRequest
        {
            Code = "print(\"hello\")",
            Context = new CodeContext
            {
                Id = "ctx-789",
                Language = SupportedLanguage.Python
            }
        };

        var json = JsonSerializer.Serialize(request);
        Assert.Contains("\"code\":", json);
        Assert.Contains("print", json);
        Assert.Contains("\"context\":", json);
        Assert.Contains("\"id\":\"ctx-789\"", json);
        Assert.Contains("\"language\":\"python\"", json);
    }

    [Fact]
    public void RunCodeRequest_DeserializesFromJson()
    {
        var json = "{\"code\":\"console.log('test')\",\"context\":{\"id\":\"ctx-abc\",\"language\":\"javascript\"}}";
        var request = JsonSerializer.Deserialize<RunCodeRequest>(json);

        Assert.NotNull(request);
        Assert.Equal("console.log('test')", request.Code);
        Assert.NotNull(request.Context);
        Assert.Equal("ctx-abc", request.Context.Id);
        Assert.Equal("javascript", request.Context.Language);
    }

    [Fact]
    public void RunCodeOptions_DefaultsAreNull()
    {
        var options = new RunCodeOptions();

        Assert.Null(options.Context);
        Assert.Null(options.Language);
        Assert.Null(options.Handlers);
    }

    [Fact]
    public void RunCodeOptions_CanSetProperties()
    {
        var context = new CodeContext { Language = SupportedLanguage.Go };
        var handlers = new OpenSandbox.Models.ExecutionHandlers();

        var options = new RunCodeOptions
        {
            Context = context,
            Handlers = handlers
        };

        Assert.Same(context, options.Context);
        Assert.Same(handlers, options.Handlers);
    }

    [Fact]
    public void RunCodeOptions_CanSetLanguageOnly()
    {
        var options = new RunCodeOptions
        {
            Language = SupportedLanguage.TypeScript
        };

        Assert.Null(options.Context);
        Assert.Equal(SupportedLanguage.TypeScript, options.Language);
    }
}
