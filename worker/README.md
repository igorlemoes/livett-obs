# Relay da câmera do celular (Cloudflare Worker)

Serve a página que o celular abre ao ler o QR code (HTTPS com certificado válido: é isso que faz o iPhone liberar a câmera) e leva as
mensagens de conexão entre o celular e o OBS. **O vídeo e o som não passam por aqui**: vão direto do celular para o OBS (WebRTC).
Por isso o consumo é minúsculo e cabe no plano gratuito da Cloudflare (ver abaixo).

## Como funciona

- `GET /c/<sala>` é a página do celular (a mesma do controlador, copiada de `controller/internal/assets/camera/`).
- `GET /ws?room=<sala>` é um WebSocket que entra numa sala (um *Durable Object* com SQLite, com hibernação: sala parada não gasta).
- A **sala** é `sha256(rxKey)[0:32]`, onde `rxKey` é uma chave que só existe no computador do usuário (o controlador a guarda em
  `runtime/camera/pairing.json`). Só quem tem `rxKey` entra como **receptor** (a página dentro do OBS); nem o QR code permite calculá-la.
- O receptor registra na sala o hash `sha256(phoneKey)` do código do celular. O QR leva `phoneKey` no **fragmento** (`#k=...`), que o
  navegador não envia ao servidor com o pedido da página. O celular entra com `phoneKey`; a sala confere o hash.
- Trocar o código (ou "Parar de aceitar") registra outro hash (ou vazio): o celular conectado é desconectado e o QR antigo para de valer.
- A sala não guarda mensagens (só repassa) e só guarda o hash do código do celular. Mensagens de até 64 KiB, 300 por 10 s por conexão;
  quem não entra em 15 s é desconectado.

## Custo

Plano gratuito da Cloudflare (conferido nas páginas de preços em 27/10/2026; podem mudar): Workers 100 mil pedidos/dia; Durable Objects
(só com SQLite no plano gratuito) 100 mil pedidos/dia e 13.000 GB-s/dia. Uma conexão WebSocket conta como 1 pedido; mensagens recebidas pelos
Durable Objects contam 1 para cada 20; com hibernação a sala parada não cobra duração. Passando do limite, o serviço **falha até o dia
seguinte** (não há cobrança sem você assinar o plano pago, US$ 5/mês).

## Publicar (uma vez, por quem mantém o projeto)

Precisa de Node 20+ e de uma conta Cloudflare (gratuita).

```powershell
cd worker
npm install
npx wrangler login      # abre o navegador para você autorizar; nada de senha passa pelo terminal
npm run deploy          # copia as páginas e publica; mostra o endereço https://tiktok-live-camera.SEU-USUARIO.workers.dev
```

O endereço publicado pelo projeto (27/10/2026) é `https://tiktok-live-camera.tiktok-live-camera-relay.workers.dev` e está em `camera.DefaultRelay` (`controller/internal/camera/hub.go`); depois de publicar de novo em outra conta, troque-o e gere um pacote novo. Para testar outro
endereço sem recompilar: variável de ambiente `TIKTOK_CAMERA_RELAY=https://...` (o controlador só aceita `https://`).

## Desenvolver e testar

```powershell
cd worker
npm install
npm test        # sobe o Worker de verdade (wrangler dev --local) e testa as salas por WebSocket
npm run dev     # Worker local em http://127.0.0.1:8787
```

Teste ponta a ponta local: `npm run dev -- --port 8799`, e o controlador com `TIKTOK_CAMERA_RELAY=http://127.0.0.1:8799` (só `http://127.0.0.1` é aceito sem TLS).

Licença: GPL-2.0-or-later (a do projeto).
