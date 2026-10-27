// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

// Teste de integração do relay: sobe o Worker de verdade (wrangler dev --local) e fala com ele por WebSocket.
import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import http from 'node:http';
import { createHash, randomBytes } from 'node:crypto';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = join(dirname(fileURLToPath(import.meta.url)), '..');
const PORT = 8799;
const base = `127.0.0.1:${PORT}`;
const sha = (s) => createHash('sha256').update(s).digest('hex');

let dev;

before(async () => {
  await new Promise((resolve, reject) => {
    const sync = spawn(process.execPath, [join(root, 'scripts', 'sync.mjs')], { cwd: root });
    sync.on('exit', (c) => (c === 0 ? resolve() : reject(new Error('sync falhou'))));
  });
  dev = spawn(process.execPath, [join(root, 'node_modules', 'wrangler', 'bin', 'wrangler.js'), 'dev', '--local', '--port', String(PORT), '--ip', '127.0.0.1'], {
    cwd: root,
    env: { ...process.env, WRANGLER_SEND_METRICS: 'false', CI: '1' },
  });
  let log = '';
  await new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error('wrangler não ficou pronto:\n' + log)), 90_000);
    const onData = (d) => {
      log += d;
      if (/Ready on/i.test(log)) {
        clearTimeout(timer);
        resolve();
      }
    };
    dev.stdout.on('data', onData);
    dev.stderr.on('data', onData);
    dev.on('exit', () => reject(new Error('wrangler terminou:\n' + log)));
  });
});

after(() => {
  if (dev) dev.kill();
});

// Cliente WebSocket com fila de mensagens e código de fechamento.
function client(room) {
  const ws = new WebSocket(`ws://${base}/ws?room=${room}`);
  const inbox = [];
  const waiting = [];
  let closed = null;
  const closeWaiters = [];
  ws.onmessage = (e) => {
    const m = JSON.parse(e.data);
    const w = waiting.shift();
    if (w) w(m);
    else inbox.push(m);
  };
  ws.onclose = (e) => {
    closed = e.code;
    closeWaiters.splice(0).forEach((f) => f(e.code));
  };
  const opened = new Promise((r) => (ws.onopen = r));
  return {
    ws,
    opened,
    send: (obj) => ws.send(typeof obj === 'string' ? obj : JSON.stringify(obj)),
    next: (ms = 4000) =>
      new Promise((resolve, reject) => {
        if (inbox.length) return resolve(inbox.shift());
        const t = setTimeout(() => reject(new Error('sem mensagem')), ms);
        waiting.push((m) => {
          clearTimeout(t);
          resolve(m);
        });
      }),
    code: (ms = 4000) =>
      new Promise((resolve, reject) => {
        if (closed !== null) return resolve(closed);
        const t = setTimeout(() => reject(new Error('não fechou')), ms);
        closeWaiters.push((c) => {
          clearTimeout(t);
          resolve(c);
        });
      }),
    silent: async (ms = 400) => {
      await new Promise((r) => setTimeout(r, ms));
      return inbox.length === 0;
    },
  };
}

function newPairing() {
  const rxKey = randomBytes(16).toString('hex');
  const phoneKey = randomBytes(16).toString('hex');
  return { rxKey, phoneKey, room: sha(rxKey).slice(0, 32), phoneHash: sha(phoneKey) };
}

async function joinedRx(p) {
  const rx = client(p.room);
  await rx.opened;
  rx.send({ type: 'join', role: 'rx', key: p.rxKey });
  assert.equal((await rx.next()).type, 'joined');
  rx.send({ type: 'phone-key', hash: p.phoneHash });
  return rx;
}

async function joinedPhone(p) {
  const phone = client(p.room);
  await phone.opened;
  phone.send({ type: 'join', role: 'phone', key: p.phoneKey });
  return phone;
}

test('o receptor só entra com a chave que gera a sala', async () => {
  const p = newPairing();
  const rx = client(p.room);
  await rx.opened;
  rx.send({ type: 'join', role: 'rx', key: randomBytes(16).toString('hex') });
  assert.equal(await rx.code(), 4003);
  const good = client(p.room);
  await good.opened;
  good.send({ type: 'join', role: 'rx', key: p.rxKey });
  const msg = await good.next();
  assert.equal(msg.type, 'joined');
  assert.equal(msg.role, 'rx');
});

test('o celular só entra depois de o receptor registrar o código, e só com a chave certa', async () => {
  const p = newPairing();
  const early = await joinedPhone(p);
  assert.equal(await early.code(), 4003, 'sem código registrado ninguém entra como celular');

  const rx = await joinedRx(p);
  await new Promise((r) => setTimeout(r, 200));
  const wrong = client(p.room);
  await wrong.opened;
  wrong.send({ type: 'join', role: 'phone', key: randomBytes(16).toString('hex') });
  assert.equal(await wrong.code(), 4003);

  const phone = await joinedPhone(p);
  const joined = await phone.next();
  assert.deepEqual([joined.type, joined.role, joined.peer], ['joined', 'phone', true]);
  rx.ws.close();
});

test('as mensagens vão de um lado ao outro sem mudar', async () => {
  const p = newPairing();
  const rx = await joinedRx(p);
  await new Promise((r) => setTimeout(r, 200));
  const phone = await joinedPhone(p);
  await phone.next(); // joined
  const offer = JSON.stringify({ type: 'offer', data: { sdp: 'v=0\r\no=- 1 1 IN IP4 127.0.0.1' } });
  phone.send(offer);
  assert.deepEqual(await rx.next(), JSON.parse(offer));
  rx.send({ type: 'answer', data: { sdp: 'v=0 resposta' } });
  assert.deepEqual(await phone.next(), { type: 'answer', data: { sdp: 'v=0 resposta' } });
  phone.send({ type: 'ice', data: { candidate: 'candidate:1' } });
  assert.equal((await rx.next()).type, 'ice');
  rx.ws.close();
  phone.ws.close();
});

test('trocar o código desconecta o celular e invalida o QR antigo', async () => {
  const p = newPairing();
  const rx = await joinedRx(p);
  await new Promise((r) => setTimeout(r, 200));
  const phone = await joinedPhone(p);
  await phone.next();
  const next = randomBytes(16).toString('hex');
  rx.send({ type: 'phone-key', hash: sha(next) });
  assert.equal(await phone.code(), 4003);
  const old = await joinedPhone(p);
  assert.equal(await old.code(), 4003, 'a chave antiga não entra mais');
  const fresh = client(p.room);
  await fresh.opened;
  fresh.send({ type: 'join', role: 'phone', key: next });
  assert.equal((await fresh.next()).type, 'joined');
  // "parar de aceitar": hash vazio
  rx.send({ type: 'phone-key', hash: '' });
  assert.equal(await fresh.code(), 4003);
  rx.ws.close();
});

test('se o receptor sai o celular é avisado, e se o celular sai o receptor é avisado', async () => {
  const p = newPairing();
  const rx = await joinedRx(p);
  await new Promise((r) => setTimeout(r, 200));
  const phone = await joinedPhone(p);
  await phone.next();
  phone.ws.close();
  assert.equal((await rx.next()).type, 'bye');
  const phone2 = await joinedPhone(p);
  await phone2.next();
  rx.ws.close();
  assert.equal((await phone2.next()).type, 'rx-offline');
  phone2.send({ type: 'offer', data: {} });
  assert.equal((await phone2.next()).type, 'rx-offline');
  phone2.ws.close();
});

test('uma conexão nova do mesmo papel substitui a antiga', async () => {
  const p = newPairing();
  const first = await joinedRx(p);
  const second = client(p.room);
  await second.opened;
  second.send({ type: 'join', role: 'rx', key: p.rxKey });
  assert.equal((await second.next()).type, 'joined');
  assert.equal(await first.code(), 4000);
  second.ws.close();
});

test('mensagem grande demais, sala inválida e quem não entra são recusados', async () => {
  const p = newPairing();
  const rx = await joinedRx(p);
  rx.send('x'.repeat(70 * 1024));
  assert.equal(await rx.code(), 1009);

  const bad = await new Promise((resolve, reject) => {
    const req = http.request({ host: '127.0.0.1', port: PORT, path: '/ws?room=nao-e-uma-sala', headers: { Upgrade: 'websocket', Connection: 'Upgrade' } }, resolve);
    req.on('error', reject);
    req.end();
  });
  assert.equal(bad.statusCode, 400);
  const plain = await fetch(`http://${base}/ws?room=${p.room}`);
  assert.equal(plain.status, 426);

  const lurker = client(p.room);
  await lurker.opened;
  lurker.send({ type: 'offer', data: {} }); // sem join: ignorada
  assert.equal(await lurker.silent(300), true);
  assert.equal(await lurker.code(25_000), 4002, 'quem não entra a tempo é desconectado');
});

test('a página do celular vem com política de conteúdo estrita', async () => {
  const p = newPairing();
  const page = await fetch(`http://${base}/c/${p.room}`);
  assert.equal(page.status, 200);
  const csp = page.headers.get('content-security-policy');
  assert.match(csp, /default-src 'none'/);
  assert.match(csp, new RegExp(`connect-src 'self' wss://${base.replace('.', '\\.')}`));
  assert.equal(page.headers.get('cache-control'), 'no-store');
  assert.match(await page.text(), /Câmera para o OBS/);
  assert.equal((await fetch(`http://${base}/c/curto`)).status, 404);
  assert.equal((await fetch(`http://${base}/s/phone.js`)).status, 200);
});
