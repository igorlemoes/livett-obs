# Aviso de risco — leia antes de usar

Este projeto é **independente e não oficial**. Não tem vínculo, patrocínio nem aprovação do TikTok, da ByteDance, do OBS Project, da Cloudflare ou do Google.
**Usar esta ferramenta pode violar os Termos de Serviço do TikTok e pode custar a sua conta.** Se isso não é aceitável para você, não use.

## 1. Como a ferramenta fala com o TikTok (sem rodeios)

- Ela **não usa uma API oficial**. Usa serviços internos e não documentados do TikTok, chamados a partir da **sua própria sessão** no Chrome. A senha e a verificação de login
  são sempre feitas por você, na janela do próprio TikTok. Ela **não** usa assinaturas de segurança, identificadores de dispositivo, cookies ou arquivos de aplicativos do TikTok,
  e a sala sai marcada como de terceiros (`live_type_third_party`).
- Ela cria a sala da LIVE, mantém a LIVE no ar, **lê as mensagens da sua sala** (chat, presentes, entradas), pode **enviar mensagens ao chat**, mudar o "Sobre mim" e encerrar a LIVE.
- Com um assistente de IA (MCP), parte disso pode ser feita por um programa, não por uma pessoa, inclusive responder espectadores. Isso só acontece se você ligar.

## 2. O que os Termos do TikTok dizem

Os Termos de Serviço do TikTok (versão consultada: "outras regiões", atualizada em 1/12/2025; leia a versão em vigor para o seu país) proíbem, na seção 5, entre outras
coisas: engenharia reversa e criação de obras derivadas dos serviços; usar scripts automatizados para coletar informações ou interagir com os serviços; incorporar os
serviços em outro programa ou produto; e contornar medidas de restrição de acesso. O TikTok também se reserva o direito de recusar o serviço e **desativar contas** a qualquer momento.

Este projeto não foi feito para contornar a elegibilidade para LIVE (ele só segue se o TikTok confirmar que a conta pode criar uma sala), mas **o modo como funciona pode ser
considerado uma violação dos termos**. Não temos como saber como o TikTok enxerga isso hoje nem como vai enxergar amanhã. As regras específicas de LIVE e o contrato do
TikTok LIVE Studio podem ter exigências adicionais que não foram avaliadas aqui.

## 3. O que pode acontecer com você

- Restrição de recursos, suspensão temporária ou **banimento da conta**, com perda de acesso a LIVE, seguidores, presentes e ganhos.
- Remoção de conteúdo ou da LIVE.
- A ferramenta **pode parar de funcionar a qualquer momento** (qualquer atualização do TikTok pode quebrá-la), inclusive no meio de uma transmissão.
- O TikTok pode agir contra o próprio projeto (por exemplo, pedir a remoção do código).

Não há garantia, não há suporte e **o risco é seu**. Não é aconselhamento jurídico: em caso de dúvida (principalmente se for cobrar por isso ou usar numa empresa), consulte um advogado.

## 4. Como reduzir o risco

1. **Teste primeiro com uma conta que você aceite perder**, nunca com a conta principal ou comercial no começo.
2. Se a sua conta tiver acesso à **transmissão por software com chave de transmissão fornecida pelo próprio TikTok**, prefira esse caminho oficial (confira as regras atuais do TikTok).
3. **Não use para spam**, para enganar espectadores ou para contornar restrições ou banimentos. Respeite as Diretrizes da Comunidade, os direitos autorais do que você transmite e a idade mínima.
4. **Não deixe um programa conversar sozinho com o público.** As respostas automáticas (assistente de IA) ficam desligadas por padrão, valem só para a LIVE em que você as liga, e
   é melhor avisar os espectadores quando uma resposta for automática.
5. Não compartilhe a pasta `runtime`, o perfil do Chrome nem as chaves de transmissão. Não instale versões do plugin de origem desconhecida.
6. Mantenha só o necessário ligado: se você não usa a câmera do celular ou o assistente de IA, não os ative.

## 5. Privacidade

- As sessões das contas ficam **só neste computador**, criptografadas com a chave do seu usuário do Windows (DPAPI).
- Nada do TikTok é enviado a terceiros. Na **câmera do celular** o vídeo e o som vão direto do celular para o OBS; apenas as mensagens de conexão passam pelo servidor de
  sinalização da Cloudflare (ver [SEGURANCA.md](SEGURANCA.md)).
- Os registros de diagnóstico (`runtime/traces`) mascaram segredos, mas podem conter textos de espectadores: não os publique.
