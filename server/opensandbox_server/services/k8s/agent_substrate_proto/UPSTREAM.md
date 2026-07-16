# Agent Substrate API Pin

Source repository: <https://github.com/agent-substrate/substrate>

Commit: `c1ab0958f85202faf9f87ea66cd98903a9de763b`

Source file: `pkg/proto/ateapipb/ateapi.proto`

Regenerate the Python client from the `server/` directory:

```bash
uv run python -m grpc_tools.protoc \
  -I . \
  --python_out=. \
  --pyi_out=. \
  --grpc_python_out=. \
  opensandbox_server/services/k8s/agent_substrate_proto/ateapi.proto
```

Do not edit `ateapi_pb2.py` or `ateapi_pb2_grpc.py` manually.