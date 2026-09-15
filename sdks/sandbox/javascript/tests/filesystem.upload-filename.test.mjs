import assert from "node:assert/strict";
import test from "node:test";

import { FilesystemAdapter, createExecdClient } from "../dist/internal.js";

const BASE_URL = "http://127.0.0.1:8080";

function createAdapter(capture) {
  const fetchImpl = async (input, init) => {
    const request = input instanceof Request ? input : new Request(input, init);
    capture.contentType = request.headers.get("content-type");
    capture.body = await request.text();
    return new Response("{}", {
      status: 200,
      headers: { "content-type": "application/json" },
    });
  };
  const client = createExecdClient({ baseUrl: BASE_URL, fetch: fetchImpl });
  return new FilesystemAdapter(client, { baseUrl: BASE_URL, fetch: fetchImpl });
}

async function* streamOf(text) {
  yield new TextEncoder().encode(text);
}

// Streams take the hand-built multipart path; in-memory data goes through the
// platform FormData instead.
async function upload(path, data) {
  const capture = {};
  const adapter = createAdapter(capture);
  await adapter.writeFiles([{ path, data }]);
  return capture;
}

function parseUpload(capture) {
  return new Response(capture.body, {
    headers: { "content-type": capture.contentType },
  }).formData();
}

function fileDisposition(capture) {
  return capture.body
    .split("\r\n")
    .find((line) => line.startsWith("Content-Disposition:") && line.includes('name="file"'));
}

test("streamed upload sends a plain filename unchanged", async () => {
  const capture = await upload("/tmp/report.txt", streamOf("hello"));

  const form = await parseUpload(capture);
  assert.equal(form.get("file").name, "report.txt");
  assert.equal(await form.get("file").text(), "hello");
});

test("streamed upload survives a quote in the filename", async () => {
  const capture = await upload('/tmp/re"port.txt', streamOf("hello"));

  const form = await parseUpload(capture);
  assert.ok(form.get("file"), "file part missing from the multipart body");
  assert.equal(await form.get("file").text(), "hello");
});

test("streamed upload survives a line break in the filename", async () => {
  const capture = await upload("/tmp/re\r\nport.txt", streamOf("hello"));

  const form = await parseUpload(capture);
  assert.ok(form.get("file"), "file part missing from the multipart body");
  assert.equal(await form.get("file").text(), "hello");
});

// A backslash opens a quoted-pair, so a trailing one swallows the closing
// delimiter. Node's own parser ignores quoted-pairs and reads the part anyway,
// while Go's `mime/multipart` honours them and drops the part, so this case is
// pinned on the emitted header rather than on a round trip through `formData()`.
test("streamed upload doubles a backslash in the filename", async () => {
  const capture = await upload("/tmp/report\\", streamOf("hello"));

  assert.equal(
    fileDisposition(capture),
    'Content-Disposition: form-data; name="file"; filename="report\\\\"'
  );
});

test("streamed and in-memory uploads agree on the file part header", async () => {
  const path = '/tmp/re"po\r\nrt.txt';

  const streamed = await upload(path, streamOf("hello"));
  const buffered = await upload(path, "hello");

  assert.equal(fileDisposition(streamed), fileDisposition(buffered));
});
