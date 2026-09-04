import pytest
from unittest.mock import AsyncMock, MagicMock, patch
from opensandbox_mcp.server import create_server 

@pytest.mark.asyncio
@patch("opensandbox_mcp.server.Sandbox")
async def test_sandbox_create_with_volumes(mock_sandbox):
    mock_instance = MagicMock()
    mock_instance.id = "sbx_mock_test_123"
    mock_instance.get_info = AsyncMock()
    mock_sandbox.create = AsyncMock(return_value=mock_instance)
    
    mcp_app = create_server()
    tool_name = "sandbox_create"
    target_func = None
    
    if hasattr(mcp_app, "_tool_manager"):
        tool_obj = mcp_app._tool_manager.get_tool(tool_name)
        if tool_obj:
            target_func = getattr(tool_obj, "fn", None)
        
    if not target_func:
        raise ValueError(f"Could not locate '{tool_name}' inner function within the FastMCP tool manager.")

    with patch("opensandbox_mcp.server.SandboxInfoResponse") as mock_info_response:
        mock_response_instance = MagicMock()
        mock_response_instance.sandbox_id = "sbx_mock_test_123"
        mock_info_response.return_value = mock_response_instance

        result = await target_func(
            image="ghcr.io/agent-infra/sandbox:latest",
            host_path="/var/lib/sandboxes/workspaces",
            mount_path="/workspace"
        )
    
    mock_sandbox.create.assert_called_once()
    executed_kwargs = mock_sandbox.create.call_args[1]
    
    assert "volumes" in executed_kwargs
    assert executed_kwargs["volumes"] is not None
    assert len(executed_kwargs["volumes"]) == 1
    
    volume_spec = executed_kwargs["volumes"][0]
    assert volume_spec.name == "mcp-persistent-storage"
    assert volume_spec.host.path == "/var/lib/sandboxes/workspaces"
    assert volume_spec.mount_path == "/workspace"
    assert result.sandbox_id == "sbx_mock_test_123"

@pytest.mark.asyncio
@patch("opensandbox_mcp.server.Sandbox")
async def test_sandbox_create_partial_volumes_raises_error(mock_sandbox):
    mcp_app = create_server()
    tool_name = "sandbox_create"
    target_func = None
    
    if hasattr(mcp_app, "_tool_manager"):
        tool_obj = mcp_app._tool_manager.get_tool(tool_name)
        if tool_obj:
            target_func = getattr(tool_obj, "fn", None)

    with pytest.raises(ValueError, match="Both 'host_path' and 'mount_path' must be provided together"):
        await target_func(
            image="ghcr.io/agent-infra/sandbox:latest",
            host_path="/var/lib/sandboxes/workspaces",
            mount_path=None  # Triggers the exclusive-OR partial specification error
        )