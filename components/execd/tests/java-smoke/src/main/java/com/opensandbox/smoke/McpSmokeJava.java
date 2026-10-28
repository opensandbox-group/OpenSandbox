// Copyright 2025 The OpenSandbox Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package com.opensandbox.smoke;

import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.time.Duration;
import java.util.Map;

import io.modelcontextprotocol.client.McpClient;
import io.modelcontextprotocol.client.transport.HttpClientStreamableHttpTransport;
import io.modelcontextprotocol.spec.McpSchema;

/**
 * Smoke test for the execd MCP proxy bridge using the official java SDK.
 *
 * Env: BASE_URL (execd), API_TOKEN (execd access token).
 */
public final class McpSmokeJava {

    private static String baseUrl = env("BASE_URL", "http://127.0.0.1:44772");
    private static final String API_TOKEN = env("API_TOKEN", "");

    private static void expect(boolean condition, String message) {
        if (!condition) {
            throw new AssertionError("FAIL: " + message);
        }
        System.out.println("ok: " + message);
    }

    private static void drive(String name, String toolName, Map<String, Object> args, String expectedText) {
        var transportBuilder = HttpClientStreamableHttpTransport.builder(baseUrl)
            .endpoint("/mcp/" + name)
            .addHttpRequestCustomizer((builder, method, uri, method1, context) ->
                builder.header("X-EXECD-ACCESS-TOKEN", API_TOKEN));

        var client = McpClient.sync(transportBuilder.build())
            .requestTimeout(Duration.ofSeconds(30))
            .build();

        try {
            var initResult = client.initialize();
            expect(initResult != null && initResult.serverInfo() != null,
                name + ": initialize returned server info");

            var tools = client.listTools();
            expect(tools.tools().stream().anyMatch(t -> t.name().equals(toolName)),
                name + ": tools/list contains " + toolName);

            var result = client.callTool(new McpSchema.CallToolRequest(toolName, args));
            String text = result.content().get(0).toString();
            expect(text.contains(expectedText),
                name + ": callTool(" + toolName + ") contains " + expectedText);
        } finally {
            client.close();
        }
    }

    private static void expectUnauthorized() {
        try {
            HttpClient client = HttpClient.newHttpClient();
            HttpRequest request = HttpRequest.newBuilder()
                .uri(URI.create(baseUrl + "/mcp/smoke-http"))
                .header("Content-Type", "application/json")
                .POST(HttpRequest.BodyPublishers.ofString(
                    "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/list\"}"))
                .build();
            HttpResponse<String> response = client.send(request, HttpResponse.BodyHandlers.ofString());
            expect(response.statusCode() == 401, "unauthenticated MCP request rejected with 401");
        } catch (Exception e) {
            throw new AssertionError("FAIL: 401 check errored: " + e.getMessage(), e);
        }
    }

    public static void main(String[] args) {
        drive("smoke-http", "add", Map.of("a", 6, "b", 7), "13");
        drive("smoke-stdio", "echo", Map.of("message", "hello-from-java"), "hello-from-java");
        if (!API_TOKEN.isEmpty()) {
            expectUnauthorized();
        }
        System.out.println("ALL JAVA MCP SMOKE TESTS PASSED");
    }

    private static String env(String key, String fallback) {
        String value = System.getenv(key);
        return (value == null || value.isEmpty()) ? fallback : value;
    }

    private McpSmokeJava() {
    }
}
