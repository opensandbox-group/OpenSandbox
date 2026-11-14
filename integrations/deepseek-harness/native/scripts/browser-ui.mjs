// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import assert from 'node:assert/strict';

export const browserPrompts = Object.freeze([
  'Create browser-note.txt in the remote sandbox with the text first browser turn.',
  'Read browser-note.txt, replace first browser turn with second browser turn, and use bash to print the edited file.',
]);

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


/** Shared with manual CUA acceptance; only visible browser controls are used. */
export async function runBrowserUi(page) {
  await page.getByRole('button', {name: /^(?:继续|Continue)$/}).click();
  const composer = page.locator('[role="textbox"][contenteditable="true"]:visible');
  await composer.waitFor({state: 'visible'});
  const send = page.getByRole('button', {name: /^(?:发送消息|Send message)$/});
  const choose = async file => {
    await page.getByRole('button', {name: /^(?:添加文件或调用指令|Add files or run commands)$/}).click();
    const chosen = page.waitForEvent('filechooser');
    await page.locator('text=/^(文件|File)$/').click();
    await (await chosen).setFiles(file);
  };
  await choose({name: 'browser-upload-denied.txt', mimeType: 'text/plain', buffer: Buffer.from('must not be stored')});
  const retry = page.getByRole('button', {name: /^(?:重试上传|Retry uploading) browser-upload-denied\.txt$/});
  await retry.waitFor({state: 'visible'});
  await composer.fill(browserPrompts[0]);
  assert.equal(await send.isEnabled(), false, 'A failed attachment must block Send');
  await retry.click();
  await retry.waitFor({state: 'visible'});
  assert.equal(await send.isEnabled(), false);
  await page.getByRole('button', {name: /^(?:移除文件|Remove file) browser-upload-denied\.txt$/}).click();
  for (const [index, prompt] of browserPrompts.entries()) {
    await composer.fill(prompt);
    await until('enabled text-only Send', () => send.isEnabled());
    await send.click();
    const marker = index === 0 ? 'BROWSER_FIRST_TURN_COMPLETE' : 'BROWSER_SECOND_TURN_COMPLETE';
    await page.locator(`text=/${marker}/`).waitFor({state: 'visible'});
  }
  // Image-send tests host prompt admission, not clipboard paste. All PNG chunks
  // have valid CRCs; rejection must be policy denial, not corrupt-image decoding.
  await choose({name: 'browser-image-denied.png', mimeType: 'image/png',
    buffer: Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=', 'base64')});
  const removeImage = page.getByRole('button', {name: /^(?:移除图片|Remove image) browser-image-denied\.png$/});
  await removeImage.waitFor({state: 'visible'});
  await until('enabled image draft Send', () => send.isEnabled());
  await send.click();
  await page.locator('text=/ATTACHMENT_FILES_UNSUPPORTED/').waitFor({state: 'visible'});
  await removeImage.click();
  await page.reload();
  for (const marker of ['BROWSER_FIRST_TURN_COMPLETE', 'BROWSER_SECOND_TURN_COMPLETE']) {
    await page.locator(`text=/${marker}/`).waitFor({state: 'visible'});
  }
  await composer.waitFor({state: 'visible'});
}

