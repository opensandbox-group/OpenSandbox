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

using OpenSandbox.Models;

namespace OpenSandbox.Adapters;

/// <summary>
/// Client-side shape gate for Credential Vault writes (OSEP-0025), shared by
/// the sidecar-routed and lifecycle-routed vault transports: sources and auths
/// mixing the inline and exchange variants fail fast here instead of reaching
/// the wire. Endpoint semantics and the static-kind field lock stay
/// server-owned.
/// </summary>
internal static class CredentialVaultShapeGuard
{
    public static void ValidateCredentials(IEnumerable<Credential>? credentials)
    {
        if (credentials == null)
        {
            return;
        }

        foreach (var credential in credentials)
        {
            ValidateSource(credential?.Source);
        }
    }

    public static void ValidateBindings(IEnumerable<CredentialBinding>? bindings)
    {
        if (bindings == null)
        {
            return;
        }

        foreach (var binding in bindings)
        {
            ValidateAuth(binding?.Auth);
        }
    }

    public static void ValidatePatchRequest(CredentialVaultPatchRequest request)
    {
        ValidateCredentials(request.Credentials?.Add);
        ValidateCredentials(request.Credentials?.Replace);
        ValidateBindings(request.Bindings?.Add);
        ValidateBindings(request.Bindings?.Replace);
    }

    private static void ValidateSource(CredentialSource? source)
    {
        if (source == null)
        {
            throw new ArgumentException("Credential source must be specified.");
        }

        if (source.Type != "inline" && source.Type != "exchange")
        {
            throw new ArgumentException("Credential source type must be \"inline\" or \"exchange\".");
        }

        if (source.Type == "inline")
        {
            if (string.IsNullOrWhiteSpace(source.Value))
            {
                throw new ArgumentException("An inline credential source requires a value.");
            }
            if (source.Endpoint != null)
            {
                throw new ArgumentException("An inline credential source cannot set an endpoint.");
            }
            return;
        }

        if (string.IsNullOrWhiteSpace(source.Endpoint))
        {
            throw new ArgumentException("An exchange credential source requires an endpoint.");
        }
        if (source.Value != null)
        {
            throw new ArgumentException("An exchange credential source cannot set a value.");
        }
    }

    private static void ValidateAuth(CredentialAuth? auth)
    {
        if (auth == null || auth.Type != null)
        {
            return;
        }

        if (string.IsNullOrWhiteSpace(auth.Credential))
        {
            throw new ArgumentException("Credential auth requires either a static type or an exchange credential reference.");
        }
        if (auth.Name != null || auth.Headers != null || auth.Substitutions != null)
        {
            throw new ArgumentException("The typeless exchange auth form carries only a credential reference; 'name', 'headers', and 'substitutions' require a static type.");
        }
    }
}
