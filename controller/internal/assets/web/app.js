// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

// Control panel. All text that comes from TikTok (names, handles, photos) goes in through textContent / img.src only.
const $=id=>document.getElementById(id);
const token=document.querySelector('meta[name="panel-token"]').content;
let state={},busy=false,aboutDirty=false,ctxDirty=false,autoTried=false,fetchedAt=Date.now(),assistantInfo=null;
const phases={disconnected:'Desconectado',login:'Entrar no TikTok',ready:'Pronto',creating:'Criando sala',live:'Ao vivo',ending:'Encerrando',closed:'Encerrada',uncertain:'Verificar',recovery:'Recuperação'};
const onAir=s=>['creating','live','ending'].includes(s.phase);
const canSwitch=s=>['disconnected','login','ready','closed'].includes(s.phase);

// ---- helpers
const isDefaultName=n=>/^user\d{6,}$/.test(n||'');
const nameOf=a=>a?((a.name&&!isDefaultName(a.name))?a.name:(a.handle?('@'+a.handle):'Conta TikTok')):'Nenhuma conta';
function setAvatar(el,a){
  el.replaceChildren();el.textContent=a?(nameOf(a).replace(/^@/,'').trim().charAt(0)||'?').toUpperCase():'?';
  if(a&&/^https:\/\//.test(a.avatar||'')){const img=document.createElement('img');img.alt='';img.referrerPolicy='no-referrer';
    img.onload=()=>{el.textContent='';el.append(img);};img.src=a.avatar;}
}
function icon(id){const s=document.createElementNS('http://www.w3.org/2000/svg','svg');s.setAttribute('class','ic');const u=document.createElementNS('http://www.w3.org/2000/svg','use');u.setAttribute('href','#'+id);s.append(u);return s;}
const fmt=sec=>{sec=Math.max(0,Math.floor(sec));const h=Math.floor(sec/3600),m=Math.floor(sec%3600/60),s=sec%60;return (h?h+':'+String(m).padStart(2,'0'):m)+':'+String(s).padStart(2,'0');};

// confirmation dialog (confirm() is unreliable inside OBS docks)
function ask({title,facts=[],note='',ok,dangerous=false}){
  return new Promise(resolve=>{
    $('dlgTitle').textContent=title;$('dlgNote').textContent=note;$('dlgNote').hidden=!note;
    const dl=$('dlgFacts');dl.replaceChildren();
    for(const [k,v] of facts){const dt=document.createElement('dt');dt.textContent=k;const dd=document.createElement('dd');dd.textContent=v;dl.append(dt,dd);}
    const okb=$('dlgOk');okb.textContent=ok;okb.classList.toggle('dangerous',dangerous);
    const done=v=>{$('modal').hidden=true;okb.onclick=$('dlgCancel').onclick=null;document.removeEventListener('keydown',key);resolve(v);};
    const key=e=>{if(e.key==='Escape')done(false);};
    document.addEventListener('keydown',key);okb.onclick=()=>done(true);$('dlgCancel').onclick=()=>done(false);
    $('modal').hidden=false;$('dlgCancel').focus();
  });
}

// ---- rendering
function renderAccounts(s){
  const list=s.accounts||[],active=list.find(a=>a.active),allowed=canSwitch(s)&&!busy;
  const key=JSON.stringify([list,allowed,s.accountList]);
  $('acctBtn').hidden=!s.accountList;
  setAvatar($('acctAvatar'),active);
  if($('acctMenu').dataset.key===key)return;$('acctMenu').dataset.key=key;
  setAvatar($('menuAvatar'),active);$('menuName').textContent=nameOf(active);$('menuHandle').textContent=active?(active.handle?'@'+active.handle:'em uso'):'Conecte ou adicione uma conta';
  const ul=$('accounts');ul.replaceChildren();
  for(const a of list.filter(x=>!x.active)){
    const li=document.createElement('li');li.className='menuRow';
    const use=document.createElement('button');use.className='menuItem';use.role='menuitem';use.disabled=!allowed;
    const av=document.createElement('span');av.className='avatar';setAvatar(av,a);
    const who=document.createElement('span');who.className='who';const b=document.createElement('b');b.textContent=nameOf(a);
    const sm=document.createElement('small');sm.textContent=(a.handle?'@'+a.handle+' · ':'')+(a.expired?'sessão expirada: entre de novo':'usar esta conta');who.append(b,sm);
    use.append(av,who);use.onclick=()=>{closeMenu();action('account/use',{id:a.id});};
    const rm=document.createElement('button');rm.className='menuItem x';rm.title='Remover da lista';rm.textContent='✕';rm.disabled=busy;
    rm.onclick=async()=>{closeMenu();if(await ask({title:'Remover esta conta da lista?',facts:[['Conta',nameOf(a)]],note:'A sessão salva neste computador é apagada. Você poderá entrar de novo depois.',ok:'Remover',dangerous:true}))action('account/remove',{id:a.id});};
    li.append(use,rm);ul.append(li);
  }
  $('addAccount').disabled=!allowed;$('logout').disabled=!allowed||!active;
  $('logout').hidden=!active;
  let note=$('menuNote');if(note)note.remove();
  if(!allowed&&!busy){note=document.createElement('div');note.id='menuNote';note.className='menuNote';note.textContent='Troque de conta fora de uma LIVE.';$('acctMenu').append(note);}
}
function render(value){
  state=value;fetchedAt=Date.now();
  const live=onAir(state);
  $('app').dataset.phase=state.phase;
  $('badge').textContent=phases[state.phase]??state.phase;
  $('statusText').textContent=busy&&state.phase==='disconnected'?'Conectando ao OBS e ao TikTok…':(state.detail||'');
  $('spin').hidden=!(busy||['creating','ending'].includes(state.phase));
  $('connect').hidden=busy||state.phase!=='disconnected';
  $('connect').textContent='';$('connect').append(icon('i-refresh'),document.createTextNode(' Conectar'));
  $('setup').hidden=live;$('onair').hidden=!live;
  // "Sobre mim" sits above the start card; during a LIVE it moves below the LIVE card.
  const about=$('aboutCard');
  if(live&&about.previousElementSibling!==$('onair'))$('onair').after(about);
  else if(!live&&about.nextElementSibling!==$('setup'))$('setup').before(about);
  // setup
  $('start').disabled=busy||!['ready','closed','disconnected','login'].includes(state.phase)||!state.obs;
  // on air
  $('liveTitle').textContent=state.liveTitle||'LIVE';
  const wantView=live?'/live?compact=1':'about:blank';if($('liveView').dataset.src!==wantView){$('liveView').dataset.src=wantView;$('liveView').src=wantView;}
  $('end').disabled=busy||!state.canEnd;
  $('viewers').textContent=state.view?.viewers??state.view?.stats?.viewers??'–';
  $('guard').textContent=(state.warningMonitor?'Vigilância de avisos do TikTok ativa: a LIVE é encerrada automaticamente se ele sinalizar. ':'')+(state.productDetails?'Contexto para IA carregado.':'');
  tick();
  // sections
  if(document.activeElement!==$('autoEnd'))$('autoEnd').checked=state.autoEndOnWarning!==false;
  const ap=state.autopilot||{};
  $('autoThanks').disabled=busy||state.phase!=='live';$('autoAnswers').disabled=busy||state.phase!=='live';
  if(document.activeElement!==$('autoThanks'))$('autoThanks').checked=!!ap.thanks;
  if(document.activeElement!==$('autoAnswers'))$('autoAnswers').checked=!!ap.answers;
  $('ctxSave').textContent=state.phase==='live'?'Atualizar na LIVE':'Salvar contexto';$('ctxSave').disabled=busy||!ctxDirty;
  $('products').disabled=busy||['creating','ending'].includes(state.phase);
  if(!ctxDirty&&document.activeElement!==$('products'))$('products').value=state.context??'';
  if(!aboutDirty&&document.activeElement!==$('aboutText'))$('aboutText').value=state.aboutMe?.text??'';
  $('aboutText').disabled=busy||!state.aboutMe?.enabled;
  $('aboutSave').disabled=busy||!state.aboutMe?.enabled||!aboutDirty||!$('aboutText').value.trim();
  $('monitor').textContent=state.monitor?.active?`Monitoramento ativo: ${state.monitor.events} eventos · ${state.monitor.imMessages} mensagens IM`:(state.monitor?.dir?`Último registro: runtime/traces/${state.monitor.dir}`:'');
  renderAccounts(state);renderCamera(state.camera);
}
function tick(){
  if(!onAir(state))return;
  $('elapsed').textContent=state.startedAt?fmt(Date.now()/1000-state.startedAt):'0:00';
  $('remaining').textContent=state.remainingSec!=null?fmt(state.remainingSec-(Date.now()-fetchedAt)/1000):'sem limite';
}
setInterval(tick,1000);

// ---- talking to the controller
async function refresh(){
  try{const r=await fetch('/api/state',{cache:'no-store'});if(r.ok)render(await r.json());}
  catch{$('statusText').textContent='Painel local indisponível. Se estiver ao vivo, pare a transmissão no OBS.';}
}
async function action(path,data={}){
  busy=true;render(state);$('error').hidden=true;
  try{
    const r=await fetch('/api/'+path,{method:'POST',headers:{'Content-Type':'application/json','X-Panel-Token':token},body:JSON.stringify(data)});
    const v=await r.json();if(r.status===403){location.reload();return;}
    if(!r.ok)throw Error(v.error??'Operação não concluída.');
    if(v.source){camNote='Fonte "'+v.source+'" adicionada na cena '+v.scene+'.';render(state);}else render(v);
  }catch(e){$('error').textContent=e.message;$('error').hidden=false;}
  finally{busy=false;await refresh();}
}

// ---- account menu
function closeMenu(){$('acctMenu').hidden=true;$('acctBtn').setAttribute('aria-expanded','false');}
$('acctBtn').onclick=e=>{e.stopPropagation();const open=$('acctMenu').hidden;$('acctMenu').hidden=!open;$('acctBtn').setAttribute('aria-expanded',String(open));};
document.addEventListener('click',e=>{if(!$('acctMenu').hidden&&!$('acctMenu').contains(e.target))closeMenu();});
document.addEventListener('keydown',e=>{if(e.key==='Escape'&&$('modal').hidden)closeMenu();});
$('addAccount').onclick=()=>{closeMenu();action('account/add');};
$('logout').onclick=async()=>{closeMenu();const a=(state.accounts||[]).find(x=>x.active);
  if(await ask({title:'Sair desta conta?',facts:[['Conta',nameOf(a)]],note:'A sessão salva neste computador é apagada. Para voltar, entre de novo.',ok:'Sair',dangerous:true}))action('account/logout');};

// ---- controls
$('connect').onclick=()=>action('connect');
$('start').onclick=()=>action('start-dialog'); // the OBS window asks for the title, duration and context, and for the confirmation
$('end').onclick=async()=>{
  if(await ask({title:'Encerrar a LIVE?',note:'A transmissão para e a sala é encerrada no TikTok. Isso não pode ser desfeito.',ok:'Encerrar LIVE',dangerous:true}))action('end');
};
$('products').oninput=()=>{ctxDirty=true;render(state);};
$('ctxSave').onclick=async()=>{await action('context',{text:$('products').value});if($('error').hidden)ctxDirty=false;};
$('aboutText').oninput=()=>{aboutDirty=true;render(state);};
$('aboutSave').onclick=async()=>{
  if(!await ask({title:'Alterar o Sobre mim?',note:'O texto muda na sua conta TikTok (o mesmo do app do TikTok) e pode passar por moderação.',ok:'Salvar'}))return;
  await action('about',{text:$('aboutText').value});if($('error').hidden)aboutDirty=false;
};
$('autoEnd').onchange=()=>action('auto-end',{enabled:$('autoEnd').checked});
$('autoThanks').onchange=()=>action('autopilot',{thanks:$('autoThanks').checked});
$('autoAnswers').onchange=()=>action('autopilot',{answers:$('autoAnswers').checked});

// ---- connect an AI assistant: ready-to-copy configuration for this installation
async function postApi(path,data={}){const r=await fetch('/api/'+path,{method:'POST',headers:{'Content-Type':'application/json','X-Panel-Token':token},body:JSON.stringify(data)});if(!r.ok)throw Error('falhou');return r.json();}
async function copyText(text){
  try{await navigator.clipboard.writeText(text);return true;}catch{/* fall back below */}
  const ta=document.createElement('textarea');ta.value=text;ta.className='offscreen';document.body.append(ta);ta.select();
  let ok=false;try{ok=document.execCommand('copy');}catch{/* no clipboard */}ta.remove();return ok;
}
document.querySelectorAll('[data-copy]').forEach(btn=>{btn.onclick=async()=>{
  const label=btn.querySelector('span'),old=label.textContent;
  try{
    assistantInfo??=await postApi('assistant-info');
    const text=assistantInfo[btn.dataset.copy];
    if(await copyText(text))label.textContent='Copiado ✓';
    else{label.textContent='Copie abaixo';$('moreCfg').value=text;$('moreCfg').closest('details').open=true;}
  }catch{label.textContent='Erro';}
  setTimeout(()=>{label.textContent=old;},2200);
};});
postApi('assistant-info').then(i=>{assistantInfo=i;$('moreCfg').value=i.codexToml;}).catch(()=>{});
// Connect by itself once when the panel opens (read-only: it only looks at OBS and the TikTok session).
(async function boot(){
  await refresh();
  if(!autoTried&&state.phase==='disconnected'){autoTried=true;action('connect');}
  setInterval(refresh,2000);
})();

// Self-refresh when the panel's own code changes on the server (an update), instead of staying stale.
(async function watchForUpdates(){
  const sign=async()=>(await Promise.all(['/app.js','/style.css'].map(u=>fetch(u,{cache:'no-store'}).then(r=>r.text())))).join('\u0000');
  try{const first=await sign();setInterval(async()=>{try{if(await sign()!==first)location.reload();}catch{/* offline */}},20000);}catch{/* ignore */}
})();

// ---- phones as wireless cameras: one row per camera (each OBS source has its own), one QR code at a time
let camSel='',camQrKey='',camNote='',camOpening='',camNonce=0;
function camButton(text,iconId,onclick,disabled){const b=document.createElement('button');b.className='ghost';b.append(icon(iconId));
  const sp=document.createElement('span');sp.textContent=text;b.append(sp);b.disabled=!!disabled;b.onclick=onclick;return b;}
function renderCamera(c){
  c=c||{};
  $('camBox').hidden=!c.available;if(!c.available)return;
  const cams=c.cameras||[];
  if(!cams.some(x=>x.id===camSel))camSel='';
  const open=cams.find(x=>x.open&&x.id===camSel)||cams.find(x=>x.open);
  if(open)camSel=open.id;
  const free=!busy;
  // the list
  const list=$('camList'),key=JSON.stringify([cams.map(x=>[x.id,x.name,x.open,x.receiver,x.connected,x.label,x.width,x.height]),free,camOpening]);
  if(list.dataset.key!==key){
    list.dataset.key=key;list.replaceChildren();
    if(!cams.length){const p=document.createElement('p');p.className='hint';p.textContent='Nenhuma câmera ainda. Adicione uma: ela cria a fonte no OBS e mostra o código para o celular.';list.append(p);}
    for(const x of cams){
      const row=document.createElement('div');row.className='camRow';
      const info=document.createElement('div');info.className='camInfo';
      const name=document.createElement('b');name.textContent=x.name||'Câmera';
      const st=document.createElement('small');
      st.textContent=x.connected?('Conectada'+(x.label?': '+x.label:'')+(x.width?' · '+x.width+'×'+x.height:'')):(camOpening===x.id?'Abrindo a conexão segura…':(x.open?'Pareamento aberto: aguardando o celular':(x.receiver?'Fonte ativa no OBS, celular desconectado':'Sem fonte ativa no OBS')));
      info.append(name,st);
      const btns=document.createElement('div');btns.className='camBtns';
      btns.append(
        camButton(x.open?'Mostrar código':'Parear','i-plus',()=>camPair(x.id),!free),
        camButton('Novo código','i-refresh',()=>camRevoke(x.id),!free),
        camButton('Parar','i-stop',()=>{camNote='';action('camera/stop',{id:x.id});},!free||!x.open));
      if(!x.receiver&&!x.open&&!x.connected)btns.append(camButton('Remover','i-logout',()=>camRemove(x),!free));
      row.append(info,btns);list.append(row);
    }
  }
  // the QR code of the camera being paired
  const hosts=c.hosts||[],showing=open&&c.listening&&hosts.length>0;
  $('camPair').hidden=!showing;
  if(showing){
    const sel=$('camIp'),qk=JSON.stringify([open.id,hosts,camNonce]);
    if(camQrKey!==qk){
      camQrKey=qk;sel.replaceChildren();hosts.forEach((host,i)=>sel.add(new Option(host,String(i))));
      $('camQr').src='/camera/qr.svg?c='+encodeURIComponent(open.id)+'&i=0&v='+Date.now();
    }
    $('camPairTitle').textContent='Parear: '+(open.name||'câmera');
    $('camIpBox').hidden=hosts.length<2;
  }else camQrKey='';
  const relay=c.mode==='relay',tunnel=c.mode==='tunnel'||relay;
  $('camQrHint').textContent=tunnel?'Aponte a câmera do celular para o código: ele abre uma página segura (HTTPS válido), sem aviso.':'Aponte a câmera do celular para o código. Se o navegador avisar sobre a conexão, toque em "Avançado" e em "Continuar": o certificado é deste computador. No iPhone a câmera costuma ser recusada nesse modo.';
  $('camStatus').textContent=camNote;$('camStatus').hidden=!camNote;
  const note=$('camNote');
  note.hidden=!(c.needsTunnel||tunnel);
  note.textContent=relay?'Conexão segura pelo servidor da câmera do projeto: ele só leva a página e as mensagens para ligar; o vídeo e o som vão direto do celular para o OBS. Cada câmera tem o seu código, que não muda: o celular pareado volta sozinho.':(tunnel?'Conexão segura pela Cloudflare: só a página e as mensagens para ligar passam por ela; o vídeo e o som vão direto do celular para o OBS. O código muda a cada vez que a conexão é aberta.':(c.needsTunnel?'Para o iPhone (e sem avisos) instale uma vez o cloudflared: abra o PowerShell, rode  winget install Cloudflare.cloudflared  e toque em "Parear" de novo. Sem ele, a câmera usa a rede local com aviso de certificado.':''));
  $('camCopyRow').hidden=!c.needsTunnel||tunnel;
  $('camAdd').disabled=!free||onAir(state)||!state.obs;
}
async function camPair(id){camNote='';camOpening=id;camSel=id;camNonce++;render(state);await action('camera/pair',{id});camOpening='';render(state);}
async function camRevoke(id){
  if(await ask({title:'Gerar um novo código?',note:'O celular pareado com esta câmera deixa de funcionar e qualquer cópia do código antigo também. A fonte no OBS continua.',ok:'Gerar novo código'})){camNote='';camNonce++;action('camera/revoke',{id});}
}
async function camRemove(x){
  if(await ask({title:'Remover esta câmera?',facts:[['Câmera',x.name||'']],note:'Só some da lista: a fonte no OBS (se ainda existir) deve ser apagada por lá.',ok:'Remover',dangerous:true})){camNote='';action('camera/remove',{id:x.id});}
}
$('camIp').onchange=()=>{$('camQr').src='/camera/qr.svg?c='+encodeURIComponent(camSel)+'&i='+encodeURIComponent($('camIp').value)+'&v='+Date.now();};
$('camAdd').onclick=()=>action('camera/add-source');
$('camCopy').onclick=async()=>{const l=$('camCopy').querySelector('span'),old=l.textContent;
  l.textContent=(await copyText('winget install Cloudflare.cloudflared'))?'Copiado ✓':'Copie: winget install Cloudflare.cloudflared';setTimeout(()=>{l.textContent=old;},2400);};
