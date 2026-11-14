# Copyright 2026 The OpenSandbox Authors
# SPDX-License-Identifier: Apache-2.0

"""Run the checked-out FastPath metadata validator without its optional stack.

This fixture compiles the original AST nodes, rather than copying the rules.
It exercises only _validate_metadata, not API models, protobuf mapping or a live
server. Both create mappings must still call that validator in the source.
"""

import ast
import hashlib
import json
from pathlib import Path
import re
import sys


source_path = Path(sys.argv[1])
source = source_path.read_bytes()
tree = ast.parse(source, filename=str(source_path))
names = {
    "UnsupportedFieldError",
    "_DNS_LABEL_PATTERN",
    "_LABEL_VALUE_PATTERN",
    "_MAX_LABEL_LENGTH",
    "_validate_metadata",
}
selected = []
found = set()
for node in tree.body:
    if isinstance(node, (ast.ClassDef, ast.FunctionDef)):
        name = node.name
    elif isinstance(node, ast.Assign) and len(node.targets) == 1 and isinstance(node.targets[0], ast.Name):
        name = node.targets[0].id
    else:
        continue
    if name in names:
        selected.append(node)
        found.add(name)
if found != names:
    raise ValueError(f"Validator extraction changed: {found}")

mappings = ["map_create_request", "map_template_create_request"]
for name in mappings:
    mapping = next(node for node in tree.body if isinstance(node, ast.FunctionDef) and node.name == name)
    if not any(
        isinstance(node, ast.Call)
        and isinstance(node.func, ast.Name)
        and node.func.id == "_validate_metadata"
        for node in ast.walk(mapping)
    ):
        raise ValueError(f"{name} no longer calls the validator")

namespace = {"re": re}
exec(compile(ast.Module(body=selected, type_ignores=[]), str(source_path), "exec"), namespace)
result = {"sourceSha256": hashlib.sha256(source).hexdigest(), "mappings": mappings}
try:
    namespace["_validate_metadata"](json.load(sys.stdin))
    result["accepted"] = True
except namespace["UnsupportedFieldError"] as error:
    result.update(accepted=False, field=error.field, reason=error.reason)
print(json.dumps(result))
