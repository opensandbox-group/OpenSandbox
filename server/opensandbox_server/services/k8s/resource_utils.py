# Copyright 2025 Alibaba Group Holding Ltd.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""
Kubernetes resource calculation utilities.
"""

import re
from typing import Dict


def calculate_resource_requests(limits: Dict[str, str], fraction: float = 0.25) -> Dict[str, str]:
    """
    Calculate resource requests based on limits.
    
    Args:
        limits: Resource limits dict (e.g., {"cpu": "1", "memory": "1Gi"})
        fraction: Fraction of limits to use for requests (default: 0.25 = 1/4)
        
    Returns:
        Resource requests dict with CPU and memory set to fraction of limits.
        Other resources (e.g., gpu) remain unchanged.
    """
    requests = {}
    
    for resource, value in limits.items():
        if resource in ("cpu", "memory"):
            requests[resource] = scale_resource_value(value, fraction)
        else:
            # For other resources (e.g., gpu), keep the same value
            requests[resource] = value
    
    return requests


def scale_resource_value(value: str, factor: float) -> str:
    """
    Scale a resource value by the given factor.
    
    Args:
        value: Resource value string (e.g., "500m", "1Gi", "2")
        factor: Scaling factor (e.g., 0.25 for 1/4)
        
    Returns:
        Scaled resource value string
    """
    # Parse CPU values
    if isinstance(value, (int, float)):
        scaled = float(value) * factor
        # Return integer if whole number, otherwise float with reasonable precision
        if scaled == int(scaled):
            return str(int(scaled))
        return f"{scaled:.3g}"
    
    value_str = str(value)
    
    # Handle CPU millicores (e.g., "500m")
    cpu_match = re.match(r'^(\d+(?:\.\d+)?)m$', value_str)
    if cpu_match:
        millicores = float(cpu_match.group(1))
        scaled_millicores = millicores * factor
        # Return as integer millicores if >= 1, otherwise use cores
        if scaled_millicores >= 1:
            return f"{int(round(scaled_millicores))}m"
        else:
            # Convert to cores (e.g., 0.25)
            cores = scaled_millicores / 1000
            return f"{cores:.3g}"
    
    # Handle CPU cores without unit (e.g., "1", "0.5")
    cpu_cores_match = re.match(r'^(\d+(?:\.\d+)?)$', value_str)
    if cpu_cores_match:
        cores = float(cpu_cores_match.group(1))
        scaled_cores = cores * factor
        if scaled_cores == int(scaled_cores):
            return str(int(scaled_cores))
        return f"{scaled_cores:.3g}"
    
    # Handle memory with units (e.g., "512Mi", "1Gi", "1024Ki")
    memory_match = re.match(r'^(\d+(?:\.\d+)?)(Ki|Mi|Gi|Ti|Pi|Ei)$', value_str)
    if memory_match:
        amount = float(memory_match.group(1))
        unit = memory_match.group(2)
        scaled_amount = amount * factor
        
        # Try to keep the same unit if result is >= 1
        if scaled_amount >= 1:
            if scaled_amount == int(scaled_amount):
                return f"{int(scaled_amount)}{unit}"
            return f"{scaled_amount:.0f}{unit}"
        else:
            # Convert to smaller unit
            unit_map = {"Ki": 1024, "Mi": 1024, "Gi": 1024, "Ti": 1024, "Pi": 1024, "Ei": 1024}
            units = ["Ki", "Mi", "Gi", "Ti", "Pi", "Ei"]
            current_idx = units.index(unit)
            
            # Convert to next smaller unit
            if current_idx > 0:
                smaller_unit = units[current_idx - 1]
                converted_amount = scaled_amount * unit_map[unit]
                if converted_amount == int(converted_amount):
                    return f"{int(converted_amount)}{smaller_unit}"
                return f"{converted_amount:.0f}{smaller_unit}"
            else:
                # Already at smallest unit (Ki), return as is
                return f"{scaled_amount:.0f}{unit}"
    
    # Handle plain numbers (no unit)
    try:
        num_value = float(value_str)
        scaled_value = num_value * factor
        if scaled_value == int(scaled_value):
            return str(int(scaled_value))
        return f"{scaled_value:.3g}"
    except ValueError:
        # If parsing fails, return original value
        return value_str
