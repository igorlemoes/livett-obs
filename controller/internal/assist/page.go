// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package assist

// pageHTML is the phone-friendly view of the TikTok sign-in tab: screenshots in, the taps and text of the user out.
const pageHTML = `<!doctype html><html lang="pt-BR"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><meta name="robots" content="noindex">
<title>Login TikTok</title>
<style>
:root{color-scheme:dark light;--bg:#111;--fg:#eee;--mut:#999;--line:#333;--acc:#fe2c55}
@media (prefers-color-scheme:light){:root{--bg:#fff;--fg:#111;--mut:#666;--line:#ddd}}
body{margin:0;background:var(--bg);color:var(--fg);font:16px system-ui,sans-serif}
main{max-width:640px;margin:0 auto;padding:12px 16px 40px}
h1{font-size:1.1rem;margin:.4rem 0}
.hint{color:var(--mut);font-size:.9rem;margin:.3rem 0 .8rem}
#view{width:100%;border:1px solid var(--line);border-radius:10px;display:block;touch-action:manipulation;background:#000}
.row{display:flex;gap:8px;margin:10px 0;flex-wrap:wrap}
input{flex:1;min-width:0;padding:12px;font-size:1.1rem;border-radius:8px;border:1px solid var(--line);background:transparent;color:var(--fg)}
button{padding:11px 14px;border-radius:8px;border:1px solid var(--line);background:transparent;color:var(--fg);font-size:.95rem}
button.primary{background:var(--acc);border-color:var(--acc);color:#fff}
#status{color:var(--mut);font-size:.85rem;min-height:1.2em}
</style></head><body><main>
<h1>Login do TikTok (temporário)</h1>
<p class="hint">Toque na imagem como se fosse a tela do PC. Digite abaixo só o que o TikTok pedir (por exemplo, o código de 6 dígitos do e-mail). O que você digita vai direto ao navegador e não fica registrado.</p>
<img id="view" alt="Tela do login" draggable="false">
<div class="row"><input id="text" autocomplete="off" autocapitalize="off" spellcheck="false" inputmode="text" placeholder="Texto ou código">
<button class="primary" id="send">Enviar texto</button><button id="mask" type="button">Ocultar</button></div>
<p class="hint">Toque no campo do TikTok (na imagem): o teclado abre aqui embaixo. Digite e toque em <b>Enviar texto</b>; repita para o próximo campo.</p>
<div class="row"><button data-key="Enter">Enter</button><button data-key="Backspace">Apagar</button><button data-key="Tab">Tab</button>
<button data-go="qr">Tela do QR</button><button data-go="login">Tela de login</button><button id="stop">Encerrar</button></div>
<div id="status"></div>
<script>
const base=location.pathname.replace(/\/$/,'');const img=document.getElementById('view');const st=document.getElementById('status');
let busy=false,alive=true;
async function shot(){if(busy||!alive)return;busy=true;try{const r=await fetch(base+'/shot?'+Date.now(),{cache:'no-store'});
 if(r.status===410){alive=false;st.textContent='Sessão encerrada.';return}
 if(r.ok){const b=await r.blob();const u=URL.createObjectURL(b);const old=img.src;img.src=u;if(old.startsWith('blob:'))URL.revokeObjectURL(old);st.textContent=''}
 else st.textContent='Sem imagem ('+r.status+')'}catch(e){st.textContent='Sem conexão com o PC'}finally{busy=false}}
async function act(path,body){try{const r=await fetch(base+path,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body||{})});
 if(!r.ok)st.textContent='Falhou ('+r.status+')';setTimeout(shot,350)}catch(e){st.textContent='Sem conexão com o PC'}}
img.addEventListener('click',e=>{const r=img.getBoundingClientRect();act('/tap',{fx:(e.clientX-r.left)/r.width,fy:(e.clientY-r.top)/r.height});document.getElementById('text').focus()});
document.getElementById('send').onclick=()=>{const t=document.getElementById('text');if(t.value){act('/text',{text:t.value});t.value=''}};
document.getElementById('text').addEventListener('keydown',e=>{if(e.key==='Enter'){e.preventDefault();document.getElementById('send').click()}});
document.getElementById('mask').onclick=function(){const t=document.getElementById('text');const hide=t.type!=='password';t.type=hide?'password':'text';this.textContent=hide?'Mostrar':'Ocultar'};
document.querySelectorAll('[data-key]').forEach(b=>b.onclick=()=>act('/key',{key:b.dataset.key}));
document.querySelectorAll('[data-go]').forEach(b=>b.onclick=()=>act('/go',{to:b.dataset.go}));
document.getElementById('stop').onclick=async()=>{await act('/stop');alive=false;st.textContent='Encerrado. Pode fechar esta página.'};
shot();setInterval(shot,1200);
</script></main></body></html>`
