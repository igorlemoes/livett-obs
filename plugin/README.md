# livett (plugin nativo para o OBS 32.2.2, Windows x64)

Camada de interface do projeto (C++17 / Qt6). **Não tem lógica de negócio**: inicia o controlador Go (`controller.exe`), cria o painel
**LiveTT** (navegador embutido do OBS, apontando para `http://127.0.0.1:8765/`, com `/live` embutido) e adiciona
Ferramentas → "LiveTT: janela flutuante Ao vivo", uma janela **nativa** com fundo translúcido por pixel e texto opaco, estatísticas, chat e fotos pelo
`/avatar` do controlador (o Qt do OBS não tem HTTPS). Arquitetura geral: [../docs/ARQUITETURA.md](../docs/ARQUITETURA.md).

## Como o plugin acha o controlador (`resolve_controller` em `src/plugin-main.cpp`)

Só um caminho: `data/controller/controller.exe` (ao lado da DLL, na pasta do plugin) → `serve --background --root <essa pasta>`. Se o arquivo não existir, o plugin registra o aviso no log do OBS e não sobe nada.

O `config.json` fica em `%APPDATA%\obs-studio\plugin_config\livett\` (layout dos painéis, janela flutuante: posição, opacidade, "sempre no topo", `open_floating_on_start`).

## Compilar

Pré-requisitos (não versionados): Visual Studio 2022 Build Tools (MSVC), CMake, Ninja.

1. `plugin\build-deps\` (**~1,8 GB, não versionar**), cada item em pasta própria e nova: `obs-studio` (tag **32.2.2** + submódulo `plugins/obs-browser`),
   `obs-deps\x` e `qt6\x` (obs-deps `2026-07-15`; os SHA-256 estão em `CMakePresets.json` do OBS 32.2.2: windows-x64 prebuilt `6f90e959…`, qt6 `7c7f9857…`).
2. `powershell -File plugin\tools\gen-importlibs.ps1` gera `obs.lib` e `obs-frontend-api.lib` a partir do OBS instalado (mesma versão do alvo).
3. `plugin\tools\build.cmd` (rode no cmd/PowerShell, **não** no Git Bash, que altera `/s /c`). Saída: `plugin\build\livett\livett.dll`.

Normalmente você não chama isso diretamente: `scripts\build.ps1` compila o plugin e o controlador e gera o zip.

## Testar sem risco (cópia portátil do OBS)

Mantenha uma cópia portátil do OBS 32.2.2 (por exemplo `work\obs-local`, ignorada pelo git) para não mexer no OBS principal:

1. Copie a DLL para `obs-plugins\64bit\` e o `controller.exe` para `data\obs-plugins\livett\controller\controller.exe`.
   (No modo portátil o OBS **não** lê a pasta de plugins do usuário; use as pastas padrão da cópia.)
2. Abra `obs64.exe --portable --multi --disable-shutdown-check`.
3. O log (`config\obs-studio\logs`) deve conter `[livett] loaded`, `controller start requested (...controller.exe): ok`, `docks added`, `docks placed`.
4. Pare o controlador do OBS principal antes (a porta 8765 é única). **Não** instale primeiro no OBS principal.

## Instalar no OBS principal

Com o OBS fechado, extraia o pacote em `C:\ProgramData\obs-studio\plugins\` (ver [../docs/OPERACAO.md](../docs/OPERACAO.md)). O OBS 32 só lê plugins dessa pasta.
Nunca troque o plugin com uma LIVE no ar.

## O serviço "LiveTT" e a API de controle

`src/obs-service.cpp` registra o serviço `tiktok_live` (botão Iniciar transmissão do OBS) e `src/obs-api.cpp` é a API local pela qual o controlador lê e comanda o OBS (substitui o
obs-websocket). Teste do botão numa cópia do OBS: `tools/obs-button-test/README.md`; da API: `go run ./cmd/obscheck` (em `controller`).

## Limites

Compilado contra o OBS **32.2.2**; outras versões podem exigir recompilar contra as respectivas fontes. A DLL não é assinada.
