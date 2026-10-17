// Copyright 2026 The OpenSandbox Authors
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

using System;
using System.Collections.Concurrent;
using System.Collections.Generic;
using System.IO;
using System.Net;
using System.Net.Http;
using System.Net.Sockets;
using System.Threading;
using System.Threading.Tasks;
using OpenSandbox.Config;

class Program
{
    static readonly string[] Headers = { "OPEN-SANDBOX-API-KEY", "oPeNsAnDbOx-Secure-Access", "X-EXECD-ACCESS-TOKEN", "X-API-Key" };
    static async Task<int> Main()
    {
        Console.WriteLine("Actual runtime: " + System.Runtime.InteropServices.RuntimeInformation.FrameworkDescription);
        Console.WriteLine("SDK asset: " + System.Reflection.CustomAttributeExtensions.GetCustomAttribute<System.Runtime.Versioning.TargetFrameworkAttribute>(typeof(ConnectionConfig).Assembly)?.FrameworkName);
        int failures = 0;
        foreach (bool sse in new[] { false, true })
            foreach (bool sync in new[] { false, true })
                foreach (string path in new[] { "same", "cross", "return" })
                {
                    string label = $"sse={sse} sync={sync} path={path}";
                    try { await Run(sse, sync, path); Console.WriteLine("PASS " + label); }
                    catch (Exception e) { failures++; Console.WriteLine("FAIL " + label + ": " + e.Message); }
                }
        Console.WriteLine($"Results: {12 - failures} passed, {failures} failed");
        return failures == 0 ? 0 : 1;
    }
    static async Task Run(bool sse, bool sync, string path)
    {
        string sourceUrl = null;
        using var target = new Server(_ => path == "return" ? Redirect(sourceUrl + "/returned") : Ok, 1);
        using var source = new Server(requestPath => requestPath == "/start"
            ? Redirect(path == "same" ? "/final" : target.Url + "/away") : Ok, path == "cross" ? 1 : 2);
        sourceUrl = source.Url;
        var headers = new Dictionary<string, string>();
        foreach (var name in Headers) headers[name] = "synthetic-token";
        headers["X-Request-ID"] = "synthetic-trace";
        var config = new ConnectionConfig(new ConnectionConfigOptions { Domain = source.Url, Headers = headers, RequestTimeoutSeconds = 5 });
        using var client = sse ? config.CreateSseHttpClient() : config.CreateHttpClient();
        using var request = new HttpRequestMessage(HttpMethod.Get, source.Url + "/start");
        using var timeout = new CancellationTokenSource(TimeSpan.FromSeconds(5));
        using var response = sync ? client.Send(request, timeout.Token) : await client.SendAsync(request, timeout.Token);
        Check(response.StatusCode == HttpStatusCode.OK, "expected 200, got " + response.StatusCode);
        await AwaitCompletion(source.Completion);
        if (path != "same") await AwaitCompletion(target.Completion);
        var sent = source.Captured.ToArray();
        AssertHeaders(sent[0], true);
        if (path == "same") AssertHeaders(sent[1], true);
        else
        {
            AssertHeaders(target.Captured.ToArray()[0], false);
            if (path == "return") AssertHeaders(sent[1], false);
        }
        foreach (var name in Headers) Check(client.DefaultRequestHeaders.Contains(name), "default header mutated");
    }
    static void AssertHeaders(Dictionary<string, string> captured, bool expected)
    {
        foreach (var name in Headers) Check(captured.ContainsKey(name) == expected, name + " presence differed");
        Check(captured["X-Request-ID"] == "synthetic-trace", "trace lost");
    }
    static async Task AwaitCompletion(Task completion)
    {
        if (await Task.WhenAny(completion, Task.Delay(TimeSpan.FromSeconds(5))) != completion)
        {
            throw new TimeoutException("Expected loopback requests were not received");
        }
        await completion;
    }
    static void Check(bool condition, string message) { if (!condition) throw new Exception(message); }
    const string Ok = "HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n";
    static string Redirect(string uri) => "HTTP/1.1 307 Temporary Redirect\r\nLocation: " + uri + "\r\nContent-Length: 0\r\nConnection: close\r\n\r\n";
    sealed class Server : IDisposable
    {
        readonly TcpListener listener = new TcpListener(IPAddress.Loopback, 0);
        public string Url { get; }
        public Task Completion { get; }
        public ConcurrentQueue<Dictionary<string, string>> Captured { get; } = new ConcurrentQueue<Dictionary<string, string>>();
        public Server(Func<string, string> response, int count)
        {
            listener.Start();
            Url = "http://127.0.0.1:" + ((IPEndPoint)listener.LocalEndpoint).Port;
            Completion = Task.Run(() =>
            {
                try
                {
                    for (int i = 0; i < count; i++)
                    {
                        using var connection = listener.AcceptTcpClient();
                        using var stream = connection.GetStream();
                        stream.ReadTimeout = 5000;
                        stream.WriteTimeout = 5000;
                        using var reader = new StreamReader(stream, System.Text.Encoding.ASCII, false, 1024, true);
                        var line = reader.ReadLine();
                        string path = line.Split(' ')[1];
                        var headers = new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase);
                        while (!string.IsNullOrEmpty(line = reader.ReadLine()))
                        {
                            int colon = line.IndexOf(':');
                            headers.Add(line.Substring(0, colon), line.Substring(colon + 1).Trim());
                        }
                        Captured.Enqueue(headers);
                        byte[] bytes = System.Text.Encoding.ASCII.GetBytes(response(path));
                        stream.Write(bytes, 0, bytes.Length);
                    }
                }
                catch (SocketException) { }
                catch (ObjectDisposedException) { }
            });
        }
        public void Dispose() { listener.Stop(); Completion.GetAwaiter().GetResult(); }
    }
}
