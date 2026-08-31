#
# Copyright 2026 Alibaba Group Holding Ltd.
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
#

from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..models.capabilities_response_hardening_init_mode import CapabilitiesResponseHardeningInitMode
from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.hardening_layer_state import HardeningLayerState


T = TypeVar("T", bound="CapabilitiesResponseHardening")


@_attrs_define
class CapabilitiesResponseHardening:
    """execd init-mode and workload-hardening state (OSEP-0018): whether execd is the sandbox init and which of its
    controls are in effect. Not an isolation capability; reported here so operators see enforcement state in one place.

        Attributes:
            init_mode (CapabilitiesResponseHardeningInitMode | Unset): How execd supervises the sandbox process tree. pid1:
                execd is the kernel init of the container. subreaper: execd reaps orphans but lacks the PID 1 kernel signal
                shield. none: init mode is off (default).
            signal_shield (bool | Unset): Whether the kernel PID 1 signal shield protects execd from in-namespace signals
                (true only in init_mode pid1).
            cap_drop (HardeningLayerState | Unset): Whether one hardening layer is actually enforced. state is "active" |
                "disabled" (not configured) | "degraded" (configured but a prerequisite is missing) | "unsupported"
                (kernel/build cannot provide it). message gives the concrete reason whenever state is not active.
            seccomp (HardeningLayerState | Unset): Whether one hardening layer is actually enforced. state is "active" |
                "disabled" (not configured) | "degraded" (configured but a prerequisite is missing) | "unsupported"
                (kernel/build cannot provide it). message gives the concrete reason whenever state is not active.
            landlock (HardeningLayerState | Unset): Whether one hardening layer is actually enforced. state is "active" |
                "disabled" (not configured) | "degraded" (configured but a prerequisite is missing) | "unsupported"
                (kernel/build cannot provide it). message gives the concrete reason whenever state is not active.
            ebpf (HardeningLayerState | Unset): Whether one hardening layer is actually enforced. state is "active" |
                "disabled" (not configured) | "degraded" (configured but a prerequisite is missing) | "unsupported"
                (kernel/build cannot provide it). message gives the concrete reason whenever state is not active.
    """

    init_mode: CapabilitiesResponseHardeningInitMode | Unset = UNSET
    signal_shield: bool | Unset = UNSET
    cap_drop: HardeningLayerState | Unset = UNSET
    seccomp: HardeningLayerState | Unset = UNSET
    landlock: HardeningLayerState | Unset = UNSET
    ebpf: HardeningLayerState | Unset = UNSET
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        init_mode: str | Unset = UNSET
        if not isinstance(self.init_mode, Unset):
            init_mode = self.init_mode.value

        signal_shield = self.signal_shield

        cap_drop: dict[str, Any] | Unset = UNSET
        if not isinstance(self.cap_drop, Unset):
            cap_drop = self.cap_drop.to_dict()

        seccomp: dict[str, Any] | Unset = UNSET
        if not isinstance(self.seccomp, Unset):
            seccomp = self.seccomp.to_dict()

        landlock: dict[str, Any] | Unset = UNSET
        if not isinstance(self.landlock, Unset):
            landlock = self.landlock.to_dict()

        ebpf: dict[str, Any] | Unset = UNSET
        if not isinstance(self.ebpf, Unset):
            ebpf = self.ebpf.to_dict()

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update({})
        if init_mode is not UNSET:
            field_dict["init_mode"] = init_mode
        if signal_shield is not UNSET:
            field_dict["signal_shield"] = signal_shield
        if cap_drop is not UNSET:
            field_dict["cap_drop"] = cap_drop
        if seccomp is not UNSET:
            field_dict["seccomp"] = seccomp
        if landlock is not UNSET:
            field_dict["landlock"] = landlock
        if ebpf is not UNSET:
            field_dict["ebpf"] = ebpf

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.hardening_layer_state import HardeningLayerState

        d = dict(src_dict)
        _init_mode = d.pop("init_mode", UNSET)
        init_mode: CapabilitiesResponseHardeningInitMode | Unset
        if isinstance(_init_mode, Unset):
            init_mode = UNSET
        else:
            init_mode = CapabilitiesResponseHardeningInitMode(_init_mode)

        signal_shield = d.pop("signal_shield", UNSET)

        _cap_drop = d.pop("cap_drop", UNSET)
        cap_drop: HardeningLayerState | Unset
        if isinstance(_cap_drop, Unset):
            cap_drop = UNSET
        else:
            cap_drop = HardeningLayerState.from_dict(_cap_drop)

        _seccomp = d.pop("seccomp", UNSET)
        seccomp: HardeningLayerState | Unset
        if isinstance(_seccomp, Unset):
            seccomp = UNSET
        else:
            seccomp = HardeningLayerState.from_dict(_seccomp)

        _landlock = d.pop("landlock", UNSET)
        landlock: HardeningLayerState | Unset
        if isinstance(_landlock, Unset):
            landlock = UNSET
        else:
            landlock = HardeningLayerState.from_dict(_landlock)

        _ebpf = d.pop("ebpf", UNSET)
        ebpf: HardeningLayerState | Unset
        if isinstance(_ebpf, Unset):
            ebpf = UNSET
        else:
            ebpf = HardeningLayerState.from_dict(_ebpf)

        capabilities_response_hardening = cls(
            init_mode=init_mode,
            signal_shield=signal_shield,
            cap_drop=cap_drop,
            seccomp=seccomp,
            landlock=landlock,
            ebpf=ebpf,
        )

        capabilities_response_hardening.additional_properties = d
        return capabilities_response_hardening

    @property
    def additional_keys(self) -> list[str]:
        return list(self.additional_properties.keys())

    def __getitem__(self, key: str) -> Any:
        return self.additional_properties[key]

    def __setitem__(self, key: str, value: Any) -> None:
        self.additional_properties[key] = value

    def __delitem__(self, key: str) -> None:
        del self.additional_properties[key]

    def __contains__(self, key: str) -> bool:
        return key in self.additional_properties
