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

using OpenSandbox.Models;

namespace OpenSandbox.Services;

/// <summary>Optional caller-bound creation capability, separate from legacy implementers.</summary>
public interface IExecutionOperations
{
    Task<ExecutionInstance> GetExecutionInstanceAsync(CancellationToken cancellationToken = default);
    Task<ExecutionOperation> GetExecutionOperationAsync(string kind, string operationId, CancellationToken cancellationToken = default);
    Task<ExecutionOperation> CreateCommandOperationAsync(string operationId, string command, RunCommandOptions? options = null, CancellationToken cancellationToken = default);
    Task<ExecutionOperation> CreatePtyOperationAsync(string operationId, string? cwd = null, string? command = null, CancellationToken cancellationToken = default);
}

/// <summary>Access the optional capability without changing the existing command interface.</summary>
public static class ExecutionOperationExtensions
{
    private static IExecutionOperations Operations(IExecdCommands commands)
        => commands as IExecutionOperations ?? throw new NotSupportedException("Execution creation recovery is unavailable");

    public static Task<ExecutionInstance> GetExecutionInstanceAsync(this IExecdCommands commands, CancellationToken cancellationToken = default)
        => Operations(commands).GetExecutionInstanceAsync(cancellationToken);

    public static Task<ExecutionOperation> GetExecutionOperationAsync(this IExecdCommands commands, string kind, string operationId, CancellationToken cancellationToken = default)
        => Operations(commands).GetExecutionOperationAsync(kind, operationId, cancellationToken);

    public static Task<ExecutionOperation> CreateCommandOperationAsync(this IExecdCommands commands, string operationId, string command, RunCommandOptions? options = null, CancellationToken cancellationToken = default)
        => Operations(commands).CreateCommandOperationAsync(operationId, command, options, cancellationToken);

    public static Task<ExecutionOperation> CreatePtyOperationAsync(this IExecdCommands commands, string operationId, string? cwd = null, string? command = null, CancellationToken cancellationToken = default)
        => Operations(commands).CreatePtyOperationAsync(operationId, cwd, command, cancellationToken);
}
