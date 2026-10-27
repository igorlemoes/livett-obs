// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

'use strict';
// The phone's side of the wireless camera: captures the camera and microphone, sends them to the OBS receiver over WebRTC
// and uses the controller only to exchange the small signalling messages (offer, answer, ICE) by long polling.
(() => {
  const $ = (id) => document.getElementById(id);
  const base = location.pathname.replace(/\/+$/, '');
  const params = new URLSearchParams(location.search);
  const key = params.get('k') || '';
  // Served by the relay (a Cloudflare Worker) the page lives at /c/<room> and the secret is in the fragment (#k=...), which the
  // browser never sends to a server; signalling then goes over a WebSocket. Served by the controller it uses long polling.
  const hashKey = new URLSearchParams(location.hash.replace(/^#/, '')).get('k') || '';
  const viaRelay = hashKey !== '' && key === '';
  const room = (new RegExp('^/c/([0-9a-f]{32})').exec(location.pathname) || [])[1] || '';
  const url = (path, extra = '') => `${base}/${path}?k=${encodeURIComponent(key)}${extra}`;
  const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
  const fake = params.has('fake'); // test pattern and a quiet tone instead of the real camera and microphone
  const BITRATE = { 480: 2e6, 720: 4e6, 1080: 8e6 };
  const SIZE = { 480: [854, 480], 720: [1280, 720], 1080: [1920, 1080] };

  let stream = null, pc = null, running = false, after = 0, wakeLock = null, generation = 0, stopFake = null;
  let sendChain = Promise.resolve();
  let socket = null, joined = Promise.resolve(false), joinResolve = () => {}, heartbeat = 0;

  function say(text, kind = '') {
    const el = $('status');
    el.textContent = text;
    el.dataset.kind = kind;
  }
  function badge(on, text) {
    const el = $('badge');
    el.dataset.on = on ? '1' : '0';
    el.textContent = text;
  }

  // Messages leave in order: an ICE candidate must never overtake the offer that introduces it.
  function post(type, data) {
    if (viaRelay) {
      sendChain = sendChain
        .then(async () => {
          if (await joined && socket && socket.readyState === 1) socket.send(JSON.stringify({ type, data }));
        })
        .catch(() => {});
      return sendChain;
    }
    sendChain = sendChain
      .then(() => fetch(url('send'), { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ type, data }) }))
      .catch(() => {});
    return sendChain;
  }

  function deviceName() {
    const ua = navigator.userAgent;
    if (/iPhone/.test(ua)) return 'iPhone';
    if (/iPad/.test(ua)) return 'iPad';
    const android = /Android [\d.]+; ([^;)]+)/.exec(ua);
    if (android) return android[1].trim();
    return 'Celular';
  }

  function fakeStream() {
    const canvas = document.createElement('canvas');
    canvas.width = 1280;
    canvas.height = 720;
    const g = canvas.getContext('2d');
    let n = 0;
    const timer = setInterval(() => {
      n++;
      g.fillStyle = '#0c0f16';
      g.fillRect(0, 0, 1280, 720);
      const colors = ['#fe2c55', '#25f4ee', '#f3c969', '#43dccf', '#ffffff'];
      colors.forEach((c, i) => { g.fillStyle = c; g.fillRect(i * 256, 520, 256, 200); });
      g.fillStyle = '#25f4ee';
      g.fillRect((n * 8) % 1160, 260, 120, 120);
      g.fillStyle = '#ffffff';
      g.font = 'bold 56px sans-serif';
      g.fillText('TESTE DA CÂMERA ' + new Date().toLocaleTimeString(), 40, 120);
    }, 33);
    const out = canvas.captureStream(30);
    const audio = new AudioContext();
    const osc = audio.createOscillator();
    const gain = audio.createGain();
    gain.gain.value = 0.03;
    const dest = audio.createMediaStreamDestination();
    osc.connect(gain).connect(dest);
    osc.start();
    dest.stream.getAudioTracks().forEach((t) => out.addTrack(t));
    stopFake = () => { clearInterval(timer); osc.stop(); audio.close(); stopFake = null; };
    return out;
  }

  function constraints() {
    const [w, h] = SIZE[$('quality').value] || SIZE[1080];
    // Always the same (landscape) request: the browser turns the picture by itself as the phone turns, and OBS follows the size
    // that arrives. Asking for a portrait size makes iPhones answer with the sensor's own landscape picture instead.
    const video = { width: { ideal: w }, height: { ideal: h }, frameRate: { ideal: +$('fps').value } };
    const cam = $('camera').value;
    if (cam) video.deviceId = { exact: cam };
    else video.facingMode = { ideal: 'environment' }; // the back camera until another is chosen in the list
    const mic = $('mic').value;
    const audio = mic === 'off' ? false : Object.assign(mic ? { deviceId: { exact: mic } } : {}, { echoCancellation: false, noiseSuppression: true, autoGainControl: true });
    return { video, audio };
  }

  async function capture() {
    if (fake) {
      if (stopFake) stopFake();
      return fakeStream();
    }
    if (!navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) {
      throw new Error('Este navegador não liberou a câmera. Abra o endereço https do QR code.');
    }
    return navigator.mediaDevices.getUserMedia(constraints());
  }

  async function fillDevices() {
    if (!navigator.mediaDevices || !navigator.mediaDevices.enumerateDevices) return;
    const devices = await navigator.mediaDevices.enumerateDevices();
    const fill = (select, kind, first) => {
      const current = select.value;
      select.textContent = '';
      first.forEach(([value, text]) => select.add(new Option(text, value)));
      devices.filter((d) => d.kind === kind).forEach((d, i) => select.add(new Option(d.label || `${kind === 'videoinput' ? 'Câmera' : 'Microfone'} ${i + 1}`, d.deviceId)));
      if ([...select.options].some((o) => o.value === current)) select.value = current;
    };
    fill($('camera'), 'videoinput', [['', 'Padrão (traseira)']]);
    fill($('mic'), 'audioinput', [['', 'Padrão'], ['off', 'Sem áudio']]);
  }

  function setupZoom() {
    const track = stream && stream.getVideoTracks()[0];
    const caps = track && track.getCapabilities ? track.getCapabilities() : {};
    const box = $('zoomBox');
    if (!caps.zoom) {
      box.hidden = true;
      return;
    }
    const range = $('zoom');
    range.min = caps.zoom.min;
    range.max = caps.zoom.max;
    range.step = caps.zoom.step || 0.1;
    range.value = (track.getSettings().zoom) || caps.zoom.min;
    $('zoomValue').textContent = `${(+range.value).toFixed(1)}×`;
    box.hidden = false;
  }

  function tune() {
    if (!pc) return;
    for (const sender of pc.getSenders()) {
      if (!sender.track || sender.track.kind !== 'video') continue;
      const p = sender.getParameters();
      if (!p.encodings || !p.encodings.length) p.encodings = [{}];
      p.encodings[0].maxBitrate = BITRATE[$('quality').value] || 4e6;
      p.degradationPreference = 'maintain-resolution';
      sender.setParameters(p).catch(() => {});
    }
  }

  function closePc() {
    if (!pc) return;
    const old = pc;
    pc = null;
    old.onicecandidate = old.onconnectionstatechange = null;
    old.close();
  }

  async function negotiate() {
    closePc();
    const mine = (pc = new RTCPeerConnection({ iceServers: [] })); // same network: no STUN or TURN needed
    stream.getTracks().forEach((t) => mine.addTrack(t, stream));
    mine.onicecandidate = (e) => { if (e.candidate && pc === mine) post('ice', e.candidate.toJSON()); };
    mine.onconnectionstatechange = () => {
      if (pc !== mine) return;
      const s = mine.connectionState;
      if (s === 'connected') {
        badge(true, 'Ao vivo no OBS');
        say('Conectado ao OBS. Pode usar o celular como câmera.', 'ok');
      } else if (s === 'failed' || s === 'disconnected') {
        badge(false, 'Reconectando…');
        say('A conexão caiu. Tentando de novo…', 'error');
        setTimeout(() => { if (running && pc === mine && mine.connectionState !== 'connected') negotiate(); }, 3000);
      }
    };
    badge(false, 'Conectando…');
    say('Procurando o OBS na rede…');
    await post('hello', deviceName());
    const offer = await mine.createOffer();
    if (pc !== mine) return;
    await mine.setLocalDescription(offer);
    await post('offer', { sdp: offer.sdp });
  }

  async function onMessage(m) {
    if (m.type === 'rxhello') {
      if (stream) negotiate(); // OBS reloaded the receiver: start over
      return;
    }
    if (!pc) return;
    if (m.type === 'answer' && m.data && m.data.sdp) {
      await pc.setRemoteDescription({ type: 'answer', sdp: m.data.sdp });
      tune();
    } else if (m.type === 'ice' && m.data) {
      try { await pc.addIceCandidate(m.data); } catch (e) { /* a stale candidate */ }
    }
  }

  async function pollLoop(mine) {
    try {
      const r = await (await fetch(url('recv', '&after=-1'))).json();
      after = r.last;
    } catch (e) { /* the loop below retries */ }
    while (running && mine === generation) {
      try {
        const r = await fetch(url('recv', `&after=${after}&wait=20`));
        if (!r.ok) throw new Error(String(r.status));
        const body = await r.json();
        for (const m of body.messages) {
          after = Math.max(after, m.seq);
          await onMessage(m);
        }
      } catch (e) {
        await sleep(1500);
      }
    }
  }

  // The relay connection: joins as the phone with the key from the QR code. If it drops, it comes back and negotiates again.
  function connectRelay(mine, again) {
    joined = new Promise((resolve) => { joinResolve = resolve; });
    const s = (socket = new WebSocket(`${location.protocol === 'https:' ? 'wss:' : 'ws:'}//${location.host}/ws?room=${room}`));
    s.onopen = () => s.send(JSON.stringify({ type: 'join', role: 'phone', key: hashKey }));
    s.onmessage = async (e) => {
      let m;
      try { m = JSON.parse(e.data); } catch (err) { return; }
      if (m.type === 'joined') {
        joinResolve(true);
        if (again && running && mine === generation) negotiate();
      } else if (m.type === 'rx-offline') {
        badge(false, 'Aguardando o OBS');
        say('O OBS não está recebendo agora. Abra o OBS com a fonte "Câmera do celular".', 'error');
      } else {
        await onMessage(m);
      }
    };
    s.onclose = (e) => {
      clearInterval(heartbeat);
      if (socket !== s) return;
      socket = null;
      joinResolve(false);
      if (!running || mine !== generation) return;
      if (e.code === 4003) {
        stop().then(() => say('Este código não vale mais. Leia o QR code novo no OBS.', 'error'));
        return;
      }
      badge(false, 'Reconectando…');
      setTimeout(() => { if (running && mine === generation) connectRelay(mine, true); }, 3000);
    };
    heartbeat = setInterval(() => { if (s.readyState === 1) s.send('ping'); }, 25000); // keeps the connection from going idle
  }

  async function keepAwake() {
    try {
      if (navigator.wakeLock && !wakeLock) {
        wakeLock = await navigator.wakeLock.request('screen');
        wakeLock.addEventListener('release', () => { wakeLock = null; });
      }
    } catch (e) { /* not available: the screen may turn off */ }
  }

  function explain(error) {
    const name = error && error.name;
    if (name === 'NotAllowedError' || name === 'SecurityError') return 'A câmera foi negada. Libere a câmera e o microfone nas permissões do navegador e tente de novo.';
    if (name === 'NotFoundError') return 'Não encontrei câmera ou microfone neste celular.';
    if (name === 'NotReadableError') return 'A câmera está em uso por outro aplicativo.';
    return (error && error.message) || 'Não consegui abrir a câmera.';
  }

  async function start() {
    $('start').disabled = true;
    try {
      stream = await capture();
    } catch (e) {
      say(explain(e), 'error');
      $('start').disabled = false;
      return;
    }
    $('preview').srcObject = stream;
    await fillDevices().catch(() => {});
    setupZoom();
    running = true;
    const mine = ++generation;
    $('start').hidden = true;
    $('stop').hidden = false;
    keepAwake();
    if (viaRelay) {
      connectRelay(mine, false);
      if (!(await joined)) {
        say('Não consegui falar com o servidor da câmera. Confira a internet do celular.', 'error');
        await stop();
        return;
      }
    } else {
      pollLoop(mine);
    }
    await negotiate();
  }

  async function stop() {
    running = false;
    generation++;
    await post('bye', null);
    if (socket) {
      const s = socket;
      socket = null;
      clearInterval(heartbeat);
      s.close(1000);
    }
    closePc();
    if (stream) stream.getTracks().forEach((t) => t.stop());
    if (stopFake) stopFake();
    stream = null;
    $('preview').srcObject = null;
    $('zoomBox').hidden = true;
    $('start').hidden = false;
    $('start').disabled = false;
    $('stop').hidden = true;
    badge(false, 'Parado');
    say('Câmera parada.');
    if (wakeLock) wakeLock.release().catch(() => {});
  }

  // Changing camera, quality... swaps the tracks in the running connection; adding or dropping the microphone renegotiates.
  async function reacquire() {
    if (!running) return;
    let next;
    try {
      next = await capture();
    } catch (e) {
      say(explain(e), 'error');
      return;
    }
    const old = stream;
    stream = next;
    $('preview').srcObject = next;
    setupZoom();
    let renegotiate = false;
    for (const kind of ['video', 'audio']) {
      const track = next.getTracks().find((t) => t.kind === kind);
      const sender = pc && pc.getSenders().find((s) => s.track && s.track.kind === kind);
      if (track && sender) await sender.replaceTrack(track);
      else if (!!track !== !!sender) renegotiate = true;
    }
    if (old && old !== next) old.getTracks().forEach((t) => t.stop());
    if (renegotiate) await negotiate();
    else tune();
  }

  for (const id of ['camera', 'mic', 'quality', 'fps']) $(id).addEventListener('change', reacquire);
  // What is really being sent, so it can be seen at a glance (the picture turns by itself as the phone turns).
  let shown = '';
  const showSent = () => {
    const track = stream && stream.getVideoTracks()[0];
    const size = track ? track.getSettings() : {};
    shown = size.width && size.height ? `${size.width}×${size.height}${size.frameRate ? ' a ' + Math.round(size.frameRate) + ' quadros' : ''}` : '';
    $('sent').textContent = shown ? `Enviando ${shown}.` : '';
  };
  $('preview').addEventListener('resize', showSent);
  $('preview').addEventListener('loadedmetadata', showSent);
  $('zoom').addEventListener('input', () => {
    const track = stream && stream.getVideoTracks()[0];
    const value = +$('zoom').value;
    $('zoomValue').textContent = `${value.toFixed(1)}×`;
    if (track && track.applyConstraints) track.applyConstraints({ advanced: [{ zoom: value }] }).catch(() => {});
  });
  $('start').addEventListener('click', start);
  $('stop').addEventListener('click', stop);
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState !== 'visible' || !running) return;
    keepAwake();
    const track = stream && stream.getVideoTracks()[0];
    if (track && track.readyState === 'ended') reacquire(); // the system took the camera while the page was hidden
  });
  if (viaRelay) $('certNote').hidden = true;
  if (fake) say('Modo de teste: imagem e som artificiais no lugar da câmera. Toque em Iniciar câmera.');
})();
