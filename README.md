# broto-api

Backend do Broto em Go + PostgreSQL, independente de Supabase em execução. Substitui autenticação, acesso a dados, fotos privadas, funções de IA e rotinas agendadas encontradas em `../broto-supabase`.

## Rodar localmente

```sh
# Na primeira instalação; não substitui .env existente.
python3 scripts/setup_env.py
docker compose up -d --build
curl http://localhost:8080/healthz
```

O `.env` de desenvolvimento confirma e-mails automaticamente. Para publicar, configure SMTP e `DEV_AUTO_CONFIRM=false`, domínios reais em `PUBLIC_URL`, `SITE_URL` e `CORS_ORIGINS`, e HTTPS no proxy. A API fica exposta apenas em `127.0.0.1:8080`; o banco fica na rede interna do Compose.

Configure `DEEPSEEK_API_KEY` no `.env`. Visão (`VISION_MODEL`), fatos/confirmações (`TEXT_MODEL`), chat (`CHAT_MODEL`) e busca (`SEARCH_MODEL`) usam `deepseek-flash` por padrão. Sem chave a API continua funcionando para login/dados/fotos e retorna 503 nas operações de IA. Nenhuma chave do Supabase é necessária para executar este backend.

A integração usa `https://api.deepseek.com/chat/completions`, imagens base64 no formato `image_url`, modo JSON para respostas estruturadas e validação local contra os schemas embutidos. Thinking fica desabilitado para respostas diretas e menor consumo; `CHAT_MAX_TOKENS` limita a saída. Respostas vazias, truncadas ou fora do schema não confirmam créditos. Não existe fallback para Claude. `cost_micros` é uma estimativa conservadora pelas tarifas de pico do Flash consultadas em 21/09/2026, não a fatura do provedor (fora de pico pode custar menos).

Login Google já está implementado no backend: configure `GOOGLE_AUTH_ENABLED`, credenciais Web e callback conforme [google-auth.md](docs/google-auth.md). O app ainda precisa trocar as chamadas Supabase. Não é necessário outro contêiner nem SMTP para autenticar com Google.

As migrations são embutidas no binário, aplicadas na inicialização em transação e protegidas por lock. O comando `broto-api migrate` aplica somente as migrations. Go 1.24+ compila o projeto; Docker usa Go 1.27 para o build.

## Recursos

- Senhas bcrypt; tokens aleatórios de sessão armazenados apenas como SHA-256, com expiração em 30 dias, rotação e revogação.
- Cadastro, login, confirmação de e-mail, recuperação de senha, encerramento de sessão e exclusão de conta.
- Login Google com OIDC, PKCE, proteção do retorno pelo navegador e vínculo por subject. Nenhuma união automática de contas por e-mail.
- CRUD com campos permitidos explicitamente e isolamento por usuário, incluindo vínculos entre plantas, grupos, tarefas, conversas e fotos.
- Upload privado JPEG/PNG/WebP, URLs assinadas válidas por uma hora e limpeza de arquivos com fila persistente.
- Análise com uma chamada de visão, fatos por espécie/idioma em cache e complementação textual de diagnóstico. Prompts e schemas originais em `internal/api/assets`.
- Chat com 12 mensagens recentes e contexto das plantas/tarefas; busca de espécies com cache e imagens da Wikipédia.
- Créditos e tetos da última migration: 40 análises/mês, 50/dia; chat 150/mês e 30/dia. Créditos avulsos são consumidos antes do plano, fora do teto mensal, conforme o SQL mais recente. Uma conta grátis tem 1 cota mensal + 2 créditos de boas-vindas.
- Análise/chat mantêm consumo e gravação na mesma transação. Falhas, cancelamentos e fotos ilegíveis não consomem crédito. Pedidos de IA concorrentes da mesma conta recebem 409. A busca mantém orçamento global de 1.000 chamadas/mês, inclusive quando o provedor falha.
- Lembretes Expo em português/inglês/espanhol; tarefas consultadas por hora local, histórico de entrega e remoção de tokens inválidos. Limpeza de análises soltas após 7 dias, conversas após 30 dias e lembretes após 14 dias. Jobs executados pela API, sem pg_cron/pg_net/Vault.

## Integrar e migrar

Leia [o contrato HTTP](docs/api.md) e [o roteiro de migração](docs/migration.md). Os corpos JSON de análise/chat/busca foram preservados, com aliases `/functions/v1/*`. **Isso não torna a API um substituto transparente do SDK Supabase:** auth, consultas e storage devem ser trocados no app. O frontend foi comparado e as diferenças estão no [relatório de compatibilidade](docs/frontend-compatibility.md). A troca das chamadas no app/site e a importação de dados de produção ainda não foram realizadas.

Os scripts de concessão de créditos/assinaturas continuam sendo operações administrativas de banco. Não há endpoint público para conceder plano, compra ou anúncio. O backend original não contém validação RevenueCat/SSV para portar; isso exige uma integração separada com o provedor de pagamentos/anúncios.

## Espaço e operação

Apenas API e PostgreSQL são executados. Não há Redis, MinIO, painel ou stack Supabase. Cada serviço tem logs limitados a 2 arquivos de 5 MB. O PostgreSQL tem 384 MB de limite de memória, a API 256 MB. Fotos têm limite de **512 MB no total** por padrão (`STORAGE_QUOTA_BYTES`), com 8 MB por upload. O limite de fotos não limita o banco, backups ou cache de build; `max_wal_size` do PostgreSQL também é um alvo, não uma quota rígida de disco.

```sh
docker compose ps
docker compose logs --tail=100 api
docker system df
docker compose stop             # para os serviços; mantém os dados
```

Para reiniciar use `docker compose up -d`. Evite `docker compose down -v`: remove os volumes com banco e fotos. Não execute limpeza global do Docker sem conferir os outros projetos.

Faça backup do banco e das fotos no mesmo período de manutenção:

```sh
docker compose stop api
docker compose exec -T db pg_dump -U broto -d broto -Fc > broto.dump
docker run --rm --volumes-from "$(docker compose ps -aq api)":ro --entrypoint tar postgres:17-alpine -C /data/photos -czf - . > broto-photos.tar.gz
docker compose start api
```

O dump e as fotos contêm dados privados. Guarde fora do repositório, confira o espaço antes do backup e teste restauração em outro banco. Use o mesmo `SIGNING_KEY` ao reiniciar; trocar a chave invalida URLs assinadas existentes.

## Validar

```sh
go test ./...                   # testes unitários; integração exige banco
go vet ./...
sh scripts/test-integration.sh  # PostgreSQL temporário em RAM, removido ao sair
```

O script aceita argumentos extras de `go test`, por exemplo `sh scripts/test-integration.sh -coverprofile=/tmp/broto-coverage.out`. Os testes baseados no frontend estão em `internal/api/frontend_*_test.go`, com um snapshot das interfaces inspecionadas em `internal/api/testdata/frontend-contract.json`.

O script de integração usa a imagem PostgreSQL já utilizada pelo Compose, cria um banco exclusivo por execução e roda `go test -race`. Exercita migrations e reaplicação, autenticação, permissões, vínculos entre contas, uploads/assinaturas/quota, chat, análise, rollback de créditos, consumo concorrente, busca/cache, lembretes, exclusão e limpeza. IA/Wikipédia/Expo são simulados nesses testes; não há gasto nem notificações reais.

SMTP é exercitado por um servidor TCP local dentro dos testes Go, com STARTTLS, captura em memória, confirmação/recuperação e falhas simuladas. A suíte também cobre refresh concorrente, indisponibilidade do banco na validação de sessão, falhas de upload/commit e retry de limpeza/jobs. Não precisa de credenciais externas ou de outro contêiner. Veja [requisitos e limites dos testes](docs/testing.md), incluindo o que seria necessário para validar e-mail, IA e push reais.

`SMTP_MIN_INTERVAL_SECONDS=60` limita tentativas de e-mail por usuário, compartilhado entre confirmação e recuperação e persistido no banco. Tentativas bloqueadas retornam 429/`intervalo_email` com `Retry-After`; falha SMTP também ocupa o intervalo para evitar repetição de envios ambíguos. Os testes reais ficam separados em `scripts/test-live.py` e só enviam e-mail/gastam tokens com flags explícitas; não fazem parte da suíte automática.

Documentação dos provedores consultada: [DeepSeek Vision](https://api-docs.deepseek.com/guides/vision/), [JSON mode](https://api-docs.deepseek.com/guides/json_mode/), [preços](https://api-docs.deepseek.com/quick_start/pricing/), [serviços Compose](https://docs.docker.com/reference/compose-file/services/), [versões Go](https://go.dev/dl/).

## Produção AWS

Terraform, CloudFront privado, preparação do primeiro deploy e custos estão no repositório separado [broto-infra-tf](https://github.com/JeanRoque0/broto-infra-tf) (cópia local `../broto-infra-tf`).

O build de produção usa `scratch`, binário Go estático, UID 10001, certificados CA/RDS e timezone embutido. Não há shell ou gerenciador de pacotes na imagem. `/livez` verifica o processo; `/readyz` e `/healthz` verificam PostgreSQL. O executável aceita `healthcheck` para Docker/ECS. Ao receber SIGTERM, aguarda requisições e workers antes de fechar o pool.

Em AWS, `STORAGE_BUCKET`/`AWS_REGION` ativam S3; o IAM da task substitui access keys. `CLOUDFRONT_URL`, `CLOUDFRONT_KEY_ID` e `CLOUDFRONT_PRIVATE_KEY` ativam URLs assinadas de 5 minutos, emitidas após checar dono e existência. O contrato `signedUrl`/`expiresIn` permanece; Compose continua com arquivos locais.

`DB_HOST`, `DB_PORT`, `DB_NAME`, `DB_USER`, `DB_PASSWORD`, `DB_SSLMODE` e `DB_SSLROOTCERT` permitem credenciais separadas, com TLS verificado. `DATABASE_URL` tem precedência. `AUTO_MIGRATE=false` exige schema atualizado pela task `migrate`; o usuário runtime não faz DDL. `DB_MAX_CONNS`, `MAX_CONCURRENT_REQUESTS` e `TRUSTED_PROXY_CIDRS` controlam pool, admissão e confiança no proxy. Só configure as sub-redes do ALB como proxies confiáveis.

`.github/workflows/deploy.yml` roda CI, faz build/push no ECR e deploy via OIDC. O deploy permanece desativado até a repository variable `DEPLOY_ENABLED=true`; configure as environment variables do Terraform e proteja o environment `production` para main. Não há secrets no YAML. O script `scripts/deploy/deploy.py` exige digest imutável, executa migrations e detecta rollback do ECS.

Validação adicional: `python3 -m unittest discover -s scripts/deploy -p 'test_*.py'`. Testes de assinatura CDN, confiança no proxy, pool/runtime sem DDL, S3 compartilhado e fila de exclusão rodam junto à integração; não contatam serviços pagos. O fluxo real OIDC → ECR → ECS → CloudFront ainda exige infraestrutura provisionada, domínio e secrets configurados.
