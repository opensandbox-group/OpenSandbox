# Copyright 2025 The OpenSandbox Authors
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

import pytest
import yaml

from opensandbox_server.services.k8s.batchsandbox_template import BatchSandboxTemplateManager

class TestBatchSandboxTemplateManager:
    
    def test_load_valid_yaml_template_successfully(self, tmp_path):
        template_file = tmp_path / "valid_template.yaml"
        template_content = {
            "metadata": {"annotations": {"test": "value"}},
            "spec": {"template": {"spec": {"nodeSelector": {"env": "test"}}}}
        }
        template_file.write_text(yaml.dump(template_content))
        
        manager = BatchSandboxTemplateManager(str(template_file))
        
        assert manager._template == template_content
        assert manager.template_file_path == str(template_file)
    
    def test_load_nonexistent_file_raises_error(self):
        with pytest.raises(FileNotFoundError) as exc_info:
            BatchSandboxTemplateManager("/path/to/nonexistent.yaml")
        
        assert "not found" in str(exc_info.value)
    
    def test_load_invalid_yaml_raises_error(self, tmp_path):
        template_file = tmp_path / "invalid.yaml"
        template_file.write_text("invalid: yaml: [missing: bracket")

        with pytest.raises(RuntimeError) as exc_info:
            BatchSandboxTemplateManager(str(template_file))
        
        assert "Failed to load" in str(exc_info.value)
    
    def test_load_non_dict_yaml_raises_error(self, tmp_path):
        template_file = tmp_path / "list.yaml"
        template_file.write_text("- item1\n- item2")

        with pytest.raises(ValueError) as exc_info:
            BatchSandboxTemplateManager(str(template_file))
        
        assert "must be a YAML object" in str(exc_info.value)
        assert "got list" in str(exc_info.value)
    
    def test_init_without_template_file_creates_empty_manager(self):
        manager = BatchSandboxTemplateManager(None)
        
        assert manager._template is None
        assert manager.template_file_path is None
    
    def test_merge_with_runtime_values_without_template(self):
        manager = BatchSandboxTemplateManager(None)
        runtime_manifest = {"spec": {"replicas": 1}}
        
        result = manager.merge_with_runtime_values(runtime_manifest)
        
        assert result == runtime_manifest
    
    def test_merge_with_runtime_values_with_template(self, tmp_path):
        template_file = tmp_path / "template.yaml"
        template_content = {
            "spec": {
                "template": {
                    "spec": {
                        "nodeSelector": {"workload": "sandbox"},
                        "tolerations": [{"operator": "Exists"}]
                    }
                }
            }
        }
        template_file.write_text(yaml.dump(template_content))
        
        manager = BatchSandboxTemplateManager(str(template_file))

        runtime_manifest = {
            "spec": {
                "replicas": 1,
                "template": {
                    "spec": {
                        "containers": [{"name": "test"}],
                        "volumes": [{"name": "vol"}]
                    }
                }
            }
        }
        
        result = manager.merge_with_runtime_values(runtime_manifest)

        assert result["spec"]["template"]["spec"]["nodeSelector"] == {"workload": "sandbox"}
        assert result["spec"]["template"]["spec"]["tolerations"] == [{"operator": "Exists"}]
        assert result["spec"]["replicas"] == 1
        assert result["spec"]["template"]["spec"]["containers"] == [{"name": "test"}]
        assert result["spec"]["template"]["spec"]["volumes"] == [{"name": "vol"}]
