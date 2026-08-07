"""
CCS Security Guard for OpenSandbox

Provides runtime command verification before sandbox execution.
Integrates CCS (Credential & Compliance Standard) to prevent dangerous commands.
"""

import logging
from typing import Tuple, Optional

try:
    from ccs_verifier import Verifier, Command
    from ccs_verifier.builtin_rules import RCERule, SSRFRule, CredentialLeakRule
    CCS_AVAILABLE = True
except ImportError:
    CCS_AVAILABLE = False
    logging.warning("ccs-verifier not installed. Install with: pip install ccs-verifier")

logger = logging.getLogger(__name__)

_verifier: Optional[Verifier] = None


def get_verifier() -> Optional[Verifier]:
    """Lazy-initialize the CCS verifier."""
    global _verifier
    if _verifier is None and CCS_AVAILABLE:
        rules = [RCERule(), SSRFRule(), CredentialLeakRule()]
        _verifier = Verifier(rules=rules)
        logger.info("CCS security guard initialized for OpenSandbox")
    return _verifier


def verify_command(command: str, agent_id: str = "opensandbox") -> Tuple[bool, str]:
    """
    Verify a command using CCS security rules before sandbox execution.
    
    Args:
        command: The shell command to verify
        agent_id: Identifier for the agent executing the command
        
    Returns:
        Tuple of (allowed: bool, reason: str)
        - If allowed is True, command is safe to execute in sandbox
        - If allowed is False, reason explains why it was blocked
    """
    if not CCS_AVAILABLE:
        logger.warning("ccs-verifier not installed, skipping security check")
        return True, "CCS not available"
    
    verifier = get_verifier()
    if verifier is None:
        return True, "Verifier initialization failed"
    
    try:
        cmd = Command(
            agent_id=agent_id,
            tool="shell",
            params={"command": command}
        )
        result = verifier.verify(cmd)
        
        if result.verdict.value == "deny":
            reason = getattr(result, "reason", "CCS security policy violation")
            logger.warning(f"CCS blocked command: {command[:80]}... Reason: {reason}")
            return False, reason
        
        return True, "CCS verified safe"
        
    except Exception as e:
        logger.error(f"CCS verification error: {e}")
        return True, f"CCS verification error: {e}"


def demo():
    """Demonstrate CCS security guard for OpenSandbox."""
    print("=" * 60)
    print("CCS Security Guard for OpenSandbox - Demo")
    print("=" * 60)
    
    test_commands = [
        ("ls -la", "List files"),
        ("python script.py", "Run Python script"),
        ("rm -rf /", "Destructive RCE"),
        ("curl http://169.254.169.254/latest/meta-data/", "AWS metadata SSRF"),
        ("echo $AWS_SECRET_ACCESS_KEY", "Credential leak"),
    ]
    
    for cmd, description in test_commands:
        allowed, reason = verify_command(cmd)
        status = "✓ ALLOW" if allowed else "✗ DENY"
        print(f"{status} | {description:<30} | {cmd[:40]}")
        if not allowed:
            print(f"       Reason: {reason}")
    
    print("\n" + "=" * 60)
    print("Integration with OpenSandbox:")
    print("""
    from opensandbox import Sandbox
    from ccs_guard import verify_command
    
    sandbox = Sandbox.create()
    command = "ls -la"
    
    allowed, reason = verify_command(command)
    if allowed:
        execution = sandbox.commands.run(command)
        print(execution.stdout)
    else:
        print(f"Command blocked: {reason}")
    """)


if __name__ == "__main__":
    demo()
