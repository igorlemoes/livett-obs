// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

// Relay de sinalização da câmera do celular (LiveTT).
//
// Leva só as mensagens pequenas que o celular e o receptor do OBS trocam para abrir a conexão WebRTC (oferta, resposta, candidatos ICE).
// O vídeo e o som NÃO passam por aqui: vão direto do celular para o OBS. Serve também a página do celular, num endereço HTTPS com
// certificado válido (é isso que faz o iPhone liberar a câmera).
//
// Autenticação, sem estado no Worker:
//   - O receptor (a página dentro do OBS) tem uma chave secreta `rxKey` que só existe no computador do usuário. A sala é
//     `room = sha256(rxKey)[0:32]`: quem não tem a chave não consegue entrar como receptor, nem calculando a partir do QR.
//   - O receptor registra na sala o hash `sha256(phoneKey)` da chave do celular (mensagem `phone-key`); o QR leva `phoneKey` no
//     fragmento (#k=...). O celular entra com `phoneKey`; a sala confere o hash. Registrar outro hash (ou vazio) desconecta o
//     celular e invalida o QR antigo.
//
// Licença: GPL-2.0-or-later.

const ROOM_RE = /^[0-9a-f]{32}$/;
const HASH_RE = /^[0-9a-f]{64}$/;
const MAX_MESSAGE = 64 * 1024;
const JOIN_WINDOW_MS = 15_000; // quem não entra (join) nesse prazo é desconectado
const RATE_WINDOW_MS = 10_000;
const RATE_MAX = 300; // mensagens por janela, por conexão

async function sha256Hex(text) {
  const digest = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(text));
  return [...new Uint8Array(digest)].map((b) => b.toString(16).padStart(2, '0')).join('');
}

// Comparação em tempo constante de duas strings hexadecimais do mesmo tamanho.
function sameHex(a, b) {
  if (typeof a !== 'string' || typeof b !== 'string' || a.length !== b.length) return false;
  let diff = 0;
  for (let i = 0; i < a.length; i++) diff |= a.charCodeAt(i) ^ b.charCodeAt(i);
  return diff === 0;
}

function pageHeaders(host) {
  return {
    'Content-Type': 'text/html; charset=utf-8',
    'Cache-Control': 'no-store',
    'X-Content-Type-Options': 'nosniff',
    'Referrer-Policy': 'no-referrer',
    'Content-Security-Policy':
      `default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self' wss://${host}; ` +
      "media-src 'self' blob:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'",
  };
}

export default {
  async fetch(request, env) {
    const url = new URL(request.url);

    if (url.pathname === '/ws') {
      if (request.headers.get('Upgrade') !== 'websocket') return new Response('WebSocket esperado', { status: 426 });
      const room = url.searchParams.get('room') || '';
      if (!ROOM_RE.test(room)) return new Response('sala inválida', { status: 400 });
      const forwarded = new Request(request);
      forwarded.headers.set('X-Room', room); // definido aqui, nunca pelo cliente
      return env.ROOMS.get(env.ROOMS.idFromName(room)).fetch(forwarded);
    }

    if (/^\/c\/[0-9a-f]{32}\/?$/.test(url.pathname)) {
      const page = await env.ASSETS.fetch(new Request(new URL('/phone.html', url), { method: 'GET' }));
      return new Response(page.body, { status: page.status, headers: pageHeaders(url.host) });
    }

    return env.ASSETS.fetch(request); // /, /s/phone.js, /s/camera.css
  },
};

export class Room {
  constructor(state) {
    this.state = state;
    // "ping" do celular/receptor é respondido sem acordar a sala (a hibernação não é cobrada).
    state.setWebSocketAutoResponse(new WebSocketRequestResponsePair('ping', 'pong'));
  }

  async fetch(request) {
    const room = request.headers.get('X-Room');
    if (!room || request.headers.get('Upgrade') !== 'websocket') return new Response('', { status: 400 });
    const { 0: client, 1: server } = new WebSocketPair();
    this.state.acceptWebSocket(server);
    server.serializeAttachment({ role: null, room, since: Date.now(), windowStart: Date.now(), count: 0 });
    await this.state.storage.setAlarm(Date.now() + JOIN_WINDOW_MS);
    return new Response(null, { status: 101, webSocket: client });
  }

  sockets(role) {
    return this.state.getWebSockets().filter((ws) => (ws.deserializeAttachment() || {}).role === role);
  }

  async webSocketMessage(ws, raw) {
    if (typeof raw !== 'string' || raw.length > MAX_MESSAGE) {
      ws.close(1009, 'mensagem grande demais');
      return;
    }
    const att = ws.deserializeAttachment() || {};
    const now = Date.now();
    if (now - (att.windowStart || 0) > RATE_WINDOW_MS) {
      att.windowStart = now;
      att.count = 0;
    }
    att.count = (att.count || 0) + 1;
    if (att.count > RATE_MAX) {
      ws.close(4008, 'mensagens demais');
      return;
    }
    ws.serializeAttachment(att);

    let msg;
    try {
      msg = JSON.parse(raw);
    } catch {
      return;
    }
    if (!msg || typeof msg.type !== 'string' || msg.type.length > 16) return;

    if (!att.role) {
      if (msg.type === 'join') await this.join(ws, att, msg);
      return;
    }
    if (att.role === 'rx' && msg.type === 'phone-key') {
      await this.setPhoneKey(msg.hash);
      return;
    }
    if (msg.type === 'join' || msg.type === 'phone-key') return;

    const other = this.sockets(att.role === 'rx' ? 'phone' : 'rx').pop();
    if (other) other.send(raw);
    else if (att.role === 'phone') ws.send(JSON.stringify({ type: 'rx-offline' }));
  }

  async join(ws, att, msg) {
    const key = typeof msg.key === 'string' ? msg.key : '';
    if (key.length < 16 || key.length > 128) {
      ws.close(4001, 'chave inválida');
      return;
    }
    if (msg.role === 'rx') {
      const room = (await sha256Hex(key)).slice(0, 32);
      if (!sameHex(room, att.room)) {
        ws.close(4003, 'não autorizado');
        return;
      }
    } else if (msg.role === 'phone') {
      const stored = await this.state.storage.get('phoneHash');
      if (!stored || !sameHex(await sha256Hex(key), stored)) {
        ws.close(4003, 'não autorizado');
        return;
      }
    } else {
      ws.close(4001, 'papel inválido');
      return;
    }
    for (const old of this.sockets(msg.role)) {
      old.serializeAttachment({ ...old.deserializeAttachment(), role: null });
      old.close(4000, 'substituído por outra conexão');
    }
    att.role = msg.role;
    ws.serializeAttachment(att);
    ws.send(JSON.stringify({ type: 'joined', role: msg.role, peer: this.sockets(msg.role === 'rx' ? 'phone' : 'rx').length > 0 }));
  }

  async setPhoneKey(hash) {
    if (hash !== '' && !(typeof hash === 'string' && HASH_RE.test(hash))) return;
    const before = await this.state.storage.get('phoneHash');
    if ((before || '') === hash) return;
    if (hash === '') await this.state.storage.delete('phoneHash');
    else await this.state.storage.put('phoneHash', hash);
    for (const phone of this.sockets('phone')) {
      phone.serializeAttachment({ ...phone.deserializeAttachment(), role: null });
      phone.close(4003, 'o código mudou');
    }
  }

  // Quem não entrou a tempo é desconectado (não fica ocupando a sala).
  async alarm() {
    const limit = Date.now() - JOIN_WINDOW_MS;
    for (const ws of this.state.getWebSockets()) {
      const att = ws.deserializeAttachment() || {};
      if (!att.role && (att.since || 0) <= limit) ws.close(4002, 'não entrou a tempo');
    }
  }

  tellOther(ws, message) {
    const att = ws.deserializeAttachment() || {};
    if (att.role !== 'rx' && att.role !== 'phone') return;
    const other = this.sockets(att.role === 'rx' ? 'phone' : 'rx').filter((s) => s !== ws).pop();
    if (other) other.send(JSON.stringify(message));
  }

  async webSocketClose(ws, code, reason) {
    const att = ws.deserializeAttachment() || {};
    this.tellOther(ws, att.role === 'rx' ? { type: 'rx-offline' } : { type: 'bye' });
    try {
      ws.close(code === 1005 ? 1000 : code, reason);
    } catch {
      /* já fechada */
    }
  }

  async webSocketError(ws) {
    await this.webSocketClose(ws, 1011, 'erro');
  }
}
