# Copyright 2026 The OpenSandbox Authors
# SPDX-License-Identifier: Apache-2.0

"""Fault injection: lose membership while leaving the decrypted socket open."""

import subject_admission


def tls_established_client(data):
    subject_admission.runtime().client_disconnected(data.context.client)
