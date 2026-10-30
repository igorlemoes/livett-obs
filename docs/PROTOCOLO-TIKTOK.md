# Como o controlador fala com o TikTok

Resumo do que o controlador faz, verificado em testes reais (22–27/10/2026). **Importante:** são serviços internos e não documentados do TikTok, usados a partir da sessão
do próprio usuário; leia o [aviso de risco](RISCOS.md) antes. Os detalhes (parâmetros, campos, formato das mensagens) estão no código, que é a fonte da verdade:
`controller/internal/assets/panel_browser.js` (chamadas), `controller/internal/room` (criação e encerramento), `controller/internal/impoll` e `controller/internal/wire`
(mensagens da sala). Este documento não lista identificadores de aplicativo nem formatos internos; o que não foi verificado está marcado como **incerto**.
Nada aqui autoriza falsificar assinaturas, identificadores de dispositivo ou arquivos do aplicativo oficial (ver [SEGURANCA.md](SEGURANCA.md)).

## 1. Como as chamadas são feitas

Todas saem **de dentro do Chrome gerenciado**, de uma aba no domínio do serviço de LIVE, com os cookies da sessão do usuário (`credentials: 'include'`); o controlador nunca
exporta nem registra cookies. O script `panel_browser.js` define a ponte (`window.__tiktokPanelV11`) e envia o corpo
assinado como o serviço espera (um resumo MD5 do corpo). Respostas: JSON com `status_code` (0 = ok); a área de dados pode trazer `prompts` com o texto do erro.

Códigos vistos: `0` ok · `20003` sessão inválida ou ausente · `4014257` "precisa atualizar" · `4003150` tolerado no ping de manutenção · `4003` recusa de criação.

## 2. Sessão (login)

O login web (QR, e-mail, telefone) é feito pelo usuário numa janela do próprio TikTok e cria a sessão em `.tiktok.com`. O serviço de LIVE só reconhece a sessão se existir
uma cópia em `.tiktokv.com` (`session.SyncWebcastSession`: leitura e escrita de cookies pelo Chrome DevTools, dentro do perfil do usuário, sem nunca registrar o valor), ou se o
login for trocado por um ticket (`session/account.go`, operação `pkce`). Sem isso: `20003`. Em um perfil novo o TikTok pode pedir verificação (telefone, e-mail ou senha) e limitar
tentativas; isso é sempre do usuário.

## 3. Operações da ponte

| Operação | Uso |
| --- | --- |
| `check` | Pré-requisitos da conta: pode criar sala, não está em LIVE, sem banimento; devolve padrões da conta |
| `create` | Cria a sala (título, opções fixas e os padrões da conta) |
| `ping` | `status=1` preparação (antes de configurar o OBS), `2` manutenção a cada ~5 s, `4` encerrar |
| `verify` | Lê o estado da sala: `status == 4` = encerrada (a confirmação que o controlador exige) |
| `probe`, `roomstats` | Estado da sala e contagens (espectadores, curtidas...) para o painel e a telemetria |
| `imfetch` | Canal de mensagens da sala (formato binário), ~2 s |
| `chat` | Enviar uma mensagem (JSON, até 200 caracteres) |
| `aboutme` / `aboutmesave` | Ler e alterar o "Sobre mim" da conta |
| `profile` | Nome, @ e foto da conta, como o serviço de LIVE os mostra |
| `drop` | Local: esquece o vínculo com a sala já encerrada |

### Criação
A resposta traz o identificador da sala, o identificador da sala **de ping** (diferente), o do fluxo e as URLs de envio; usamos o destino `rtmp` (servidor = tudo até a última `/`, chave =
o resto, com a query). Identificadores são sempre **strings** (números grandes perdem precisão em JavaScript). **A URL de envio é um segredo**: nunca é registrada nem exibida.
A sala sai marcada `live_type_third_party: true` (verdadeiro e mantido). O tópico da LIVE (`hashtag_id`) vai como `0`; um parâmetro de tópico **não existe ainda**.

## 4. Mensagens da sala

O controlador lê o canal de mensagens da sala e decodifica um subconjunto: chat, entradas, espectadores, curtidas, presentes, seguidores e compartilhamentos, entrada no clube de
fãs e a introdução da sala. Os sinais de encerramento (fim remoto e avisos) só valem com o identificador da sala igual ao da LIVE e mensagem não histórica. **Texto de espectador é não
confiável**: só é exibido como texto. O `push_server` (WebSocket) que o canal oferece **não é usado** (incerto como se conectar). Categorias de mensagem não mapeadas
(possíveis avisos de restrição) são ignoradas: **incerto**.

## 5. Encerramento

Parar a saída do OBS **e** `ping status=4` e confirmar `status == 4` na sala. As duas pontas rodam em paralelo; falhar numa não impede a outra. O estado só vira `closed` com as duas confirmadas.

## 6. OBS e áudio

O plugin conversa com o controlador por uma API local própria (sem obs-websocket). Durante o início, o botão "Iniciar transmissão" do OBS usa um serviço próprio em memória (o `service.json` do perfil não é
alterado). O caminho do painel/MCP usa `rtmp_custom` com o destino recebido e restaura o original no fim. Perfil do OBS **sem conta do YouTube** (senão o `StartStream` abre o diálogo do YouTube).
Medido numa LIVE real: ~4,4–5,8 Mbps, 30–60 fps, CPU do OBS ~2%, 0 quadros perdidos.

## 7. Entrega das mensagens do anfitrião (problema aberto)

- O envio de mensagem responde sucesso, devolve um identificador novo e o autor correto (a conta anfitriã).
- Em tentativas reais o usuário, assistindo por outro app/conta, **não viu** a mensagem.
- Os **comentários dos espectadores chegam** ao feed normalmente; a ausência de eco da própria mensagem no feed é esperada.
- **Hipóteses (incertas)**: o aplicativo/conta que assiste não mostra mensagens do anfitrião, ou mostra com atraso; filtro ou restrição do TikTok na conta. É preciso um espectador de
  **outra conta/aparelho** para separar as causas.

## 8. O que ainda é incerto

Socket de push das mensagens · categorias de mensagem não mapeadas (restrições) · envio de mídia independente do OBS ·
suporte a outras versões do OBS · **como o TikTok classifica, no longo prazo, uma conta que transmite assim** (ver [RISCOS.md](RISCOS.md)).
