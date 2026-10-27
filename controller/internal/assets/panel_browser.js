// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

(function(){
  function md5(text) {
    const bytes = new TextEncoder().encode(text);
    const size = Math.ceil((bytes.length + 9) / 64) * 64;
    const padded = new Uint8Array(size); padded.set(bytes); padded[bytes.length] = 128;
    const view = new DataView(padded.buffer);
    view.setUint32(size - 8, bytes.length * 8, true);
    const shifts = [7,12,17,22,5,9,14,20,4,11,16,23,6,10,15,21];
    let h = [0x67452301,0xefcdab89,0x98badcfe,0x10325476];
    for (let offset = 0; offset < size; offset += 64) {
      let [a,b,c,d] = h;
      for (let j = 0; j < 64; j++) {
        let f, g;
        if (j < 16) { f = (b & c) | (~b & d); g = j; }
        else if (j < 32) { f = (d & b) | (~d & c); g = (5*j+1)%16; }
        else if (j < 48) { f = b ^ c ^ d; g = (3*j+5)%16; }
        else { f = c ^ (b | ~d); g = (7*j)%16; }
        const n = (a + f + Math.floor(Math.abs(Math.sin(j+1))*4294967296)
          + view.getUint32(offset+g*4,true)) | 0;
        const shift = shifts[Math.floor(j/16)*4+j%4];
        const next = (b + ((n << shift) | (n >>> (32-shift)))) | 0;
        a=d; d=c; c=b; b=next;
      }
      h = h.map((v,i) => (v+[a,b,c,d][i])|0);
    }
    return h.map(v => [0,8,16,24].map(s => ((v>>>s)&255).toString(16).padStart(2,'0')).join('')).join('');
  }

  if(location.origin!=='https://webcast.tiktokv.com') throw Error('Wrong service origin');
  if(window.__tiktokPanelV9) return {ready:true};
  const common={aid:'8311',live_studio:'1',app_name:'tiktok_live_studio',channel:'studio',
    version_code:'1.38.0',webcast_sdk_version:'1380',device_platform:'windows',live_mode:'6',
    language:'en',app_language:'en',webcast_language:'en',timezone_name:Intl.DateTimeFormat().resolvedOptions().timeZone};
  let requested=false, owned=null;
  async function request(path,data,query={}) {
    const u=new URL(path,location.origin);u.search=new URLSearchParams({...common,...query});
    const body=data==null?undefined:new URLSearchParams(data).toString();
    const headers=body===undefined?{}:{'Content-Type':'application/x-www-form-urlencoded; charset=UTF-8',
      'sdk_aid':'8311','x-ss-stub':md5(body)};
    const r=await fetch(u,{method:body===undefined?'GET':'POST',body,headers,
      credentials:'include',redirect:'error',signal:AbortSignal.timeout(8000)});
    if(r.status!==200) throw Error('Service HTTP failure');
    return await r.json();
  }
  const check=async()=>{
    const b=await request('/webcast/room/create_info/');
    return {code:b.status_code,liveStatus:b.data?.live_status??null,
      enabled:b.data?.live_scenario?.enable_live_studio===true,
      banned:b.data?.ban_status?.is_ban??null,pendingCreation:requested,
      defaults:{cover_uri:b.data?.cover?.uri??null,
        screenshot_cover_status:Number.isInteger(b.data?.screenshot_cover_status)?b.data.screenshot_cover_status:null,
        is_group_live_session:typeof b.data?.is_group_live_session==='boolean'?b.data.is_group_live_session:null}};
  };
  const create=async({fields})=>{
    if(requested) throw Error('Creation already attempted');
    if(!fields||typeof fields.title!=='string'||fields.live_studio!=='1'||
       Object.values(fields).some(v=>typeof v!=='string'))throw Error('Invalid create fields');
    const pre=await check();
    if(pre.code!==0||pre.liveStatus!==4||!pre.enabled||pre.banned!==false)
      return {status_code:-1};
    requested=true;
    const b=await request('/webcast/room/create/',fields);
    if(b.status_code===0)owned=b.data;
    else requested=false; // a definite server rejection created nothing; the controller reconciles via check
    return b;
  };
  const ping=async({binding,status})=>{
    if(![1,2,4].includes(status))throw Error('Invalid status');
    const b=await request('/webcast/room/ping/anchor/',{status:String(status),
      room_id:binding.ping_room_id,stream_id:binding.stream_id});
    return {code:b.status_code};
  };
  const verify=async(binding)=>{
    const b=await request('/webcast/room/info/',null,{room_id:binding.room_id});
    const room=b.data?.room??b.data;
    return {code:b.status_code,matches:room?.id_str===binding.room_id,
      status:Number.isInteger(room?.status)?room.status:null,
      ended:b.status_code===0&&room?.id_str===binding.room_id&&room.status===4};
  };
  // Read-only observation: full room/info and create_info bodies for diffing.
  const probe=async(binding)=>{
    const [info,create]=await Promise.all([
      request('/webcast/room/info/',null,{room_id:binding.room_id}),
      request('/webcast/room/create_info/')]);
    return {info,create};
  };
  // Raw IM batch as base64; only the IM fetch path on the service origin is allowed.
  const imfetch=async({url})=>{
    const u=new URL(url);
    if(u.origin!==location.origin||u.pathname!=='/webcast/im/fetch/')throw Error('Unsupported URL');
    const r=await fetch(u,{credentials:'include',redirect:'error',signal:AbortSignal.timeout(12000)});
    const buf=new Uint8Array(await r.arrayBuffer());
    let text='';for(let i=0;i<buf.length;i+=32768)text+=String.fromCharCode(...buf.subarray(i,i+32768));
    return {status:r.status,type:r.headers.get('content-type'),size:buf.length,b64:btoa(text)};
  };
  async function requestJson(path,obj,query={}){
    const u=new URL(path,location.origin);u.search=new URLSearchParams({...common,...query});
    const body=JSON.stringify(obj);
    const r=await fetch(u,{method:'POST',body,credentials:'include',redirect:'error',
      headers:{'Content-Type':'application/json','sdk_aid':'8311','x-ss-stub':md5(body)},
      signal:AbortSignal.timeout(8000)});
    if(r.status!==200) throw Error('Service HTTP failure');
    return await r.json();
  }
  // Account-level "Sobre mim".
  const aboutme=async()=>{
    const b=await request('/webcast/anchor/about_me/');
    const a=b.data?.about_me;
    const list=Array.isArray(a?.template_list)?a.template_list:[];
    const t=list.find(x=>String(x.id)===String(a?.current_template_id))??list[0];
    const boxes=Array.isArray(t?.input_box_list)?t.input_box_list:[];
    return {code:b.status_code,enabled:a?.switch_status===true,templateId:String(t?.id??a?.current_template_id??''),
      texts:boxes.map(x=>typeof x.content==='string'?x.content:''),
      max:boxes[0]?.max_character_count??240,imageUri:a?.image?.uri??'',audit:a?.audit_status??null};
  };
  // Only called from an explicit user action: read fresh, change the first box, write, read back.
  const aboutmesave=async({text})=>{
    const cur=await aboutme();
    if(cur.code!==0||!cur.enabled||!cur.texts.length||!cur.templateId)return {saved:false,reason:'unavailable'};
    if(typeof text!=='string'||!text.trim().length||text.length>cur.max)return {saved:false,reason:'invalid_text'};
    const body={template_id:cur.templateId,method_type:1,input_content_list:[text,...cur.texts.slice(1)]};
    if(cur.imageUri)body.image_uri=cur.imageUri;
    const b=await requestJson('/webcast/anchor/about_me/update/',body,{aid:'8311'});
    const after=await aboutme();
    return {saved:b.status_code===0,code:b.status_code,audit:b.data?.audit_status??null,text:after.texts[0]??null};
  };
  // Read-only counters shown in the panel (viewers, likes, comments...).
  const roomstats=async(binding)=>{
    const b=await request('/webcast/room/info/',null,{room_id:binding.room_id});
    const r=b.data?.room??b.data;const s=r?.stats??{};
    const n=v=>Number.isInteger(v)?v:null;
    return {code:b.status_code,status:n(r?.status),viewers:n(r?.user_count),total:n(s.total_user),
      enter:n(s.enter_count),likes:n(s.like_count)??n(s.digg_count),comments:n(s.comment_count),
      shares:n(s.share_count),follows:n(s.follow_count)??n(s.room_follow_count),gifters:n(s.gift_uv_count)};
  };
  // Host chat message: JSON POST, 200 characters at most.
  const chat=async({room_id,content})=>{
    if(!/^\d{1,25}$/.test(String(room_id)))throw Error('Invalid room');
    if(typeof content!=='string'||!content.trim()||content.length>200||/[\u0000-\u001f\u007f]/.test(content))throw Error('Invalid message');
    const b=await requestJson('/webcast/room/chat/',{room_id:String(room_id),content,emotes_with_index:'',
      client_start_timestamp_millisecond:Date.now()},{aid:'8311'});
    return {code:b.status_code,prompts:typeof b.data?.prompts==='string'?b.data.prompts.slice(0,200):null};
  };
  // PKCE sign-in: the ticket made by the sign-in window becomes a session of the LIVE
  // service (cookies set by the service itself). Nothing secret is returned, only whether it worked and who signed in.
  const pkce=async({ticket,verifier,challenge})=>{
    if(![ticket,verifier,challenge].every(v=>typeof v==='string'&&v.length>0&&v.length<=4096))throw Error('Invalid ticket');
    const b=await request('/passport/user/login_by_verify_ticket/',{aid:'8311',language:'en',verify_ticket:ticket,
      need_bind_mobile:'0',scene:'pkce_login',code_challenge:challenge,code_verifier:verifier});
    const d=b.data&&typeof b.data==='object'?b.data:{};
    return {ok:b.message==='success',message:typeof b.message==='string'?b.message.slice(0,80):null,
      errorCode:d.error_code??null,user:{id:String(d.user_id_str??d.user_id??''),
      name:String(d.screen_name??d.nickname??''),handle:String(d.username??d.unique_id??''),avatar:String(d.avatar_url??'')}};
  };
  // The account's public profile as the LIVE service shows it:
  // display name, @handle and photo. Read-only.
  const profile=async()=>{
    const b=await request('/webcast/game/user/profile/self');
    const u=b.data&&b.data.user&&typeof b.data.user==='object'?b.data.user:{};
    const url=o=>{const v=o&&Array.isArray(o.url_list)?o.url_list[0]:'';return typeof v==='string'&&v.startsWith('https://')?v:'';};
    return {code:b.status_code,name:String(u.nick_name??'').slice(0,80),handle:String(u.unique_id??'').slice(0,40),
      avatar:url(u.avatar_medium)||url(u.avatar_thumb)||url(u.avatar_large)};
  };
  const drop=async(binding)=>{
    const v=await verify(binding);
    if(!v.ended)return {cleared:false};
    if(owned){delete owned.stream_url;delete owned.multi_stream_url;}
    owned=null;requested=false;return {cleared:true};
  };
  Object.defineProperty(window,'__tiktokPanelV9',{value:{check,create,ping,verify,drop,probe,imfetch,aboutme,aboutmesave,roomstats,chat,pkce,profile}});
  return {ready:true};

})();