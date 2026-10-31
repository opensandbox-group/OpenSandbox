// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import { Context } from '@deepseek-ai/cordis';
import { AttachmentError, AttachmentStore } from '@deepseek-ai/dsh-attachment';
import { SessionId } from '@deepseek-ai/dsh-session';
import type { SessionRequestId } from '@deepseek-ai/dsh-api-session-controller';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { apply as applyClient } from '../src/client.js';
import { createNativePlugin } from '../src/index.js';
import { DeniedAttachments } from '../src/services.js';
import { createControllerFixture } from './fixtures/controller.js';

const roots: Context[] = [];
const cleanup: (() => Promise<void>)[] = [];
afterEach(async () => {
  await Promise.all(cleanup.splice(0).map(close => close()));
  await Promise.all(roots.splice(0).map(root => root.fiber.dispose()));
  vi.restoreAllMocks();
});

async function fixture() {
  const root = new Context(); roots.push(root);
  await root.plugin(DeniedAttachments);
  return root.attachments;
}

describe('native text-only attachment boundary', () => {
  it('retains the host AttachmentStore contract without a local attachment backend', async () => {
    const attachments = await fixture();
    expect(attachments).toBeInstanceOf(AttachmentStore);
    expect(attachments.imageLimits).toEqual({
      mediaTypes: [], maxImageBytes: 1, maxImagesPerMessage: 1,
      maxMessageImageBytes: 1, maxImagePixels: 1, maxImageDimension: 1,
    });
    const input = [{ type: 'text' as const, text: '  native text\n', get unused() { throw new Error('Do not clone arbitrary input'); } }];
    expect(await attachments.admitPromptContent(input)).toEqual([{ type: 'text', text: '  native text\n' }]);
    expect(await attachments.admitPromptContent([])).toEqual([]);
  });

  it('rejects image and file prompt parts before reading payloads or text', async () => {
    const attachments = await fixture();
    for (const type of ['image', 'file'] as const) {
      const input = [
        { type: 'text', get text(): never { throw new Error('No partial text admission'); } },
        { type, mediaType: 'image/png', get data(): never { throw new Error('Do not read image bytes'); }, get attachment(): never { throw new Error('Do not read file references'); } },
      ] as Parameters<AttachmentStore['admitPromptContent']>[0];
      await expect(attachments.admitPromptContent(input)).rejects.toMatchObject({
        name: 'AttachmentError', code: 'ATTACHMENT_FILES_UNSUPPORTED', message: expect.stringContaining('disabled'),
      });
    }
  });

  it('never stores or reads binary attachments through alternate service methods', async () => {
    const attachments = await fixture();
    const untouched = new Proxy({}, { get() { throw new Error('Attachment input must not be read'); } });
    const operations = [
      () => attachments.validateImage(untouched as Parameters<AttachmentStore['validateImage']>[0]),
      () => attachments.saveImage(untouched as Parameters<AttachmentStore['saveImage']>[0]),
      () => attachments.saveImages(untouched as Parameters<AttachmentStore['saveImages']>[0]),
      () => attachments.admitEncodedFile(untouched as Parameters<AttachmentStore['admitEncodedFile']>[0]),
      () => attachments.saveFile(untouched as Parameters<AttachmentStore['saveFile']>[0]),
      () => attachments.saveFileStream(untouched as Parameters<AttachmentStore['saveFileStream']>[0]),
      () => attachments.readImage(untouched as Parameters<AttachmentStore['readImage']>[0]),
      () => attachments.readImageRequest(untouched as Parameters<AttachmentStore['readImageRequest']>[0], untouched as Parameters<AttachmentStore['readImageRequest']>[1]),
      async () => { for await (const _ of attachments.readFileStream(untouched as Parameters<AttachmentStore['readFileStream']>[0])) throw new Error('No attachment bytes may be emitted'); },
    ];
    for (const operation of operations) await expect(operation()).rejects.toBeInstanceOf(AttachmentError);
    expect(attachments.imageHostPath(untouched as Parameters<AttachmentStore['imageHostPath']>[0])).toBeUndefined();
    expect(attachments.fileHostPath(untouched as Parameters<AttachmentStore['fileHostPath']>[0])).toBeUndefined();
  });

  it('rejects native image prompts but preserves subsequent text prompts and native history', async () => {
    const openBinding = vi.fn(async (): Promise<never> => { throw new Error('Attachments must not acquire a sandbox'); });
    const app = await createControllerFixture(createNativePlugin({ openBinding }), {
      domain: 'offline.invalid', protocol: 'https', kind: 'create-template', templateId: 'offline-template', remoteCwd: '/workspace',
    });
    cleanup.push(app.dispose);
    const sessionId = SessionId('attachment-policy');
    await app.root.sessionController.create({ sessionId, cwd: app.cwd, agentPreset: 'opensandbox' });
    const resolved = await app.root.sessionController.resolveAgent(sessionId);
    if ('error' in resolved) throw resolved.error;
    const { agent } = resolved;
    const signal = new AbortController().signal;
    await expect(app.root.sessionController.prompt({
      sessionId, requestId: 'refuse-image' as SessionRequestId, mode: 'queue',
      content: [{ type: 'image', mediaType: 'image/png', data: 'not-decoded' }],
    }, signal)).rejects.toMatchObject({ code: 'session/attachment-invalid', details: { reason: 'ATTACHMENT_FILES_UNSUPPORTED' } });
    expect(app.modelRequests).toEqual([]);
    expect(agent.session.snapshotEvents().filter(event => event.type === 'user/message')).toEqual([]);
    await app.root.sessionController.prompt({
      sessionId, requestId: 'accept-text' as SessionRequestId, mode: 'queue', content: [{ type: 'text', text: 'native text after denied image' }],
    }, signal);
    await agent.whenIdle();
    expect(app.modelRequests).toHaveLength(1);
    const messages = agent.session.snapshotEvents().filter(event => event.type === 'user/message');
    expect(messages).toHaveLength(1);
    expect(messages[0]?.data.content).toEqual([{ type: 'text', text: 'native text after denied image' }]);
    expect(openBinding).not.toHaveBeenCalled();
    expect(app.hostCalls).toEqual([]);
  });

  it('cold resumes persisted text history with valid projection limits and attachments still denied', async () => {
    const openBinding = vi.fn(async (): Promise<never> => { throw new Error('Text-only resume must not acquire a sandbox'); });
    const config = {
      domain: 'offline.invalid', protocol: 'https' as const, kind: 'create-template' as const,
      templateId: 'offline-template', remoteCwd: '/workspace',
    };
    const app = await createControllerFixture(createNativePlugin({ openBinding }), config);
    cleanup.push(app.dispose);
    const sessionId = SessionId('attachment-policy-cold');
    const signal = new AbortController().signal;
    await app.root.sessionController.create({ sessionId, cwd: app.cwd, agentPreset: 'opensandbox' });
    const created = await app.root.sessionController.resolveAgent(sessionId);
    if ('error' in created) throw created.error;
    await app.root.sessionController.prompt({
      sessionId, requestId: 'before-cold-resume' as SessionRequestId, mode: 'queue', content: [{ type: 'text', text: 'Persist text-only history.' }],
    }, signal);
    await created.agent.whenIdle();
    const history = created.agent.session.snapshotEvents();
    await app.dispose();

    const resumed = await createControllerFixture(createNativePlugin({ openBinding }), config, { store: app.store });
    cleanup.push(resumed.dispose);
    const resolved = await resumed.root.sessionController.resolveAgent(sessionId);
    if ('error' in resolved) throw resolved.error;
    expect(resolved.agent).not.toBe(created.agent);
    expect(resolved.agent.session.snapshotEvents().slice(0, history.length)).toEqual(history);
    expect(resumed.root.attachments.imageLimits.mediaTypes).toEqual([]);
    await expect(resumed.root.sessionController.prompt({
      sessionId, requestId: 'image-after-cold-resume' as SessionRequestId, mode: 'queue',
      content: [{ type: 'image', mediaType: 'image/png', data: 'not-decoded' }],
    }, signal)).rejects.toMatchObject({ code: 'session/attachment-invalid', details: { reason: 'ATTACHMENT_FILES_UNSUPPORTED' } });
    expect(resumed.modelRequests).toEqual([]);
    await resumed.root.sessionController.prompt({
      sessionId, requestId: 'text-after-cold-resume' as SessionRequestId, mode: 'queue', content: [{ type: 'text', text: 'Continue text-only history.' }],
    }, signal);
    await resolved.agent.whenIdle();
    expect(resumed.modelRequests).toHaveLength(1);
    expect(resolved.agent.session.snapshotEvents().filter(event => event.type === 'user/message').map(event => event.data.content)).toEqual([
      [{ type: 'text', text: 'Persist text-only history.' }], [{ type: 'text', text: 'Continue text-only history.' }],
    ]);
    expect(openBinding).not.toHaveBeenCalled();
    expect(resumed.hostCalls).toEqual([]);
  });
});

describe('native browser upload compatibility service', () => {
  it('supplies fileUpload while refusing upload and post before reading inputs or calling transport', async () => {
    const root = new Context(); roots.push(root);
    const fetch = vi.spyOn(globalThis, 'fetch').mockRejectedValue(new Error('Transport must never be called'));
    await root.plugin({ apply: applyClient });
    const service = root.get('fileUpload') as {
      upload(...args: unknown[]): Promise<never>;
      post(...args: unknown[]): Promise<never>;
    };
    expect(service).toBeDefined();
    const untouched = new Proxy({}, { get() { throw new Error('Upload input must not be read'); } });
    await expect(service.upload(untouched, untouched, untouched, untouched)).rejects.toThrow('File uploads are disabled in the OpenSandbox profile.');
    await expect(service.post(untouched)).rejects.toThrow('File uploads are disabled in the OpenSandbox profile.');
    expect(fetch).not.toHaveBeenCalled();
  });
});
