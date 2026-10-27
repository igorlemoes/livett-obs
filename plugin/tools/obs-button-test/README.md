# Teste da janela de início e do botão do OBS (plugin → serviço "LiveTT")

Prova, num OBS de teste, que o clique em "Iniciar transmissão" abre a janela de início, que "Iniciar LIVE" chama o controlador e entrega o destino ao OBS,
que o serviço do perfil volta ao que era quando a transmissão para e que o `service.json` do perfil **não muda**.
Nunca use no OBS real: abre janelas e inicia a saída.

1. Instale a DLL e esconda o controlador da cópia portátil (renomeie `data\obs-plugins\livett\controller` para `controller-off`).
2. `go build -o stub.exe stub\main.go` e `stub.exe 127.0.0.1:18765 stub.log` (um controlador de mentira; `STUB_FAIL=1` faz recusar).
3. Abra o OBS de teste com `LOCALAPPDATA` apontando para uma pasta temporária e `TIKTOK_PLUGIN_BASE=http://127.0.0.1:18765`.
4. Deixe no perfil um serviço comum (`SetStreamServiceSettings` na API do plugin, veja `cmd/obscheck`) e guarde o hash do `service.json`.
5. `click-button.ps1 -ProcessId <pid> -ButtonName 'Iniciar transmissão'` (mouse de verdade) e
   `drive-dialog.ps1 -ProcessId <pid> -WindowTitle 'Iniciar LIVE no TikTok' -Button 'Iniciar LIVE'` (ou `Cancelar`).
6. Confira `stub.log` (pedidos recebidos), o log do OBS (`Connecting to RTMP URL ...`), `GetStreamServiceSettings` depois (o serviço do usuário de volta)
   e o hash do `service.json` (igual ao de antes).

Execute os `.ps1` com o Windows PowerShell 5.1 (`powershell.exe`): a automação de interface (UIAutomation) é do .NET Framework.
