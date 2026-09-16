# Testes: requisitos e limites

## Rodar a suíte local

São necessários Go 1.24+, Docker funcionando e a imagem `postgres:17-alpine`, já usada pelo Compose. Não é necessário contratar SMTP, fornecer chave Supabase ou configurar credenciais de IA. Não é preciso instalar Expo, emuladores ou um contêiner de e-mail.

```sh
go test ./...
go vet ./...
sh scripts/test-integration.sh -coverprofile=/tmp/broto-coverage.out
go tool cover -func=/tmp/broto-coverage.out
```

Sem `TEST_DATABASE_URL`, `go test` pula os testes que exigem PostgreSQL. Use o script para executar a suíte completa com `-race`. Ele cria seu próprio PostgreSQL temporário, com dados em RAM (tmpfs de 192 MB), memória limitada a 256 MB e remoção automática ao sair. Não usa o banco do app. O Go mantém cache de compilação no disco; a imagem PostgreSQL é reutilizada quando já está instalada.

## Cobertura adicionada para infraestrutura

| Arquivo | O que verifica |
|---|---|
| `internal/api/config_test.go` | Configuração obrigatória, quota e SMTP inválidos; padrões locais |
| `internal/api/mail_test.go` | SMTP por TCP local com STARTTLS e certificado verificado; autenticação, recusas, prazo e cancelamento |
| `internal/api/auth_mail_integration_test.go` | Cadastro sem confirmação automática, captura do link, reenvio, confirmação, recuperação, expiração, uso único e troca de senha; reenvio após falha SMTP |
| `internal/api/infrastructure_test.go` | Refresh concorrente, indisponibilidade do banco na autenticação, falha de gravação e de commit do upload, repetição da limpeza após excluir conta, exclusão mútua/retry dos jobs e cancelamento do scheduler |

O servidor SMTP dos testes roda dentro do processo Go, aceita conexões somente em `127.0.0.1` e captura mensagens em memória. Usa credenciais fictícias e um certificado confiado apenas pela instância de teste. O transporte de produção continua verificando certificados com as autoridades do sistema e exigindo STARTTLS.

Falhas de armazenamento são provocadas em diretórios temporários e por trigger de erro no banco descartável. Não enchemos o disco para testar falta de espaço. IA, Wikipédia e Expo continuam simulados; os contratos baseados no app estão descritos em [frontend-compatibility.md](frontend-compatibility.md).

## O que seria necessário para validar serviços reais

| Validação | Dados ou implementação necessários |
|---|---|
| E-mail chegando à caixa de entrada | `SMTP_HOST`, `SMTP_PORT`, `SMTP_USER`, `SMTP_PASSWORD`, `SMTP_FROM`, um destinatário de teste autorizado e saída de rede para o servidor |
| Link de confirmação/recuperação abrindo a interface | `SITE_URL` acessível e páginas `/confirmado` e `/nova-senha` adaptadas para chamar a broto-api |
| IA real | Chave Anthropic, modelos disponíveis na conta, imagens/casos de avaliação e orçamento autorizado |
| Push no celular | App configurado para notificações, aparelho, permissões e token Expo válido; autorização para enviar |
| Google/Apple | Google implementado e testado com provedor simulado: faltam configuração externa e adaptação do app. Apple ainda precisa de implementação |
| Compra real | Integração de pagamentos ainda por implementar e ambiente sandbox do provedor |
| Fluxo completo do app | Adaptar os serviços do frontend e executá-lo na web/dispositivo; os testes atuais exercitam a API, sem executar as telas |

Para SMTP real, o código atual usa **STARTTLS**, com porta padrão **587**; não implementa TLS implícito da porta 465. Configure os valores no `.env` local, sem compartilhá-los no chat ou versioná-los. Para validar confirmação obrigatória, use `DEV_AUTO_CONFIRM=false`. Remetente/domínio precisam estar autorizados no serviço de e-mail; configurações DNS exigidas pelo provedor são uma etapa externa à suíte.

## Limites restantes

A captura SMTP prova o comportamento do protocolo e do fluxo de autenticação; não comprova entrega final, caixa de spam, reputação ou configuração DNS do remetente. Mocks de IA/push não comprovam qualidade das respostas ou entrega no aparelho.

Também não há teste de carga prolongada, disco fisicamente cheio, queda abrupta do processo, restauração de backup, sinais do sistema/shutdown HTTP completo ou acionamento do ticker após um minuto real. Os jobs são exercitados diretamente, com concorrência e cancelamento controlados. Operações compostas entre várias requisições ainda precisam de contratos transacionais/idempotentes próprios. Estas validações não dependem de SMTP: precisam de implementação ou cenários operacionais específicos.

As credenciais externas são necessárias somente para uma validação real autorizada. Os testes locais podem continuar sendo ampliados sem elas.

## Resultado em 14/09/2026

`go test ./... -count=1`, `go vet ./...` e `sh scripts/test-integration.sh -coverprofile=/tmp/broto-infrastructure-coverage.out` passaram. A suíte completa, com `-race`, executou em aproximadamente 62 segundos e tem 29 funções `Test*`, com subcasos.

Cobertura de statements: **74,6% em `internal/api`**, **73,3% no projeto inteiro**, **95,2% em `sendSMTP`** e **100% em `LoadConfig`**. O `main` continua com 0% de cobertura automatizada. Percentuais medem código executado e não garantem cobertura de todos os cenários.

O teste de indisponibilidade reproduziu um erro real: falha ao consultar sessão retornava 401/`sem_token`. A API agora distingue esse caso e retorna 503/`banco_indisponivel`, permitindo ao frontend preservar a sessão e oferecer nova tentativa. O transporte SMTP também passou a respeitar cancelamento de contexto durante conexão e espera por resposta; configuração SMTP inválida é rejeitada na inicialização.

## Ampliação em 15/09/2026: Google, SMTP e Anthropic

Google OAuth foi implementado e testado com RSA/JWKS simulados, PostgreSQL real descartável, cookies/estado/nonce/PKCE, conflito de e-mail, identidade migrada, expiração, replay, concorrência e rollback. Veja [google-auth.md](google-auth.md). Não foi feito login real no Google ou alterado o app.

Foram adicionados testes do intervalo SMTP de 60 segundos por usuário, incluindo concorrência, bloqueio entre confirmação/recuperação e liberação após expiração. Os testes Anthropic verificam `output_config.effort`, preservação de JSON schema, omissão de effort no Haiku e leitura de texto após blocos de thinking.

`go test ./... -count=1`, `go vet ./...` e `sh scripts/test-integration.sh -coverprofile=/tmp/broto-google-coverage.out` passaram. A suíte automática completa com `-race` levou aproximadamente 77 s. Cobertura: **76,6% em `internal/api`**, **75,5% no projeto inteiro**. Os testes reais são pulados por padrão.

Validação real autorizada executada com credenciais do `.env`:

- **Anthropic:** duas chamadas HTTP à rota de chat da API com `claude-opus-5`, esforço `medium` e máximo de 2048 tokens por resposta. As duas passaram: conversa mantida, quatro mensagens persistidas e dois usos de chat registrados, em banco descartável com conta/planta fictícias. Essa amostra não é avaliação abrangente da qualidade do modelo.
- **Brevo SMTP:** conexão estabelecida, mas autenticação recusada com **535 5.7.8 Authentication failed**. Nenhum e-mail foi enviado. É necessário conferir o login e a chave SMTP na conta Brevo antes de repetir. A suíte local de TLS/SMTP e os fluxos de autenticação continuam passando.

Com autorização explícita para cada destinatário/custo, os checks podem ser executados separadamente:

```sh
# Envia uma mensagem real; não roda se o destinatário não for explicitado.
python3 scripts/test-live.py --smtp-to <destinatario-autorizado>
# Faz duas chamadas reais de chat e verifica persistência/consumo em banco descartável.
python3 scripts/test-live.py --chat
```

O script lê o `.env` sem executar seu conteúdo como shell e não imprime credenciais. Ele cria somente o PostgreSQL temporário já usado pela suíte. E-mail aceito pelo SMTP ainda exige confirmação de chegada pelo destinatário. Para teste em celular, URLs locais de confirmação precisam ser substituídas por endereços alcançáveis e as páginas precisam ser adaptadas.

Referências de configuração: [Anthropic effort](https://platform.claude.com/docs/en/build-with-claude/effort), [Opus 5 e limite de tokens](https://platform.claude.com/docs/en/models/opus-5/whats-new-opus-5), [credenciais SMTP Brevo](https://developers.brevo.com/docs/smtp-integration).

### Nova tentativa SMTP com credenciais corrigidas

O `.env` recebeu o novo login, chave SMTP e remetente fornecidos pelo usuário. O teste real retornou **525 5.7.1 Unauthorized IP address**. Nenhum e-mail foi enviado; ainda não é possível afirmar que o fluxo SMTP está funcionando. O chat real não foi repetido nesta tentativa.

É necessário autorizar o IP de saída em **Settings → Security → Authorized IPs** no Brevo. Confira a tentativa recente na lista de IPs não autorizados, pois o IP de saída pode variar conforme rede/NAT. Após liberar o endereço reconhecido, repita apenas `scripts/test-live.py --smtp-to <destinatario-autorizado>`. [Orientação oficial do Brevo](https://help.brevo.com/hc/en-us/articles/5740111683858-Authorize-and-block-IP-addresses-for-API-and-SMTP-security).

**Reteste posterior solicitado pelo usuário: aprovado.** `TestLiveSMTP` conectou com STARTTLS, autenticou e teve uma mensagem de teste aceita pelo Brevo para o remetente/destinatário autorizado. O bloqueio 525 não ocorreu nesse reteste. A entrega na caixa de entrada ainda precisa ser confirmada pelo destinatário. Nenhuma chamada Anthropic foi executada novamente.

## Produção AWS e CDN

Adicionados testes de assinatura CloudFront RSA-2048 (validação criptográfica e alteração do recurso), rejeição de configuração inválida, isolamento de fotos por usuário, validade de 300s, confiança restrita em X-Forwarded-For, backpressure com healthcheck disponível, credenciais SQL com caracteres especiais, usuário runtime sem DDL, migrations pendentes, leitura de S3 entre réplicas e exclusão com fila/retry. O teste de shutdown mantém uma requisição em andamento durante o SIGTERM simulado.

`scripts/deploy/test_deploy.py` verifica preservação de segurança na task definition, descarte de campos de resposta da AWS, detecção de rollback e não rotação/sobrescrita indevida da senha de banco. Terraform tem dois testes mock no repositório de infraestrutura.

Validação local desta entrega: `go test ./...`, `go vet ./...`, integração completa com `-race` e PostgreSQL descartável, quatro testes Python, build scratch e Compose saudável. Não houve envio de SMTP nem uso pago de IA nesses testes. AWS real foi consultada apenas para identidade, disponibilidade do RDS e preços. IAM efetivo, OIDC GitHub, rollout real, CloudFront/S3 real, recuperação de backup e teste de carga prolongado ainda precisam de ambiente provisionado. Não existe garantia de capacidade em usuários simultâneos derivada desses testes.
