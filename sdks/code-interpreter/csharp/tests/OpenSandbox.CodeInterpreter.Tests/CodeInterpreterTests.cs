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

using OpenSandbox.CodeInterpreter.Models;
using OpenSandbox.Core;
using Xunit;

namespace OpenSandbox.CodeInterpreter.Tests;

public class CodeInterpreterTests
{
    [Fact]
    public async Task CreateAsync_ThrowsOnNullSandbox()
    {
        await Assert.ThrowsAsync<InvalidArgumentException>(
            () => CodeInterpreter.CreateAsync(null!));
    }

    [Fact]
    public void CodeInterpreterCreateOptions_DefaultsAreNull()
    {
        var options = new CodeInterpreterCreateOptions();

        Assert.Null(options.AdapterFactory);
    }

    [Fact]
    public void CodeInterpreterCreateOptions_CanSetAdapterFactory()
    {
        var factory = new TestAdapterFactory();
        var options = new CodeInterpreterCreateOptions
        {
            AdapterFactory = factory
        };

        Assert.Same(factory, options.AdapterFactory);
    }

    private class TestAdapterFactory : Factory.ICodeInterpreterAdapterFactory
    {
        public Services.ICodes CreateCodes(Factory.CreateCodesStackOptions options)
        {
            throw new NotImplementedException();
        }
    }
}
