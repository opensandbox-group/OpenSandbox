# CCS Security Guard for OpenSandbox

This example demonstrates how to integrate [CCS (Credential & Compliance Standard)](https://github.com/Correctover/ccs-verifier)
with OpenSandbox for runtime command verification.

## What is CCS?

CCS is an IETF-standardized runtime verification framework that provides:
- **RCE Protection**: Detects dangerous shell commands (rm -rf, chmod 777, fork bombs)
- **SSRF Prevention**: Blocks requests to internal/cloud metadata endpoints
- **Credential Leak Detection**: Prevents exposure of secrets and API keys
- **Sub-millisecond Overhead**: P50 ≈ 7.5μs in-process verification

## Why CCS for OpenSandbox?

OpenSandbox provides secure execution environments for AI agents. Adding CCS verification
provides an additional layer of security by validating commands before they reach the sandbox:

1. **Pre-sandbox validation**: Block dangerous commands before they consume sandbox resources
2. **Audit trail**: Cryptographic receipts for every command verification
3. **Compliance**: IETF-standardized security checks

## Quick Start

```python
from ccs_verifier import Verifier, Command
from ccs_verifier.builtin_rules import RCERule, SSRFRule, CredentialLeakRule
from opensandbox import Sandbox

# Initialize CCS verifier
rules = [RCERule(), SSRFRule(), CredentialLeakRule()]
verifier = Verifier(rules=rules)

# Create sandbox
sandbox = Sandbox.create()

# Verify command before execution
cmd = Command(agent_id="opensandbox", tool="shell", params={"command": "ls -la"})
result = verifier.verify(cmd)

if result.verdict.value == "allow":
    # Safe to execute in sandbox
    execution = sandbox.commands.run("ls -la")
    print(execution.stdout)
else:
    print(f"Command blocked: {result.reason}")
```

## Standards & References

- [CCS IETF Draft](https://datatracker.ietf.org/doc/draft-correctover-ccs/)
- [CCS PyPI Package](https://pypi.org/project/ccs-verifier/)
- [Zenodo DOI: 10.5281/zenodo.21783723](https://doi.org/10.5281/zenodo.21783723)
