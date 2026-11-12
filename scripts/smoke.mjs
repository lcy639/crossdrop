// Uses only Node's standard library. Run with Node 22 or newer.
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { spawnSync } from 'node:child_process';

const origin = process.env.BASE_URL || 'http://127.0.0.1:8787';
const total = Number(process.env.FILE_MIB || 17) * 1024 * 1024 + 37;
const blockSize = 8 * 1024 * 1024;
const digest = bytes => createHash('sha256').update(bytes).digest('hex');
const block = index => Buffer.alloc(Math.min(blockSize, total - index * blockSize), index % 251);
async function call(path, { method = 'GET', token, json, bytes, headers = {}, status = 200 } = {}) {
  const response = await fetch(origin + path, {
    method,
    headers: { ...headers, ...(token ? { Authorization: `Bearer ${token}` } : {}), ...(json ? { 'Content-Type': 'application/json' } : {}) },
    body: json ? JSON.stringify(json) : bytes,
    signal: AbortSignal.timeout(180000),
  });
  assert.equal(response.status, status, `${method} ${path.split('?')[0]} expected ${status}, got ${response.status}`);
  return response;
}
async function waitReady() {
  for (let attempt = 0; attempt < 60; attempt++) {
    try { if ((await fetch(origin + '/healthz')).ok) return; } catch { /* The server is restarting. */ }
    await new Promise(resolve => setTimeout(resolve, 1000));
  }
  throw new Error('Server did not become healthy');
}
let room;
try {
  await waitReady();
  const config = await (await call('/api/config')).json();
  assert.equal(config.chunkSize, blockSize);
  room = await (await call('/api/rooms', { method: 'POST', status: 201 })).json();
  assert.match(room.code, /^\d{6}$/);
  assert.notEqual(room.ownerToken, room.receiveToken);
  const base = `/api/rooms/${room.id}`;
  const receiver = await (await call('/api/pair', { method: 'POST', json: { code: room.code } })).json();
  assert.equal(receiver.receiveToken, room.receiveToken);
  await call(base, { status: 401 });
  await call(base + '/files', { method: 'POST', token: room.receiveToken, json: {}, status: 401 });
  console.log('PASS room, pairing and separate permissions');

  const hash = createHash('sha256');
  for (let index = 0; index * blockSize < total; index++) hash.update(block(index));
  const sha256 = hash.digest('hex');
  const manifest = { name: '跨设备验证.bin', size: total, sha256 };
  const file = await (await call(base + '/files', { method: 'POST', token: room.ownerToken, json: manifest, status: 201 })).json();
  const fileBase = `${base}/files/${file.id}`;
  const put = async index => {
    const bytes = block(index);
    await call(`${fileBase}/chunks/${index}`, { method: 'PUT', token: room.ownerToken, bytes, headers: { 'X-Chunk-SHA256': digest(bytes) }, status: 204 });
  };
  await put(0);
  await put(0);
  if (process.env.RESTART_SERVER === '1') {
    assert.equal(new URL(origin).hostname, '127.0.0.1', 'Restart test must use localhost');
    const result = spawnSync('docker', ['compose', 'restart', 'crossdrop'], { cwd: new URL('..', import.meta.url), encoding: 'utf8' });
    assert.equal(result.status, 0, result.stderr);
    await waitReady();
    console.log('PASS real container restart');
  }
  const resumed = await (await call(base + '/files', { method: 'POST', token: room.ownerToken, json: manifest })).json();
  assert.equal(resumed.id, file.id);
  assert.deepEqual(resumed.chunks, [0]);
  console.log('PASS duplicate requests and persisted resume state');
  for (let index = 1; index * blockSize < total; index++) await put(index);
  await call(fileBase + '/complete', { method: 'POST', token: room.ownerToken, status: 202 });
  for (let attempt = 0; ; attempt++) {
    const state = await (await call(base, { token: room.receiveToken })).json();
    const item = state.files.find(item => item.id === file.id);
    if (item.ready) break;
    assert.equal(item.error || '', '');
    assert.ok(attempt < 180, 'Verification timed out');
    await new Promise(resolve => setTimeout(resolve, 1000));
  }
  const download = await call(fileBase + '/download', { token: room.receiveToken });
  const received = createHash('sha256');
  let receivedBytes = 0;
  for await (const chunk of download.body) { received.update(chunk); receivedBytes += chunk.length; }
  assert.equal(receivedBytes, total);
  assert.equal(received.digest('hex'), sha256);
  const partial = await call(fileBase + '/download', { token: room.receiveToken, headers: { Range: 'bytes=8388608-8388623' }, status: 206 });
  assert.deepEqual(Buffer.from(await partial.arrayBuffer()), block(1).subarray(0, 16));
  console.log(`PASS ${receivedBytes} bytes streamed with SHA-256 and HTTP Range`);
  const empty = await (await call(base + '/files', { method: 'POST', token: room.ownerToken, json: { name: 'empty.txt', size: 0, sha256: digest(Buffer.alloc(0)) }, status: 201 })).json();
  await call(`${base}/files/${empty.id}/complete`, { method: 'POST', token: room.ownerToken, status: 202 });
  console.log('PASS multiple files and empty-file registration');
} finally {
  if (room) {
    await call(`/api/rooms/${room.id}`, { method: 'DELETE', token: room.ownerToken, status: 204 });
    await call(`/api/rooms/${room.id}`, { token: room.receiveToken, status: 410 });
    console.log('PASS closed room revokes access; test files await automatic cleanup');
  }
}
