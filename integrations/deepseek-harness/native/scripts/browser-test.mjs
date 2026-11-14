// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

// Offline Chromium acceptance of the ordinary installed CLI. All sessions and
// prompts enter through visible UI controls, never through a Controller shortcut.
import assert from 'node:assert/strict';
import { mkdir, mkdtemp, readFile, rm, stat, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { assertBrowserEvidence, startBrowserFixture } from './browser-check.mjs';
import { runBrowserUi } from './browser-ui.mjs';
export { runBrowserUi };

const redact = text => String(text).replace(/([?&]token=)[^\s"'<>]+/g, '$1[redacted]');
async function until(label, check) {
  const deadline = Date.now() + 60_000;
  let lastError;
  while (Date.now() < deadline) {
    try { const result = await check(); if (result) return result; }
    catch (error) { lastError = error; }
    await new Promise(resolveWait => setTimeout(resolveWait, 100));
  }
  throw new Error(`Timed out waiting for ${label}`, {cause: lastError});
}


async function main() {
  const { chromium } = await import('playwright');
  let fixture, browser, page, artifacts;
  let success = false;
  const consoleLog = [], externalRequests = [];
  const scratch = await mkdtemp(join(tmpdir(), 'opensandbox-native-browser-'));
  artifacts = process.env.NATIVE_BROWSER_ARTIFACTS ? resolve(process.env.NATIVE_BROWSER_ARTIFACTS) : join(scratch, 'artifacts');
  try {
    fixture = await startBrowserFixture({scratch});
    browser = await chromium.launch({headless: true});
    const context = await browser.newContext({viewport: {width: 1440, height: 1000}, locale: 'en-US'});
    const origin = new URL(fixture.url).origin;
    await context.route('**/*', route => {
      const url = route.request().url();
      if (new URL(url).origin === origin) return route.continue();
      externalRequests.push(redact(url));
      return route.abort('blockedbyclient');
    });
    page = await context.newPage();
    page.setDefaultTimeout(60_000);
    page.on('console', message => consoleLog.push(`${message.type()}: ${redact(message.text())}`));
    page.on('pageerror', error => consoleLog.push(`pageerror: ${redact(error.stack ?? error)}`));
    await page.goto(fixture.url, {waitUntil: 'domcontentloaded'});
    await runBrowserUi(page);
    const completed = await until('two browser turns and native evidence', async () => {
      const evidence = await fixture.evidence(); assertBrowserEvidence(evidence); return evidence;
    });

    // The authenticated raw-byte endpoint must also deny uploads independently of
    // the browser's deny-only face. BrowserContext.request shares its login cookie.
    assert((await context.cookies(origin)).length > 0, 'Browser launch-token exchange must establish authentication');
    const upload = new URL('/api/session/uploadFileBinary', origin);
    upload.searchParams.set('sessionId', completed.modelRequests[0].sessionId);
    upload.searchParams.set('name', 'browser-raw-upload-denied.txt');
    const response = await context.request.post(upload.href, {
      headers: {'content-type': 'application/octet-stream'}, data: Buffer.from('must not be stored'), maxRedirects: 0,
    });
    assert.equal(response.status(), 404, 'The disabled stock upload route must not be registered');

    await assert.rejects(stat(join(fixture.scratch, 'home', 'attachments')), error => error.code === 'ENOENT');
    assert.doesNotMatch(await page.locator('body').innerText(), /Failed to load plugins/i);
    assert.deepEqual(externalRequests, []);
    assertBrowserEvidence(await fixture.evidence());
    success = true;
    console.log('NATIVE_BROWSER_PASS actual-installed-cli visible-two-turns history-reload file-and-image-denied authenticated-upload-404 no-attachment-storage zero-external-requests');
  } catch (error) {
    if (artifacts) {
      await mkdir(artifacts, {recursive: true});
      await Promise.allSettled([
        page?.screenshot({path: join(artifacts, 'failure.png'), fullPage: true}),
        page?.content().then(html => writeFile(join(artifacts, 'failure.html'), redact(html))),
        writeFile(join(artifacts, 'console.log'), consoleLog.join('\n')),
        writeFile(join(artifacts, 'failure.txt'), redact(error.stack ?? error)),
        ...['web.log', 'init.log', 'plugin-add.log', 'evidence.json'].map(name =>
          readFile(join(scratch, name), 'utf8').then(text => writeFile(join(artifacts, name), redact(text)))),
      ]);
      console.error(`Native browser diagnostics retained at ${artifacts}`);
    }
    throw new Error(redact(error.stack ?? error));
  } finally {
    try { await browser?.close(); } finally {
      await fixture?.stop();
      if (success) await rm(fixture.scratch, {recursive: true, force: true});
    }
  }
}

if (process.argv[1] === fileURLToPath(import.meta.url)) await main();
