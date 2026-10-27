# Arquitetura

## 1. Visão geral

```
                         ┌────────────────────────── OBS Studio 32.x ───────────────────────────┐
                         │  plugin nativo (livett.dll, C++/Qt6)                         │
                         │   • inicia o controlador   • docks "Controle" e "Ao vivo" (QCef)      │
                         │   • janela flutuante nativa (Ferramentas)                             │
                         │   • API de controle local (HTTP 127.0.0.1, token por execução)        │
                         └───────────────┬───────────────────────────────────────────────────────┘
                                         │ inicia (uma vez por OBS)            API do plugin (127.0.0.1:porta efêmera)
                                         ▼                                              ▲
 assistente ──MCP/stdio──> controller.exe mcp ──HTTP──> controller.exe serve (127.0.0.1:8765) ──┘
                                                         │   estado, guardas, painel web, telemetria
                                                         │ CDP (porta de depuração local)
                                                         ▼
                                       Chrome gerenciado (perfil %LOCALAPPDATA%\TikTokLiveOBS\chrome-profile)
                                         aba webcast.tiktokv.com (script injetado) ──HTTPS──> TikTok
                                         aba de login (www.tiktok.com), janela escondida
 controller.exe watchdog  (processo separado, só durante a LIVE: dead-man switch)
```

Ideias centrais:
1. **O navegador é o cliente do TikTok.** O Chrome gerenciado guarda a sessão do usuário; nosso código injeta um script (`panel_browser.js`) numa aba de
   `webcast.tiktokv.com` e chama as APIs por `fetch` com cookies. Não exportamos cookies, não reimplementamos assinaturas.
2. **O controlador é a única autoridade sobre a LIVE** e é pequeno e testável (Go, sem dependências). O plugin só mostra e inicia.
3. **Segurança por camadas**: confirmação explícita, diário anterior ao pedido (sobrevive a quedas), vários gatilhos de encerramento e um watchdog independente.
4. **Telemetria passiva** grava tudo o que cruza as fronteiras (OBS, Chrome, IM) num trace mascarado, sem nunca interferir.

## 2. Processos e portas

| Processo | Papel | Porta |
| --- | --- | --- |
| `obs64.exe` + plugin | Interface e início do controlador | API de controle própria (porta efêmera + token em `%LOCALAPPDATA%\TikTokLiveOBS\obs-api.json`); **sem obs-websocket** |
| `controller.exe serve --background --root <dir>` | Painel, API, estado, monitor, telemetria | **8765** (loopback) |
| (o mesmo `serve`, só depois de "Parear celular") | Porta da câmera do celular | túnel: porta efêmera em `127.0.0.1`; sem `cloudflared`: **8766 a 8775** (HTTPS, rede local) |
| `controller.exe watchdog --root <dir>` | Dead-man switch; criado por `serve` ao iniciar uma LIVE; sai sozinho | — |
| `controller.exe mcp --root <dir> [--port 8765]` | Servidor MCP por stdio; conversa com o painel | — |
| `controller.exe assist ...` | Login remoto temporário (consentimento do usuário) | 8790 (loopback) + túnel opcional |
| Chrome gerenciado | Sessão e chamadas ao TikTok | depuração local de porta dinâmica (`DevToolsActivePort`) |

`--root` padrão = pasta do executável. No pacote: `...\livett\data\controller` (onde ficam `runtime\`).

## 3. Módulos Go (`controller/internal/`)

| Pacote | Responsabilidade | Observações |
| --- | --- | --- |
| `controller` | **Máquina de estados** da LIVE: conectar, iniciar, encerrar, recuperação, agendamento, monitor, chat, autopilot, "Sobre mim", observador de login | O coração; dependências por interface (`Browser`, `Obs`, `Recorder`) → testes com fakes |
| `session` | Ponte CDP com o Chrome: iniciar/anexar, aba de serviço, injeção do script, janelas escondidas, cópia da sessão, login em janela enxuta com ticket PKCE, cookies e identidade das contas, operações da aba de login | `BridgeName` = `__tiktokPanelV9` |
| `chrome` | Perfil persistente, localizar/iniciar/fechar o Chrome, `DevToolsActivePort` | Só perfis permitidos |
| `winhide` | Esconder/mostrar janelas do Chrome (Win32), achar o PID do navegador | `SW_HIDE` |
| `obs` | Cliente HTTP da API do plugin (mesmos nomes de pedido do obs-websocket) + `StreamOutput` (para só a saída ligada ao perfil/destino da LIVE) | Nunca chama `GetOutputList` |
| `ws` | WebSocket RFC 6455 mínimo, usado só para o CDP do Chrome (cliente loopback + `Accept` para testes) | Sem libs externas |
| `room` | Validação da resposta de `create`, IDs, destinos RTMP, corpo de encerramento, campos de criação | `PushTarget` não imprime segredos |
| `wire` | Protobuf mínimo do canal IM (decodificação limitada, gzip limitado, ACK, `Classify`) e decodificadores de exibição | Texto de espectador é não confiável |
| `impoll` | Uma rodada de `im/fetch` → despacha ao guard (ou só observa) | Deduplica sinais |
| `guard` | Coordenador de parada por sinal (latch, tempo limite por operação) | Falha de um lado não impede o outro |
| `watchdog` | Dead-man switch (processo separado) | Contrato de `runtime/*.json` |
| `telemetry` + `trace` | Captura passiva (CDP de rede, OBS, processo, sala, IM) e resumo; mascaramento de segredos | Nunca afeta a LIVE |
| `server` | HTTP do painel e do painel "Ao vivo" (loopback, token, CSP) | Rotas na seção 5 |
| `mcp` | 17 ferramentas, política, cliente do painel | Ver [MCP.md](MCP.md) |
| `assist` | Login remoto assistido (+ túnel cloudflared opcional) | Ver [OPERACAO.md](OPERACAO.md) §7 |
| `vault` | Contas salvas: lista pública + sessão de cada conta criptografada com DPAPI | Nenhum segredo na lista; ids validados |
| `camera` | Câmera do celular: hub de sinalização WebRTC, porta HTTPS da rede local, certificado e QR code | Ver §7.2 e [SEGURANCA.md](SEGURANCA.md) |
| `avatar` | Proxy de fotos de espectadores (só CDN do TikTok) | Para a janela nativa (Qt sem HTTPS) |
| `assets` | Painel web e script do navegador **embutidos** (`go:embed`) | Fonte única: `internal/assets/` |

## 4. Máquina de estados (`controller.Phase`)

```
disconnected ──Connect──> login ──(usuário entra; observador a cada 3 s)──> ready
      │                    ▲                                                  │ Start (confirmado)
      └──Connect (sessão ok)────────────────────────────────────────────────>│
                                                                              ▼
                                          creating ──ok──> live ──End/sinal/prazo/OBS parou/controle perdido──> ending
                                              │                                                                   │
                                              └─ falha/dúvida ─> uncertain <── término remoto não confirmado ──────┤
                                                                                                                  ▼
                                                                                                               closed
recovery: ao subir, se o diário (`broadcast.json`) não está em closed/idle/disconnected.
```

Pontos importantes de `Start`: confirma → fase `ready/closed` → OBS ocioso e cena = pedida → `check` da conta → sala anterior (se houver) verificada e descartada → guarda o destino atual do OBS (um destino já de TikTok é
trocado por "vazio") → **diário em `creating` antes do pedido** → `create` → grava os IDs → ping de preparação (status 1) → configura `rtmp_custom` no OBS → inicia a saída e espera até 8 s →
`live` → ping status 2 → inicia vigilância IM, estatísticas, watchdog. Qualquer erro depois de a sala existir: encerra a sala identificada e fica `uncertain`.
Nunca há repetição automática de `create`.

**Monitor** (a cada 1 s na LIVE): checagem leve do OBS (parada no OBS → encerra em ~1 s); a cada 5 s checagem completa + validação do destino + ping status 2 (código 0 ou 4003150 aceitos).
≥ 2 falhas e ≥ 8 s de erro → encerra. O prazo programado (`endAt`) é honrado a ≤ 200 ms. O monitor acorda por canal quando a fase ou o prazo mudam.

**Guard/IM**: `im/fetch` a cada 1–10 s (intervalo vindo do servidor). `ControlMessage` com ação 3/4 → `remote_end` (sempre encerra); `NoticeMessage` com conteúdo → `warning` (encerra se
"encerrar por aviso" está ligado, padrão sim). Os mesmos lotes alimentam o feed (chat, entradas, presentes, seguidores, compartilhamentos, clube de fãs) — somente exibição.

## 5. API HTTP do painel (`127.0.0.1:8765`)

Leitura (sem token, só loopback): `GET /` (painel; injeta o token), `/app.js`, `/style.css`, `/live` (+ `/live.js`, `/live.css`), `/avatar?u=<url de CDN>`, `/api/state` (snapshot JSON).
Escrita (POST, exige `Host` local, `Origin` igual e `X-Panel-Token`; corpo JSON ≤ 8 KB; resposta 200 com o snapshot, 409 com `{"error": "..."}`, 504 se > 90 s — a operação continua, **não repita**):

| Rota | Corpo | Efeito |
| --- | --- | --- |
| `/api/connect` | `{}` | Conecta OBS e Chrome, lê a conta (somente leitura) |
| `/api/start` | `{title, scene, confirmed, maxMinutes?, productDetails?}` | **Publica** |
| `/api/end` | `{}` | Encerra e verifica |
| `/api/schedule` | `{minutes}` (número ou `null`) | Muda o prazo da LIVE atual |
| `/api/auto-end` | `{enabled}` | Encerrar por aviso do TikTok |
| `/api/about` | `{text}` | Altera o "Sobre mim" da conta |
| `/api/chat` | `{text, replyToN?, via?, context?}` | Envia mensagem (`via` = `thanks`/`answer` exige autopilot) |
| `/api/autopilot` | `{thanks?, answers?}` | Permissão por LIVE (só ligável durante a LIVE) |

Snapshot (`/api/state`): `phase`, `detail`, `obs{profile,scene,scenes,active,reconnecting}`, `tiktok{code,liveStatus,enabled,banned,...}`, `hasRoom`, `canStart`, `canEnd`,
`warningMonitor`, `autoEndOnWarning`, `autopilot{thanks,answers,sent}`, `endAt`, `startedAt`, `remainingSec`, `liveTitle`, `productDetails`, `aboutMe`, `view{viewers,total,stats,intro,feed[],counts}`, `monitor`.

## 6. Dados em disco (`<root>\runtime\`)

| Arquivo | Conteúdo |
| --- | --- |
| `broadcast.json` | **Diário**: `phase`, `binding{room_id,ping_room_id,stream_id}`, `profile` do OBS, `key_cleanup_pending`, `end_at`, `format`. Gravado de forma atômica; é o que impede uma segunda criação após uma queda |
| `heartbeat.json` | `{t, pid, phase}` a cada 2 s (alimenta o watchdog) |
| `watchdog.json` / `.last.json` / `watchdog.pid` | Resultado da ação do watchdog / histórico / PID |
| `browser.json` | Perfil, porta e caminho de depuração do Chrome; opções `visibility` (`hidden`/`minimized`) e `mode` (`headless`, opt-in) |
| `autopilot-log.jsonl` | Auditoria de mensagens automáticas (texto enviado, a que respondeu, falhas) |
| `mcp-policy.json` | Política opcional do MCP |
| `traces/<data>-live/` | `events.jsonl` (mascarado), `blobs/` (respostas IM brutas, frames WS), `summary.md` |
| `camera/` | `pairing.json` (id e segredo do celular pareado), `cert.pem` e `key.pem` do HTTPS local |
| `panel-url.txt`, `remote-assist-link.txt` | Endereço do painel; link temporário do assistente (apagado ao fim) |

O perfil do Chrome fica **fora** do diretório do plugin: `%LOCALAPPDATA%\TikTokLiveOBS\chrome-profile`. O plugin guarda layout e opções em
`%APPDATA%\obs-studio\plugin_config\livett\config.json`.

## 7. O plugin nativo (`plugin/livett`)

C++/Qt6 contra o OBS 32.2.2 (`obs-module`, `obs-frontend-api`, `obs_browser_init_panel`). Faz: (1) inicia `data/controller/controller.exe serve --background`
; (2) cria o dock **LiveTT** (`http://127.0.0.1:8765/`, com a visão ao vivo de `/live` embutida)
em navegadores embutidos (QCef); (3) a janela flutuante **nativa** (Ferramentas): fundo translúcido por pixel com texto opaco, três colunas × duas linhas de estatísticas,
chat com fotos pelo `/avatar`, posição/opacidade/"sempre no topo" lembrados. O plugin não tem lógica de negócio.

### 7.1 A janela de início e o botão "Iniciar transmissão" do OBS

Com o plugin carregado, o botão **Iniciar transmissão** do OBS inicia uma LIVE do TikTok (sem nenhuma opção a ligar; desativar o plugin devolve o OBS ao normal).
O plugin registra um serviço próprio, `tiktok_live` (`src/obs-service.cpp`), e um filtro de clique no botão:

```
clique no botão (ou "Iniciar LIVE" do painel, via API do plugin: OpenStartDialog)
        ▼
janela "Iniciar LIVE no TikTok": conta ativa (com foto), avisos (sem conta / login / OBS), título, duração, "Abrir painel"
        │ Cancelar = nada acontece (sem caixa de erro do OBS)           │ Iniciar LIVE
        ▼
o plugin põe o serviço "tiktok_live" na MEMÓRIA do OBS (obs_frontend_set_streaming_service, sem salvar) e aperta o botão de novo
        ▼
OBS: obs_output_start ─> service.initialize ─> POST /api/start-native {confirmed:true} ─> controlador cria a sala, ping 1, DEVOLVE {server,key}
        │ (a chave só vive na memória do plugin: get_connect_info)
        ▼
OBS conecta ao destino por conta própria ─> o controlador espera "outputActive" e conclui (ping 2, vigias, watchdog)
        falhou ao iniciar ─> o controlador encerra a sala (mesmo retrocesso do início pelo painel)
quando a transmissão para (OBS_FRONTEND_EVENT_STREAMING_STOPPED): o plugin esquece o destino e devolve ao perfil o serviço dele
```

- **O perfil nunca é tocado:** o OBS só grava o serviço (`service.json`) pelas Configurações ou por `obs_frontend_save_streaming_service`, que o plugin não chama
  neste fluxo. A troca é só em memória e dura o início/transmissão; o `service.json` do perfil fica byte a byte igual (verificado em
  `plugin/tools/obs-button-test`). Um perfil que uma versão anterior deixou com o serviço do plugin é restaurado na conexão (`releaseLegacyService`).
- **Chave nunca em disco:** servidor e chave ficam na memória do plugin; por isso, neste modo, o controlador não tem chave para apagar do OBS.
- **Confirmação explícita:** `confirmed:true` só é enviado depois do clique em "Iniciar LIVE" na janela do plugin (nenhum assistente MCP alcança esse caminho).
- **Atalhos e Stream Deck** chamam o OBS sem passar pelo clique do botão: iniciam o serviço que o perfil tiver (o do usuário). Use o botão ou o painel.
- **Controle da parada:** `obs.StreamOutput{Native:true}` valida só o perfil (o serviço volta ao do usuário assim que a transmissão para); o watchdog também
  reconhece o serviço `tiktok_live` enquanto a LIVE roda.
- **Limite do OBS:** a lista "Serviço" das Configurações vem do `services.json` do plugin `rtmp-services` (atualizado online) e não aceita serviços de plugins.
- **Contexto para IA:** fica no painel (seção Assistente de IA), é salvo pelo controlador em `runtime\context.txt` e usado por toda LIVE que começa (pelo painel, pelo botão ou pelo MCP);
  durante a LIVE "Atualizar na LIVE" o coloca no ar. A janela de início não tem esse campo.
- **Um painel só:** o dock "LiveTT" mostra o estado, o acompanhamento ao vivo (estatísticas e chat, embutidos de `/live`) e as configurações extras (contas,
  assistente de IA, Sobre mim, segurança, conexão). O formulário de início só existe na janela (sem redundância).
- **Testes:** `controller/internal/controller/native_test.go`, `obsbutton_test.go`; no OBS real, `plugin/tools/obs-button-test`.
### 7.2 A câmera do celular (`internal/camera` + `plugin/.../phone-camera.cpp`)

O celular manda vídeo e áudio ao OBS por **WebRTC entre dois navegadores** (a página do celular e uma Browser Source do OBS); o controlador só leva as mensagens de
conexão (oferta, resposta e candidatos ICE) por *long polling* HTTP. Não há WebRTC em Go.

```
celular (phone.html/js) --HTTPS--> [túnel Cloudflare | rede local :8766-8775] --> Hub (caixa de mensagens) <--HTTP 127.0.0.1:8765-- receptor (rx.html/js) numa Browser Source
        \_________________ vídeo + áudio (WebRTC, direto, sem STUN/TURN) _________________/
```

- **Modos (nesta ordem):** **relay** (`worker/`: um Cloudflare Worker publicado pelo projeto, `camera.DefaultRelay` ou `TIKTOK_CAMERA_RELAY`; nada é aberto neste computador; celular e receptor falam por WebSocket com uma sala `sha256(rx_secret)`, ver `worker/README.md`), **túnel** (`cloudflared`, abaixo) e **rede local** (certificado autoassinado). O estado traz `mode` = `relay`/`tunnel`/`lan`. No relay o receptor (`rx.js`) lê `/camera/rx/config` do controlador (chave, sala, hash do código do celular) e repassa o nome do aparelho e o estado ao controlador, que continua sendo a fonte do que o painel mostra.
- **Por que HTTPS confiável (túnel ou relay):** o celular só libera a câmera numa página HTTPS cujo certificado ele confie; um certificado criado neste computador nunca é confiável (o iPhone recusa a câmera mesmo depois do aviso). Com o `cloudflared` (achado no PATH ou nas pastas do winget) o controlador abre a porta só no `127.0.0.1` e um *quick tunnel* (`tunnel.go`) devolve o `https://…trycloudflare.com`; um *job object* do Windows mata o processo se o controlador cair. Sem ele, cai no modo local (`cert.go`, certificado autoassinado). O estado traz `mode` (`tunnel`/`lan`) e `needsTunnel` (para o painel orientar).
- `Hub` guarda uma **lista de câmeras** (`runtime/camera/cameras.json`; um `pairing.json` antigo vira a "Câmera 1"): cada uma com `id` (o receptor), `secret` (o celular), `rx_secret` (a sala no relay é o hash dele), nome e se já foi tomada por uma fonte; as mensagens, o estado, o nome do aparelho e o tamanho da imagem são **por câmera** (duas câmeras nunca dividem sala, código ou fila). A porta/túnel é uma só, compartilhada. Cada câmera tem duas filas, uma por destino (`rx` e `phone`), com `seq` crescente; `Recv` espera até 25 s. Mensagens: `hello` (reinicia a negociação e traz o nome do aparelho), `offer`, `answer`, `ice`, `bye`, `rxhello` (o receptor recarregou: o celular negocia de novo) e `state` (o receptor diz `connected`/`idle`; só atualiza o estado). O celular oferece, o receptor responde. O receptor conta como presente enquanto espera uma mensagem.
- Duas portas para o mesmo hub: a do **celular** (`lan.go`: só endereços privados/loopback, segredo em todo pedido, limite de erros) e a do **receptor** (rotas `/camera/rx*`, `/camera/qr.svg` e `/camera/qr.txt` no servidor do painel, loopback; o POST do receptor exige a mesma origem e o `id`).
- `qr.go` é um gerador de QR code (byte, correção M, versões 1 a 10) sem dependências; os testes leem o código de volta e conferem o Reed-Solomon, e a decodificação foi conferida com o jsQR. Páginas em `internal/assets/camera/` (sem script ou estilo inline: a política de conteúdo bloqueia).
- **No OBS** (`phone-camera.cpp`): o plugin registra o tipo de fonte `tiktok_phone_camera` ("Câmera do celular (LiveTT)", menu Fontes → +). Por dentro ela guarda uma Browser Source privada que carrega o receptor; desenha-a (`OBS_SOURCE_CUSTOM_DRAW`: o navegador usa o próprio efeito; sem isso a imagem sai vazia), repassa o áudio dela (`obs_source_add_audio_capture_callback` → `obs_source_output_audio`, mixers do filho em 0) e liga/desliga o filho com `show`/`activate`. Ao ser criada pela primeira vez abre a janela do QR (`pairing_shown` nas configurações da fonte; o QR vem de `/camera/qr.txt`, e fechar a janela sem celular conectado chama `/api/camera/stop`). Não duplica (um celular, um receptor).
- **Uma câmera por fonte:** a fonte do OBS guarda o `camera_id` nas configurações e, ao nascer, pede `/api/camera/claim`: recebe a câmera que lembra, ou (fonte antiga, sem id) a câmera que ninguém tomou, ou uma nova. Uma câmera "reservada" por 30 s não é entregue a outra fonte que esteja nascendo, e uma com receptor vivo não é removida. Sem isso, duas fontes dividiam a mesma sala e se derrubavam em ciclo (o relay só aceita um receptor e um celular por sala).
- O controlador (`controller/camera.go`) abre/fecha o pareamento de cada câmera, troca o segredo, cria a câmera + fonte (`CreateInput` do plugin, com `camera_id`; nomes "Câmera do celular", "… 2", …); o `Snapshot` traz `camera` **sem segredo** (`hosts` são só os endereços).
- A página do celular negocia um bitrate máximo por qualidade e aceita troca de câmera, qualidade ou quadros sem reiniciar (`replaceTrack`). Não há opção de posição do celular nem de lado da câmera: ela pede **sempre o mesmo tamanho** (horizontal): o navegador gira a imagem sozinho conforme o celular gira (pedir "em pé" faz o iPhone entregar o vídeo na posição natural do sensor, horizontal; foi um defeito corrigido) e mostra na página o tamanho que está sendo enviado, e o **receptor** mede o vídeo que chega (`resize` do `<video>`) e o informa ao controlador (`size`); o plugin consulta `/camera/size` a cada ~1,5 s e dá à fonte e ao navegador interno esse tamanho (horizontal ou vertical).

## 8. Decisões de projeto que valem lembrar

- **Go sem dependências externas**: WebSocket, protobuf mínimo e HTTP feitos à mão ou pela biblioteca padrão; um único `.exe` (~7,6 MB), sem interpretador.
- **Janela do Chrome**: escondida com `SW_HIDE` (um Chrome totalmente escondido para de desenhar: capturas de tela travam; fora da tela + estilo tool-window desenha).
  Hoje só o login a traz à frente, e ela some ao logar.
- **`panel_browser.js` é versionado pelo nome do objeto** (`__tiktokPanelV9`): ao mudar o script, suba o número, porque a aba mantém o objeto antigo; um teste garante que script e Go usam o mesmo nome.
- **Conectar nunca publica.** Publicação só por `Start`, com `Confirmed`.
- **Identidade honesta**: parâmetros do app 1.38.0 e `x-ss-stub` (MD5 do corpo), sem `device_id`/`install_id`, sem assinaturas.
