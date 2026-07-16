import assert from "node:assert/strict";
import test from "node:test";

import { EgressAdapter } from "../dist/internal.js";

const capabilities = {
    protocolVersion: "extensions.opensandbox.io/v1alpha1",
    resources: [
        {
            apiVersion: "gateway.networking.k8s.io/v1",
            kind: "HTTPRoute",
            available: true,
            operations: ["read", "replace"],
            features: ["gateway.networking.k8s.io/HTTPRouteMethodMatch"],
        },
        {
            apiVersion: "gateway.networking.k8s.io/v1alpha2",
            kind: "TLSRoute",
            available: false,
            operations: [],
            reason: "experimental CRD is not installed",
        },
    ],
};

const extensionResource = {
    apiVersion: "gateway.networking.k8s.io/v1",
    kind: "HTTPRoute",
    metadata: {
        name: "api-example",
        namespace: "sandboxes",
        labels: {
            "sandbox.opensandbox.io/sandbox": "sandbox-1",
        },
        futureMetadata: {
            preserved: true,
        },
    },
    spec: {
        hostnames: ["api.example.com"],
        rules: [
            {
                matches: [{ method: "GET" }],
                backendRefs: [
                    {
                        group: "agentgateway.dev",
                        kind: "AgentgatewayBackend",
                        name: "original-destination",
                    },
                ],
            },
        ],
        futureField: {
            preserved: [1, 2, 3],
        },
    },
};

function extensionState(revision) {
    return {
        revision,
        resources: [extensionResource],
        conditions: [
            {
                type: "Accepted",
                status: "True",
                reason: "Valid",
                observedRevision: revision,
            },
            {
                type: "Programmed",
                status: "True",
                reason: "Acknowledged",
                observedRevision: revision,
                resource: {
                    apiVersion: extensionResource.apiVersion,
                    kind: extensionResource.kind,
                    namespace: extensionResource.metadata.namespace,
                    name: extensionResource.metadata.name,
                },
            },
        ],
    };
}

test("EgressAdapter discovers and round-trips versioned extension resources", async () => {
    const requests = [];
    const client = {
        async GET(path) {
            requests.push({ method: "GET", path });
            const data = path === "/capabilities" ? capabilities : extensionState(7);
            return { data, response: new Response(null, { status: 200 }) };
        },
        async PUT(path, options) {
            requests.push({ method: "PUT", path, body: options.body });
            return { data: extensionState(8), response: new Response(null, { status: 200 }) };
        },
    };
    const adapter = new EgressAdapter(client);

    const discovered = await adapter.getCapabilities();
    const current = await adapter.getExtensions();
    const replaced = await adapter.replaceExtensions(current.resources, current.revision);

    assert.deepEqual(discovered, capabilities);
    assert.deepEqual(current, extensionState(7));
    assert.deepEqual(replaced, extensionState(8));
    assert.deepEqual(requests, [
        { method: "GET", path: "/capabilities" },
        { method: "GET", path: "/extensions" },
        {
            method: "PUT",
            path: "/extensions",
            body: {
                expectedRevision: 7,
                resources: [extensionResource],
            },
        },
    ]);
    assert.deepEqual(
        requests[2].body.resources[0].metadata.futureMetadata,
        extensionResource.metadata.futureMetadata,
    );
    assert.deepEqual(
        requests[2].body.resources[0].spec.futureField,
        extensionResource.spec.futureField,
    );
});