#
# Copyright 2026 The OpenSandbox Authors
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
from typing import Any, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..models.egress_signing_jwk_alg import EgressSigningJwkAlg
from ..models.egress_signing_jwk_crv import EgressSigningJwkCrv
from ..models.egress_signing_jwk_kty import EgressSigningJwkKty
from ..models.egress_signing_jwk_use import EgressSigningJwkUse

T = TypeVar("T", bound="EgressSigningJwk")


@_attrs_define
class EgressSigningJwk:
    """One egress caller-authentication public key (OSEP-0025). Public
    material only — the matching private keys never leave the egress
    provisioning channels.

        Attributes:
            kty (EgressSigningJwkKty):
            crv (EgressSigningJwkCrv):
            x (str): Raw Ed25519 public key, base64url without padding
            kid (str): Platform key identifier; the JWKS lookup key
            alg (EgressSigningJwkAlg):
            use (EgressSigningJwkUse):
    """

    kty: EgressSigningJwkKty
    crv: EgressSigningJwkCrv
    x: str
    kid: str
    alg: EgressSigningJwkAlg
    use: EgressSigningJwkUse
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        kty = self.kty.value

        crv = self.crv.value

        x = self.x

        kid = self.kid

        alg = self.alg.value

        use = self.use.value

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "kty": kty,
                "crv": crv,
                "x": x,
                "kid": kid,
                "alg": alg,
                "use": use,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        kty = EgressSigningJwkKty(d.pop("kty"))

        crv = EgressSigningJwkCrv(d.pop("crv"))

        x = d.pop("x")

        kid = d.pop("kid")

        alg = EgressSigningJwkAlg(d.pop("alg"))

        use = EgressSigningJwkUse(d.pop("use"))

        egress_signing_jwk = cls(
            kty=kty,
            crv=crv,
            x=x,
            kid=kid,
            alg=alg,
            use=use,
        )

        egress_signing_jwk.additional_properties = d
        return egress_signing_jwk

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
