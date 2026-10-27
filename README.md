# LiveTT

[![Licença: GPL-2.0-or-later](https://img.shields.io/badge/licen%C3%A7a-GPL--2.0--or--later-blue.svg)](LICENSE)
![Windows x64](https://img.shields.io/badge/Windows-x64-informational)
![OBS 32](https://img.shields.io/badge/OBS%20Studio-32.x-lightgrey)

> **English summary:** LiveTT is an unofficial OBS Studio plugin plus a local Go controller (and MCP server) that starts and operates a TikTok LIVE from OBS without TikTok LIVE Studio, with a wireless phone camera source. It is documented in Brazilian Portuguese. **It may violate TikTok's Terms of Service and can get your account restricted or banned; use at your own risk** (see [docs/RISCOS.md](docs/RISCOS.md)). Licensed GPL-2.0-or-later.

Transmita para o **TikTok LIVE** direto do **OBS Studio**, sem o TikTok LIVE Studio aberto, e deixe um assistente de IA operar a LIVE
por **MCP**. O **LiveTT** é um plugin do OBS mais um controlador local em **Go** (um único `controller.exe`, sem dependências).

- Cria a sala, inicia a saída do OBS e **encerra com confirmação** (o OBS é parado e o término é verificado no TikTok).
- **Encerra sozinho** se o TikTok sinalizar término ou aviso, se a duração programada acabar, se a transmissão for parada no OBS ou se o
  controle falhar por tempo demais. Um **watchdog** em processo separado encerra a LIVE mesmo se o controlador morrer.
- Lê o chat, presentes, seguidores e espectadores (painel "Ao vivo" e janela flutuante nativa no OBS).
- A LIVE começa pelo botão **Iniciar transmissão** do OBS (ou pelo painel): uma janela pede título, duração e a confirmação (ver docs/OPERACAO.md §4.1).
- **Servidor MCP** com 17 ferramentas (iniciar/encerrar com guardas, cena, chat, captura da cena, contexto da LIVE...).
- Login do TikTok feito pelo próprio usuário numa janelinha de login que fecha sozinha; várias contas salvas (criptografadas) com troca em um clique.
- **Câmera do celular sem fio**: o celular vira câmera do OBS só abrindo uma página (QR code na hora de adicionar a fonte "Câmera do celular", WebRTC pela sua rede Wi-Fi, sem app; HTTPS confiável pelo relay do projeto, um Worker da Cloudflare em `worker/`, ou pelo `cloudflared` se o relay não estiver publicado). Ver docs/OPERACAO.md §4.2.

> ## ⚠️ Aviso de risco — leia antes de usar
>
> Esta ferramenta é **independente e não oficial** e **pode violar os Termos de Serviço do TikTok**: ela usa serviços internos e não documentados do TikTok, a partir da sua
> sessão. Isso pode levar à **restrição, suspensão ou banimento da sua conta** e pode parar de funcionar
> a qualquer momento. **O risco é seu**; teste primeiro com uma conta que você aceite perder. Detalhes e como reduzir o risco: [docs/RISCOS.md](docs/RISCOS.md).
>
> Projeto independente, sem vínculo com o TikTok/ByteDance. Publicar uma LIVE é uma ação real na sua conta; use com responsabilidade e
> respeite os termos do TikTok. **Limitação conhecida:** mensagens enviadas ao chat pelo anfitrião são aceitas pelo TikTok, mas ainda não
> confirmamos que chegam aos espectadores (ver [docs/PROTOCOLO-TIKTOK.md](docs/PROTOCOLO-TIKTOK.md)).

## Instalar

Requisitos: Windows 10/11 x64, **OBS Studio 32.x** (testado com 32.2.2), **Google Chrome**. Nada para configurar no OBS (o plugin controla o OBS por uma API interna, sem WebSocket).

**Opção A — instalador (recomendado).** Baixe `LiveTT-Setup-<versão>.exe` na página de [Releases](https://github.com/igorlemoes/livett-obs/releases), feche o OBS e execute. Ele mostra o aviso de risco (você precisa aceitar), instala em
`C:\ProgramData\obs-studio\plugins\livett` e migra os dados de versões anteriores. Instalação silenciosa: `LiveTT-Setup-<versão>.exe /VERYSILENT /ACEITO=1` (o `/ACEITO=1` confirma que você leu o aviso).
Confira o hash com `SHA256SUMS.txt`. O instalador não é assinado digitalmente, então o Windows SmartScreen pode avisar ("Mais informações > Executar assim mesmo").

**Opção B — extrair o zip.** Com o OBS fechado, extraia a pasta `livett` de `LiveTT-OBS-plugin-<versão>.zip` em `C:\ProgramData\obs-studio\plugins\` (o OBS 32 só lê plugins dessa pasta), ou rode `Instalar.cmd` do zip.

Depois abra o OBS: surge o painel **LiveTT** (menu Painéis); o botão **Iniciar transmissão** do OBS passa a abrir a janela de início.
Detalhes, primeiro uso, atualização e solução de problemas: [docs/OPERACAO.md](docs/OPERACAO.md).

## Usar com uma IA (MCP)

```json
{ "mcpServers": { "livett": {
    "command": "C:\\ProgramData\\obs-studio\\plugins\\livett\\data\\controller\\controller.exe",
    "args": ["mcp", "--root", "C:\\ProgramData\\obs-studio\\plugins\\livett\\data\\controller"] } } }
```

Guia completo das ferramentas e das regras para o assistente: [docs/MCP.md](docs/MCP.md) e [docs/SEGURANCA.md](docs/SEGURANCA.md).

## Desenvolver

```powershell
powershell -File scripts\test.ps1          # testes do controlador Go (detector de corridas se houver gcc)
powershell -File scripts\build.ps1         # compila plugin + controlador e gera dist\*.zip + SHA256SUMS.txt
powershell -File scripts\build.ps1 -Instalador   # também gera dist\LiveTT-Setup-<versão>.exe (Inno Setup 6)
powershell -File scripts\build.ps1 -SkipPlugin   # reaproveita a DLL já compilada
```

Estrutura, decisões e próximos passos: [AGENTS.md](AGENTS.md), [docs/ARQUITETURA.md](docs/ARQUITETURA.md), [docs/DESENVOLVIMENTO.md](docs/DESENVOLVIMENTO.md).

## Documentação

| Arquivo | Conteúdo |
| --- | --- |
| [AGENTS.md](AGENTS.md) | Ponto de entrada para LLMs: regras e mapa |
| [docs/MCP.md](docs/MCP.md) | Configuração, as 17 ferramentas, fluxos e regras para o assistente |
| [docs/RISCOS.md](docs/RISCOS.md) | **Aviso de risco**: Termos do TikTok, o que pode acontecer com a conta, como reduzir |
| [docs/SEGURANCA.md](docs/SEGURANCA.md) | Modelo de ameaças e regras invioláveis |
| [docs/OPERACAO.md](docs/OPERACAO.md) | Instalar, entrar no TikTok, fazer uma LIVE, recuperar de falhas |
| [docs/ARQUITETURA.md](docs/ARQUITETURA.md) | Componentes, estados, API HTTP, arquivos de dados |
| [docs/PROTOCOLO-TIKTOK.md](docs/PROTOCOLO-TIKTOK.md) | Endpoints, campos, sessão, mensagens IM, o que ainda é incerto |
| [docs/DESENVOLVIMENTO.md](docs/DESENVOLVIMENTO.md) | Build, testes, como estender, pendências |

## Licença

Copyright © 2026 Igor Lemões. Este programa é software livre, licenciado sob a **GNU General Public License, versão 2 ou (a seu critério) qualquer versão
posterior** (GPL-2.0-or-later); o texto completo está em [LICENSE](LICENSE). Ele é distribuído **sem garantia alguma**. O OBS Studio, ao qual o plugin se liga, também
é GPL-2.0-or-later. Contribuições são aceitas sob a mesma licença.

### Marcas e independência

TikTok é marca da TikTok Ltd./ByteDance; OBS Studio é do OBS Project; Cloudflare, Chrome e demais nomes pertencem aos seus donos. Este projeto não tem vínculo, patrocínio nem aprovação de nenhum deles. Os nomes aparecem apenas para descrever com o que o software interopera.
