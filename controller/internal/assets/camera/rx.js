// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

'use strict';
// The OBS side of the wireless camera: this page is what an OBS Browser Source loads. It answers the phone's WebRTC offer
// and plays the video and audio, with a transparent background while nobody is connected (nothing shows on the stream).
//
// The signalling with the phone goes through the controller (local network or tunnel modes) or through the relay (a Cloudflare
// Worker); the controller says which in /camera/rx/config. What the panel shows (connected, the phone's name) always goes to
// the controller, which is on this computer.
(() => {
  const id = new URLSearchParams(location.search).get('c') || '';
  const video = document.getElementById('v');
  const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
  let pc = null, pending = [], after = 0, localChain = Promise.resolve();
  let config = { mode: 'local' }, socket = null;

  // To the controller on this computer: signalling in local mode, and always the panel's status.
  function local(type, data) {
    localChain = localChain
      .then(() => fetch(`/camera/rx/send?c=${encodeURIComponent(id)}`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ type, data }) }))
      .catch(() => {});
    return localChain;
  }

  // To the phone.
  function signal(type, data) {
    if (config.mode === 'relay') {
      if (socket && socket.readyState === 1) socket.send(JSON.stringify({ type, data }));
      return Promise.resolve();
    }
    return local(type, data);
  }

  function closePc() {
    pending = [];
    video.srcObject = null;
    reportSize();
    if (!pc) return;
    const old = pc;
    pc = null;
    old.ontrack = old.onicecandidate = old.onconnectionstatechange = null;
    old.close();
    local('state', 'idle');
  }

  // The size of the picture that really arrives, so the OBS source takes the phone's shape (upright or on its side).
  let lastSize = '';
  function reportSize() {
    const w = video.videoWidth, h = video.videoHeight;
    const key = video.srcObject && w && h ? `${w}x${h}` : '';
    if (key === lastSize) return;
    lastSize = key;
    local('size', key ? { width: w, height: h } : null);
  }
  video.addEventListener('loadedmetadata', reportSize);
  video.addEventListener('resize', reportSize);

  function openPc() {
    closePc();
    const mine = (pc = new RTCPeerConnection({ iceServers: [] })); // same network: no STUN or TURN needed
    mine.ontrack = (e) => {
      video.srcObject = e.streams[0] || new MediaStream([e.track]);
      video.play().catch(() => {});
    };
    mine.onicecandidate = (e) => { if (e.candidate && pc === mine) signal('ice', e.candidate.toJSON()); };
    mine.onconnectionstatechange = () => {
      if (pc !== mine) return;
      const s = mine.connectionState;
      if (s === 'connected') local('state', 'connected');
      else if (s === 'failed' || s === 'disconnected' || s === 'closed') { video.srcObject = null; reportSize(); local('state', 'idle'); }
    };
  }

  async function onMessage(m) {
    if (m.type === 'hello') {
      openPc(); // a new negotiation from the phone
      if (config.mode === 'relay') local('label', m.data); // in local mode the controller reads it from the message itself
    } else if (m.type === 'offer' && m.data && m.data.sdp) {
      if (!pc) openPc();
      const mine = pc;
      await mine.setRemoteDescription({ type: 'offer', sdp: m.data.sdp });
      const answer = await mine.createAnswer();
      await mine.setLocalDescription(answer);
      signal('answer', { sdp: answer.sdp });
      for (const c of pending.splice(0)) { try { await mine.addIceCandidate(c); } catch (e) { /* stale */ } }
    } else if (m.type === 'ice' && m.data) {
      if (pc && pc.remoteDescription) { try { await pc.addIceCandidate(m.data); } catch (e) { /* stale */ } }
      else pending.push(m.data); // the candidate came ahead of the offer
    } else if (m.type === 'bye') {
      closePc();
    }
  }

  async function fetchConfig() {
    const r = await fetch(`/camera/rx/config?c=${encodeURIComponent(id)}`, { cache: 'no-store' });
    if (!r.ok) throw new Error(String(r.status));
    return r.json();
  }

  // Local mode: long polling on the controller.
  async function pollLoop() {
    try {
      const r = await (await fetch(`/camera/rx/recv?c=${encodeURIComponent(id)}&after=-1`)).json();
      after = r.last;
    } catch (e) { /* retried below */ }
    signal('rxhello', null); // if a phone is already streaming, it starts over so this page can answer
    for (;;) {
      try {
        const r = await fetch(`/camera/rx/recv?c=${encodeURIComponent(id)}&after=${after}&wait=20`);
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

  // Relay mode: one WebSocket to the relay. The page proves it is this computer's with the key, and tells the relay which
  // phone code is valid (an empty code means "not accepting phones"). It also asks the controller for the config every few
  // seconds: that is how a new QR code, or "stop accepting", reaches the relay, and how the panel knows this page is alive.
  function relayLoop() {
    let beat = 0;
    const registerPhoneKey = () => {
      if (socket && socket.readyState === 1) socket.send(JSON.stringify({ type: 'phone-key', hash: config.phoneHash || '' }));
    };
    const open = () => {
      const s = (socket = new WebSocket(config.ws));
      s.onopen = () => s.send(JSON.stringify({ type: 'join', role: 'rx', key: config.key }));
      s.onmessage = async (e) => {
        let m;
        try { m = JSON.parse(e.data); } catch (err) { return; }
        if (m.type === 'joined') {
          registerPhoneKey();
          signal('rxhello', null);
        } else {
          await onMessage(m);
        }
      };
      s.onclose = () => {
        clearInterval(beat);
        if (socket !== s) return;
        socket = null;
        closePc();
        setTimeout(open, 3000);
      };
      beat = setInterval(() => { if (s.readyState === 1) s.send('ping'); }, 25000);
    };
    open();
    (async () => {
      for (;;) {
        await sleep(3000);
        try {
          const next = await fetchConfig();
          if (next.mode !== 'relay') continue;
          const changed = (next.phoneHash || '') !== (config.phoneHash || '');
          config = next;
          if (changed) registerPhoneKey();
        } catch (e) { /* the controller is restarting */ }
      }
    })();
  }

  async function start() {
    for (;;) {
      try {
        config = await fetchConfig();
        break;
      } catch (e) {
        await sleep(1500); // the controller may still be starting
      }
    }
    if (config.mode === 'relay') relayLoop();
    else pollLoop();
  }

  if (id) start();
})();
