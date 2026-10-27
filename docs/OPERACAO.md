# Operação: instalar, entrar, transmitir, recuperar

Guia para o usuário e para quem o ajuda (inclusive uma LLM). Português do Brasil.

> **Leia o [aviso de risco](RISCOS.md) antes de usar:** a ferramenta é não oficial e pode violar os Termos do TikTok.

## 1. Requisitos

- Windows 10/11 x64; **OBS Studio 32.x** (testado com 32.2.2); **Google Chrome**.
- Nada para configurar no OBS: o plugin conversa com o OBS por dentro (API de controle local, token por execução). **Não usa** o servidor WebSocket do OBS.
  arquivo de configuração do OBS; você não a digita em lugar nenhum.
- Use um **perfil do OBS sem conta do YouTube vinculada**: com conta vinculada o OBS abre um diálogo do YouTube ao iniciar a saída e a transmissão não começa
  (nunca clique "Criar e iniciar transmissão" nesse diálogo: ela iria para o YouTube).

## 2. Instalar / atualizar / remover

1. **Feche o OBS.**
2. Extraia a pasta `livett` do zip em **`C:\ProgramData\obs-studio\plugins\`** (o OBS 32 só lê plugins dessa pasta; `%APPDATA%\obs-studio\plugins` não é lida).
   Resultado: `...\livett\bin\64bit\livett.dll` e `...\livett\data\controller\controller.exe`.
3. Abra o OBS. O plugin inicia o controlador sozinho e cria o painel **LiveTT** (menu Painéis); **Ferramentas → LiveTT:
   janela flutuante Ao vivo** abre a janela nativa translúcida com estatísticas e chat.
4. **Atualizar**: feche o OBS (de forma normal) e extraia por cima; os dados em `data\controller\runtime` ficam. **Remover**: apague a pasta do plugin
   (e, se quiser, `%LOCALAPPDATA%\TikTokLiveOBS`, o perfil do Chrome).
5. Se o OBS mostrar "Falha detectada no OBS Studio" depois de um encerramento forçado, feche-o e apague a pasta `%APPDATA%\obs-studio\.sentinel`.

Nunca troque o plugin com uma LIVE no ar.

## 3. Entrar no TikTok (uma vez; depois a sessão fica)

1. Abra o painel de controle: ele **conecta sozinho** ao OBS e ao TikTok (o botão Conectar só aparece se falhar).
2. Se aparecer **"Entrar no TikTok"**, abre uma janela pequena (520×660) com a página oficial de login, sem o feed (perfil próprio do Chrome em `%LOCALAPPDATA%\TikTokLiveOBS\chrome-profile`). Entre por QR,
   e-mail ou telefone, **você mesmo**. O TikTok pode pedir "Verifique se é realmente você" (código por e-mail/telefone ou senha) porque o Chrome é um aparelho novo;
   se disser "Número máximo de tentativas atingido", espere e tente depois.
3. Quando a conta entrar, o plugin fecha a janela no ato (antes de o feed tocar vídeo), troca o ticket PKCE por uma sessão do serviço de LIVE (ou copia o cookie, se o TikTok recusar) e passa a **Pronto**. A conta entra na lista **Contas** (menu do avatar).
   Não é preciso clicar em "Conectar" de novo.
4. **Várias contas**: o menu do avatar (canto superior do painel) lista as contas salvas; **Usar** troca sem novo login, **Adicionar conta** abre o login para outra, **Sair** apaga a sessão salva. Só fora de uma LIVE. Nome, @ e foto vêm do perfil do serviço de LIVE (como no Studio). O registro `runtime\accounts\events.log` lista, sem segredos, o que cada operação de conta fez. As sessões ficam em `runtime\accounts`, criptografadas com a chave do seu usuário do Windows (DPAPI).
5. Estando longe do PC, existe o **assistente de login remoto** (seção 7), só com sua autorização.

Se, mais tarde, o painel voltar a "Entrar no TikTok", a sessão expirou: repita.

## 4. Fazer uma LIVE

1. Escolha a **cena** no OBS e confira prévia e áudio. Escreva o **título** (até 80 caracteres) e, se quiser, uma **duração máxima** (1 a 720 min).
2. Marque a confirmação ("Estou pronto para publicar...") e clique **Criar e iniciar LIVE**. Isso **publica de verdade**. Em poucos segundos o painel mostra
   "Transmitindo", a vigilância de avisos fica ativa e o painel "Ao vivo" passa a mostrar espectadores, curtidas, presentes e o chat.
3. Durante a LIVE você pode trocar a cena no OBS, mudar a duração programada, ligar/desligar "encerrar se o TikTok enviar um aviso" e (se quiser que o assistente
   cuide do chat) autorizar o agradecimento/resposta automáticos **só para esta LIVE**.
4. **Encerrar LIVE** para o OBS e verifica no TikTok que a sala terminou. O estado final correto é **"Encerrada"**. Se aparecer "Verificação pendente",
   o término remoto não foi confirmado: confira a **Central de LIVE** do TikTok e clique Encerrar de novo. Não inicie outra LIVE antes.
5. Se você parar a transmissão **dentro do OBS**, o painel encerra a sala em cerca de 1 s. Se o TikTok sinalizar término/aviso, também encerra sozinho.

### 4.1 A janela de início

Clique em **Iniciar transmissão** no OBS (ou em **Iniciar LIVE** no painel): abre a janela de início. Nela:

- a **conta** que vai transmitir (e, se não há conta ou a sessão expirou, um aviso com **Entrar no TikTok**; "Abrir painel" leva às contas);
- **Título** e **Duração**. O **Contexto para IA** (produtos, gameplay, regras: o assistente usa isto nas respostas) fica no painel, em Assistente de IA: é salvo e vai com toda LIVE; durante a LIVE, "Atualizar na LIVE" o muda no ar;
- **Iniciar LIVE** cria a LIVE e o OBS conecta sozinho; **Cancelar** não faz nada.

Para encerrar use **Parar transmissão** no OBS ou **Encerrar LIVE** no painel; ambos encerram a sala. Nada para ligar: com o plugin ativo o botão do OBS já faz isto; para
transmitir para outra plataforma, desative o plugin (seu serviço de transmissão no OBS não é alterado). Teclas de atalho/Stream Deck iniciam o serviço do OBS, não o do TikTok.
## 4.2 Usar o celular como câmera

Sem instalar nada no celular. Celular e computador na **mesma rede Wi-Fi** (sem "isolamento de clientes" no roteador); o computador também precisa de internet se usar o túnel (abaixo).

**Nada para instalar quando o projeto publica o relay** (um Worker da Cloudflare, ver `worker/README.md`): o celular abre uma página HTTPS com certificado válido (sem aviso; o iPhone só libera a câmera assim) e o computador não abre nenhuma porta. Enquanto o relay não está publicado (ou se estiver sem internet), o plugin tenta o **cloudflared** (`winget install Cloudflare.cloudflared`, que dá o mesmo HTTPS válido por um túnel gratuito) e, sem ele, usa a rede local com aviso de certificado (o iPhone costuma recusar a câmera nesse modo).

1. No OBS: **Fontes → + → Câmera do celular (LiveTT)**. Uma janela **Parear o celular** abre sozinha com o **QR code** (também em *Propriedades da fonte → Parear celular* e no painel, em **Câmera do celular**).
2. Aponte a câmera do celular para o código e toque em **Iniciar câmera** na página que abrir. Escolha câmera (a lista inclui a frontal e a traseira), microfone, qualidade (480p/720p/1080p), quadros (24/30/60) e, se o aparelho permitir, o zoom.
3. A janela mostra "Celular conectado" e fecha. A imagem e o **som** aparecem na fonte (o áudio entra no mixer do OBS). A fonte **toma sozinha o formato da imagem que chega** (horizontal ou vertical, conforme você segura o celular; ao virar o celular ela acompanha), sem nenhuma opção para isso.
4. O painel também tem **Adicionar na cena** (cria a mesma fonte na cena atual). Faça isso **antes da LIVE**: durante a LIVE o painel recusa, porque a fonte nova apareceria ao público.
5. **Novo código** desconecta o celular atual e invalida o QR antigo; **Parar de aceitar** fecha a conexão. Fechar a janela do QR sem ter conectado um celular também fecha a conexão.

**Vários celulares:** cada fonte "Câmera do celular" tem a **sua própria câmera** (sala, código e QR próprios; até 8). Adicione outra fonte (Fontes → + de novo, ou **Adicionar câmera na cena** no painel) e leia o QR dela com o segundo celular: um não derruba o outro. A lista no painel mostra cada câmera ("Câmera 1", "Câmera 2"…), se está conectada e de qual aparelho, e tem **Parear / Mostrar código**, **Novo código**, **Parar** e **Remover** (só para câmera sem fonte ativa). Cada fonte guarda a sua câmera (`camera_id`) na coleção de cenas; copiar a cena para outro computador não leva o pareamento, e a fonte pede uma câmera nova lá.

Notas: deixe a página do celular aberta e em primeiro plano (a tela fica acesa sozinha); se o OBS reabrir, a imagem volta sozinha, sem novo QR (a fonte lembra o pareamento). No modo túnel o endereço do QR muda a cada vez que a conexão é aberta; no relay o QR não muda (o celular pareado volta sozinho) até você pedir **Novo código**.
Com mais de um endereço de rede (modo local), escolha no painel o que o celular alcança. Para testar sem celular, abra o endereço do QR com `&fake=1` no fim (imagem e tom de teste).
Diagnóstico: `TIKTOK_CAMERA_BIND=127.0.0.1` mantém a porta só no computador (e sem túnel) e `TIKTOK_CAMERA_PLAIN=1` (só com isso) a serve sem HTTPS, para testar de um navegador local.

## 5. Estados e o que fazer

| Estado | Significa | Ação |
| --- | --- | --- |
| Desconectado | Ainda não conectou | Conectar e verificar |
| Entrar no TikTok | Sessão inválida (código 20003) | Entrar na janela do Chrome |
| Pronto | Pode iniciar | — |
| Criando sala | Criação em andamento | **Não repita o pedido** |
| Transmitindo | No ar | Encerrar quando quiser |
| Encerrando | Parando e verificando | Aguardar |
| Encerrada | Tudo confirmado | Pode iniciar outra |
| Verificação pendente | OBS parado mas o término remoto não foi confirmado, ou criação duvidosa | Conferir a Central de LIVE; tentar Encerrar; **não criar outra** |
| Recuperação pendente | O programa caiu no meio de uma LIVE | Conectar e verificar; encerrar a sala anterior |

## 6. Problemas comuns

| Sintoma | Causa provável / solução |
| --- | --- |
| Painéis do TikTok não aparecem | Plugin na pasta errada (use `C:\ProgramData\obs-studio\plugins`); veja o log do OBS por `[livett]`. Painéis → marque os dois painéis. |
| "Não consegui falar com o OBS" | Abra o OBS 32.x com o plugin carregado (log do OBS: `[livett] control API listening`). Em desenvolvimento, `go run ./cmd/obscheck` (em `controller`) mostra o que a API responde. |
| `20003` / "Entrar no TikTok" | Sessão do TikTok ausente ou expirada: entre na janela do Chrome. |
| A saída do OBS não inicia | Diálogo do YouTube (perfil com conta vinculada) ou outro diálogo aberto no OBS: feche e use um perfil sem YouTube. |
| Painel "indisponível" | O controlador não está rodando: reabra o OBS (ele o inicia). Com uma LIVE no ar, **pare a transmissão no OBS** e confira a Central de LIVE. |
| Mensagem do anfitrião no chat "não chega" | Limitação conhecida (ver PROTOCOLO-TIKTOK §7). Os comentários dos espectadores chegam normalmente. |
| Aviso do SmartScreen/antivírus | Os binários não são assinados; é esperado nesta versão. |

Dados e registros: `...\livett\data\controller\runtime\` — `broadcast.json` (diário), `traces\<data>-live\` (`events.jsonl`, `blobs\`, `summary.md`),
`autopilot-log.jsonl`, `heartbeat.json`, `watchdog.json`. Os traces têm segredos mascarados, mas contêm nomes de espectadores: não os compartilhe.

## 7. Assistente de login remoto (só com autorização expressa)

Para quando o usuário **não alcança o PC** e precisa concluir o login do TikTok (a verificação exige digitar um código no Chrome do PC).

```powershell
controller.exe assist --root <pasta data\controller> [--cloudflared <caminho do cloudflared.exe>] [--minutes 15]
```

Imprime `READY <link>`. Sem `--cloudflared` serve só em `127.0.0.1:8790`; com `cloudflared` cria um túnel temporário público (sem conta) e o link `https://...trycloudflare.com/<segredo>/`
funciona no celular: mostra a aba de login e deixa tocar e digitar (o texto vai direto ao Chrome e não é registrado). Apague o `cloudflared` depois. O link vale `--minutes` e se
encerra no botão "Encerrar". Prefira **QR + código de e-mail**; se usar senha, ela passa pela Cloudflare e convém trocá-la depois. Obtenha o consentimento do usuário a cada uso e baixe o
`cloudflared` só do repositório oficial da Cloudflare, conferindo a assinatura digital.

## 8. Diagnóstico sem publicar

`controller.exe version` · `go run ./cmd/transportcheck <root>` (lê versão/cena do OBS e roda o `check` do TikTok; nunca cria sala).
