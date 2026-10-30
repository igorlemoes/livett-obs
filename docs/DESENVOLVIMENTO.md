# Desenvolvimento

## 1. Requisitos

| Para | Precisa |
| --- | --- |
| Controlador Go | Go 1.24+ (nenhuma dependência externa; `controller/go.mod` não tem `require`) |
| Detector de corridas (`go test -race`) | gcc no PATH (ex.: MSYS2 `C:\msys64\ucrt64\bin`) |
| Plugin nativo | Visual Studio 2022 Build Tools, CMake, Ninja e `plugin\build-deps` (ver [../plugin/README.md](../plugin/README.md)) |
| Empacotar | PowerShell 5.1+ (tudo em `scripts\`) |
| Testes reais | OBS 32.2.2, Chrome, uma conta TikTok habilitada para LIVE e o usuário presente |

## 2. Comandos

```powershell
powershell -File scripts\test.ps1                 # go vet + go test -race -p 2 ./...  (sem gcc roda sem -race)
powershell -File scripts\build.ps1                # plugin + controlador + dist\LiveTT-OBS-plugin-<VERSION>.zip + SHA256SUMS.txt
powershell -File scripts\build.ps1 -SkipPlugin    # reaproveita a DLL já compilada
powershell -File scripts\build.ps1 -Test          # roda os testes antes
cd controller; go run ./cmd/controller version
cd controller; go run ./cmd/transportcheck <root> # diagnóstico só de leitura (OBS + Chrome); nunca cria sala
```

A versão sai do arquivo `VERSION` (injetada com `-ldflags "-X main.version=..."`). O executável é compilado com `-trimpath` (sem caminhos da máquina de build).
O `-p 2` nos testes existe porque o detector de corridas reserva muita memória por binário de teste e falha de forma intermitente com muitos em paralelo.

## 3. Como o código é testado

- **Fakes por interface**: `controller.Browser`, `controller.Obs`, `controller.Recorder`, `mcp.Panel`, `mcp.ObsAPI`, `assist.Login`, `impoll.Guard`... Os testes de `controller` simulam OBS e TikTok
  e cobrem os fluxos de risco: criação duvidosa → `recovery`, rejeição limpa → nova tentativa, chave do OBS restaurada/apagada, encerramento por sinal/aviso, parada no OBS em ~1 s,
  tolerância a falhas breves, agendamento, limites e kill switch do chat, autopilot por LIVE, observador de login.
- **CDP/OBS**: servidores falsos em `httptest` (a API do plugin: token, descoberta pelo arquivo, recusa com código, timeout). A API **real** se verifica com `go run ./cmd/obscheck` (só leitura) e `-write` (cria cena/fonte e troca o destino: **só numa cópia de teste do OBS**; use `TIKTOK_OBS_API_FILE` e `LOCALAPPDATA` próprios para não tocar no OBS real).
- **Segurança como teste**: segredos nunca chegam a trace/diário/snapshot; `PushTarget` não imprime a chave; entradas fora dos limites são recusadas; token errado = 404/403.
- O que **nenhum teste** prova: o comportamento do TikTok real. Só uma LIVE real (autorizada, curta) prova. Registre sempre o que foi visto de verdade.

## 4. Convenções

- Pacotes pequenos em `controller/internal/<nome>`; dependências por interface; sem variáveis globais mutáveis exceto pontos de teste explicitamente marcados (`startProcess`, `endpointOf`).
- Concorrência: `Controller.mu` serializa operações longas; `Controller.st` protege o que o snapshot lê e só é segurado por instantes. Nunca segure `st` durante I/O.
- Segredos: nunca em `fmt`/logs; qualquer dado que vá a um trace passa por `trace.Redact`. Textos de espectadores são tratados como dados.
- Mensagens ao usuário (erros do painel, MCP) em **português**; comentários de código em inglês curto ou português, mas explique o *porquê*.
- Escrita de arquivos de estado: temporário + rename (atômico).
- Commits em português, descrevendo o efeito; mudanças de comportamento acompanham teste e atualização dos docs.

## 5. Tarefas comuns

**Adicionar uma ferramenta MCP**: implemente o método em `internal/mcp/tools.go`, registre em `toolTable` (`server.go`, com `schema` e `readOnly`), adicione testes em `mcp_test.go`,
documente em [MCP.md](MCP.md). Ferramentas que publicam, enviam mensagens públicas ou alteram dados permanentes **precisam** de um argumento de confirmação explícito.

**Adicionar um campo ao painel**: `controller.snapshotLocked` → `web` (`internal/assets/web/*`); o painel é embutido, então basta recompilar.

**Mudar o script do navegador** (`internal/assets/panel_browser.js`): **suba o número** do objeto (`__tiktokPanelV9` → `V10`) no script **e** em `session.BridgeName`, porque a aba do Chrome mantém o
objeto antigo; o teste `TestBridgeNameMatchesThePanelScript` falha se divergirem. Novas operações entram em `allowedOps` (`session.go`).

**Novo decodificador de mensagem IM**: `internal/wire` (limites de tamanho, erro em dado malformado) + tratamento em `controller/view.go` (`ingestLocked`); teste com bytes fixos.
Nunca classifique como sinal de encerramento sem evidência de LIVE real.

**Analisar uma LIVE**: `<root>\runtime\traces\<data>-live\summary.md` (linha do tempo do controlador, chamadas ao TikTok, estatísticas do OBS, mensagens IM, falhas). `events.jsonl` tem os detalhes;
`blobs\` tem respostas IM brutas (contêm nomes de espectadores: não compartilhe).

**Lançar uma versão**: atualizar `VERSION`; `scripts\test.ps1`; compilar o plugin se mudou; `scripts\build.ps1`; testar o zip na **cópia portátil** do OBS e, depois, uma LIVE curta autorizada; conferir
que `dist\SHA256SUMS.txt` bate; atualizar docs e fazer commit/etiqueta.

## 5.1 Limpeza de dados locais

`runtime\`, `work\` e `dist\` estão no `.gitignore`. `work\obs-local` é a cópia portátil do OBS usada nos testes do plugin (~500 MB); pode ser recriada.

## 6. Pendências e próximos passos (ordem sugerida)

0. **Câmera do celular** (docs/OPERACAO.md §4.2). Validado: hub, QR (leitor independente jsQR), rotas e controlador (testes); no **OBS 32.2.2 de teste** a fonte nova (`tiktok_phone_camera`) abre a janela do QR ao ser criada, volta sozinha ao reabrir o OBS, e recebe vídeo e som de uma página de celular em modo de teste (`&fake=1`), conferido por captura da fonte e pelo registro do áudio (48 kHz). **Falta:** (a) um **iPhone/Android de verdade** pelo túnel (o modo rede local com certificado autoassinado foi tentado num iPhone: aviso de certificado e câmera não liberada); (b) o **Worker publicado de verdade** (`worker/README.md`: `wrangler login` + `npm run deploy`, depois preencher `camera.DefaultRelay`); localmente ele foi testado com 8 testes de integração e ponta a ponta (controlador + Worker local + páginas), mas não numa conta Cloudflare; e o túnel com o `cloudflared` real (só com um programa de mentira nos testes); (c) iOS/Android: câmera traseira, zoom, tela acesa, perder o Wi-Fi; (d) rede com isolamento de clientes e firewall; (e) **dois celulares de verdade** ao mesmo tempo (testado com duas páginas simuladas: ficaram estáveis por 30 s, cada uma com imagem e som na sua fonte; antes disso duas fontes dividiam uma câmera e piscavam). Se o ICE não fechar em alguma rede (candidatos mDNS), um plano B é anunciar o IP do receptor pela sinalização.
1. **Sala presa / recuperação** (caso real: o painel parou, o watchdog parou o OBS e não confirmou o término, a sala ficou aberta no TikTok). Feito: o watchdog tenta encerrar e confirmar até 6 vezes; ao clicar em Conectar com uma sala nossa ainda aberta e o OBS parado, o painel encerra essa sala; o aviso de conta ao vivo ficou explícito. **Falta:** o serviço tem GET /webcast/room/continue/ (devolve a última sala da conta, com um código próprio quando a sala está ao vivo em outro aparelho) e os códigos 30003 (sala já encerrada) e 30003001 (encerrada por outro aparelho); adotar a sala devolvida quando o diário local se perdeu, sem nunca encerrar uma sala que não possamos provar que é nossa. Precisa de uma resposta real para conhecer o formato.
1. **Entrega do chat do anfitrião** (ver [PROTOCOLO-TIKTOK.md](PROTOCOLO-TIKTOK.md) §7): LIVE curta vista por outra conta/aparelho; se continuar invisível, investigar o socket de push e a classificação da conta.
2. **Testes reais pendentes com o controlador Go**: encerramento por aviso/sinal do TikTok, por agendamento, watchdog (encerrar o `controller.exe serve` durante a LIVE e ver o `watchdog` agir), kill switch do chat
   (3 recusas), troca de cena durante a LIVE, ciclo completo de login por sessão expirada.
3. **Tópico da LIVE** (`hashtag_id`): listar via `/webcast/room/hashtag/list/` e aceitar `topic` em `start_live` (escolhido sempre pelo usuário).
4. **Distribuição**: teste em Windows limpo (Windows Sandbox), assinatura digital dos binários, licença, página de releases, suporte a outras versões do OBS.
5. **Perception/Bottom/Recall** e o socket de push do IM (possível cobertura maior de avisos de restrição).
6. **MCP**: ferramentas para tópico e para agendar uma LIVE; `obs_screenshot` com anotação de região; modo de somente-leitura configurável na política.
7. **Segurança**: opção de exigir senha local do painel; revisar a exposição do `GET /api/state` (contém nomes de espectadores) se o painel for alcançado por túnel.
