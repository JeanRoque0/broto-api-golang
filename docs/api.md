# Contrato HTTP

Base local: `http://localhost:8080`. Envie `Authorization: Bearer <access_token>` nas rotas privadas. JSON usa `Content-Type: application/json`; upload recebe o binário da foto. Erros têm `{ "erro": "codigo" }`, podendo incluir `cap`, `limite` ou `motivo`. 401 = sessão inválida; 402 = falta de plano/crédito; 409 = operação concorrente ou foto em uso; 429 = teto/rate limit; 503 = serviço indisponível ou integração não configurada; 507 = quota de fotos.

Se a consulta de sessão falhar por erro no banco, a rota privada retorna 503/`banco_indisponivel`. O cliente deve permitir nova tentativa e preservar a sessão; esse erro não indica token expirado.

## Swagger e OpenAPI

Com a API executando, abra **http://localhost:8080/docs/** (`/swagger` redireciona para essa página). O contrato para importar no Postman/Insomnia está em **http://localhost:8080/openapi.json** e no arquivo [openapi.json](../internal/api/docs/openapi.json).

O Swagger é servido pelo próprio backend, inclusive JavaScript e CSS, sem CDN ou serviço de validação externo. Os arquivos comprimidos ficam embutidos no binário de produção. A interface usa o mesmo servidor que a página; `PUBLIC_URL` deve corresponder ao endereço público da API. Essa origem e as origens explícitas de `CORS_ORIGINS` são aceitas; cabeçalhos Host enviados pelo cliente não ampliam a lista.

Para testar:

1. Faça `POST /v1/auth/login` com uma conta existente e copie `access_token`.
2. Clique em **Authorize**, cole apenas o token e confirme.
3. Use **Try it out** nas rotas privadas. O navegador não persiste a autorização entre recarregamentos.

As chamadas são reais: exclusões removem dados, SMTP pode enviar e-mails e IA pode gerar cobrança. Use contas e dados de teste. Google OAuth exige o fluxo de navegador/PKCE descrito em [google-auth.md](google-auth.md), não apenas executar seu callback no Swagger.

No ALB provisório **sem HTTPS**, apenas as rotas de saúde estão liberadas. O Swagger está incluído na aplicação, mas o acesso público às demais rotas depende da configuração de HTTPS no ingresso. Localmente, a interface está disponível na porta 8080.

## Saúde

| Método e rota | Retorno |
|---|---|
| `GET /livez` | 200 `{ok:true}` enquanto o processo atende HTTP |
| `GET /readyz` | 200 com PostgreSQL acessível; 503 `{erro:"banco_indisponivel"}` em falha |
| `GET /healthz` | Alias de readiness |

Essas rotas são públicas e não ocupam a fila limitada de requisições. A API pode responder 503/`servidor_ocupado` com `Retry-After: 1` quando essa fila estiver cheia. Rotas privadas possuem limite por usuário de 120 requisições por minuto por processo.

## Autenticação

Para login Google direto, veja [o contrato OAuth](google-auth.md): `/v1/auth/google/start`, `/authorize`, `/callback` e `/exchange`. A sessão final tem o mesmo formato do login por senha. Apple não está implementado.

| Método e rota | Corpo / retorno |
|---|---|
| `POST /v1/auth/signup` | `{email,password,name?}`; sessão local ou 202 `{confirmation_required:true}` |
| `POST /v1/auth/login` | `{email,password}`; sessão |
| `POST /v1/auth/resend` | `{email}`; envia link de confirmação |
| `POST /v1/auth/recover` | `{email}`; envia link de recuperação |
| `POST /v1/auth/verify` | `{token_hash,type:"email" ou "recovery"}`; sessão, token de e-mail de uso único |
| `GET /v1/auth/user` | Usuário autenticado, e-mail e metadados |
| `POST /v1/auth/refresh` | Sem corpo; invalida token atual e retorna nova sessão |
| `POST /v1/auth/logout` | Sem corpo; invalida token atual |
| `PATCH /v1/auth/password` | `{password}`; invalida todas as sessões e retorna nova sessão |
| `DELETE /v1/account` | Remove conta e dados; fotos são eliminadas por fila persistente |

Sessão: `{access_token,token_type:"bearer",expires_in:2592000,expires_at,user:{id,email,email_confirmed_at,user_metadata}}`. O token é opaco, **não é JWT Supabase**. Guarde no armazenamento seguro do celular; substitua-o depois de refresh, verify ou troca de senha. Senhas novas seguem o app: mínimo de 8 caracteres (contagem UTF-16), uma maiúscula e um caractere especial, com máximo de 72 bytes do bcrypt; login aceita senhas antigas mais curtas após importação compatível. Tokens de e-mail expiram em uma hora. O SMTP exige STARTTLS (normalmente porta 587).

O site recebe `${SITE_URL}/confirmado?token_hash=...&type=email` ou `/nova-senha?...&type=recovery`. A página chama `verify` na API. Na recuperação, usa a sessão retornada para chamar `PATCH /v1/auth/password`.

Confirmação e recuperação compartilham o intervalo por usuário de `SMTP_MIN_INTERVAL_SECONDS` (padrão 60 s). Nova tentativa antes disso recebe 429/`intervalo_email` e `Retry-After` em segundos. O intervalo é reservado atomicamente antes do envio e permanece mesmo se o SMTP falhar. O cliente deve aguardar antes de tentar novamente; o limitador geral de autenticação continua valendo.

## Dados

`GET /v1/data/<recurso>` retorna uma lista (máximo 200; padrão 100). `GET /v1/data/<recurso>/<id>` retorna um objeto. `profiles` retorna sempre o perfil da sessão; use `/v1/data/profiles` para ler/alterar. POST retorna o registro criado (201); PATCH retorna o atualizado; DELETE retorna `{ok:true}`. PATCH/DELETE exigem ID, exceto perfil.

Filtros são igualdade: `?plant_id=<uuid>`, `?thread_id=<uuid>`, `?archived_at=null`, etc. Apenas campos conhecidos são aceitos. Paginação: `?limit=100&offset=100`. A ordenação padrão é decrescente por data principal do recurso (tarefas por `next_at`) e desempata pela chave. Use `order=asc` para mensagens e grupos. Use `active=true` em `plant_tasks` para excluir tarefas de plantas arquivadas. Não há DSL PostgREST, join genérico, upsert genérico, Realtime ou RPC genérica. POST de `push_tokens` pode ser repetido pelo mesmo usuário; token já pertencente a outra conta retorna 409.

| Recurso | Métodos | Observação |
|---|---|---|
| `profiles` | GET, PATCH | Nome, avatar, fuso, preferências e consentimentos; plano/saldos/tetos são protegidos |
| `plants` | GET, POST, PATCH, DELETE | POST `{nickname,...}`; gera seis tarefas iniciais |
| `plant_groups` | GET, POST, PATCH, DELETE | POST `{name}` |
| `plant_tasks` | GET, POST, PATCH, DELETE | `plant_id,kind,interval_days,next_at,enabled,remind_at` |
| `care_events` | GET, POST, PATCH, DELETE | `plant_id,kind,note,happened_at` |
| `identifications` | GET, PATCH, DELETE | PATCH somente `plant_id,corrected_species,was_helpful,resolved_at` |
| `chat_threads` | GET, DELETE | Criadas somente pelo endpoint de chat |
| `chat_messages` | GET | Criadas somente pelo servidor |
| `push_tokens` | GET, POST, PATCH, DELETE | Chave é `token`; POST `{token,platform}`; token Expo |
| `reminder_events` | GET, PATCH | PATCH somente `{read_at}` |
| `announcements` | GET | Apenas avisos ativos; administração via banco |
| `ad_rewards`, `credit_purchases` | GET | Histórico da própria conta |

`user_id` é definido pelo servidor, nunca enviado pelo app. IDs de planta/grupo/conversa e caminhos de fotos precisam pertencer à mesma conta. A lista exata de campos editáveis está em `internal/api/data.go`, no mapa `resources`. O schema preserva os nomes de coluna originais.

## Fotos

1. `POST /v1/photos` com JPEG/PNG/WebP binário de até 8 MiB (sem multipart) → 201 `{path:"<user-id>/<random>.jpg"}`.
2. Guarde `path` em `plants.photo_path`, `profiles.avatar_path` ou envie a `identify`.
3. `POST /v1/photos/sign` com `{path}` → `{signedUrl,expiresIn} (300 segundos via CloudFront; 3600 no armazenamento local)`.
4. `GET` na URL assinada não precisa do header de sessão.
5. `DELETE /v1/photos` com `{path}` apaga uma foto sem referências. Remova o vínculo primeiro se receber 409.

Não grave a URL assinada no banco. A rota de leitura usa a assinatura, validade e registro do arquivo. Uploads nunca sobrescrevem caminhos existentes.

## IA

### Análise

`POST /v1/identify` ou `/functions/v1/identify`:

```json
{"photoPaths":["<user-id>/<photo>.jpg"],"plantId":null,"language":"pt-BR"}
```

Exatamente uma foto; idiomas `pt-BR`, `en-US`, `es-ES`. Retorna `especie`, `cuidados`, `toxica_para_pets`, `temperatura`, `cultivo`, `simbolismo`, `saude`, `diagnostico`, `como_confirmar`, `identification_id`. Campos de complementação podem ser nulos se a chamada textual falhar. Foto ilegível retorna 200 `{erro:"foto_ilegivel"}`, sem consumo. O app continua responsável por exibir a ressalva veterinária ao mostrar toxicidade, como no backend original.

### Chat

`POST /v1/chat` ou `/functions/v1/chat`:

```json
{"message":"Quando regar?","threadId":null,"plantId":null}
```

Retorna `{threadId,reply,restantes}`. Reutilize `threadId` na próxima mensagem. Mensagem é limitada a 800 caracteres. Exige Pro válido ou assinatura de chat válida. `plantId` seleciona contexto da planta.

### Busca

`POST /v1/search` ou `/functions/v1/search` com `{term:"jiboia"}` retorna `{resultados:[{scientific,common,extract,images}],fonte:"cache"|"modelo"|"vazio"}`. Termos menores que três caracteres retornam lista vazia. As imagens continuam em URLs da Wikimedia.

`POST /functions/v1/delete-account` é alias da exclusão de conta. As antigas funções de cron não são rotas públicas: a API executa esses trabalhos internamente.

## Exemplo de cliente

```ts
async function request(base: string, token: string, path: string, method = 'GET', body?: unknown) {
  const response = await fetch(`${base}${path}`, {
    method,
    headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const data = await response.json();
  if (!response.ok) throw Object.assign(new Error(data.erro), { status: response.status, data });
  return data;
}
// Equivalente à leitura do jardim do usuário autenticado:
const plants = await request(API_URL, token, '/v1/data/plants?archived_at=null');
```

Em análise, confira também `data.erro` mesmo quando HTTP = 200. Para completar uma tarefa, grave o evento e atualize `next_at` da tarefa; a API não presume que criar um evento é uma solicitação de reagendamento. Esses passos são chamadas distintas, como no acesso direto anterior.

## Leituras usadas pelo frontend

- `GET /v1/species-facts?scientific=<nome>&language=pt-BR`: retorna a ficha em cache diretamente ou `null`; exige sessão. Idiomas: `pt-BR`, `en-US`, `es-ES`.
- `GET /v1/reminders/unread-count`: `{count}` de todos os lembretes próprios não lidos, independente da paginação.
- `POST /v1/reminders/read`: marca todos os lembretes próprios como lidos, sem corpo, e retorna `{ok:true,updated}`. Repetir é seguro.

Veja [o comparativo com o app](frontend-compatibility.md) para sessão, OAuth e exportação.

## Manutenção do contrato

`internal/api/docs/openapi.json` é um artefato versionado: descreve 56 caminhos e 78 operações, incluindo aliases. Os schemas de dados incluem campos, tipos e nulabilidade; os corpos de escrita restringem os campos à allowlist real de `data.go`. Restrições adicionais de negócio estão nos handlers e migrations. JSONB permanece flexível, pois contém conteúdo variável.

```sh
python3 scripts/generate-openapi.py
python3 scripts/generate-openapi.py --check
# Com scripts/requirements-docs.txt instalado em um ambiente Python:
python3 -m openapi_spec_validator internal/api/docs/openapi.json
```

Ao mudar colunas públicas, atualize `docs/data-columns.json` usando `scripts/export-doc-columns.sh` contra o PostgreSQL local já migrado; ele exporta somente metadados das 13 tabelas expostas, sem registros ou credenciais. Depois, regenere o OpenAPI. A integração compara esse snapshot com um banco criado do zero pelas migrations; o CI verifica geração e validade OpenAPI. Ao adicionar rotas fora do CRUD, atualize também o gerador e a documentação descritiva.

Os testes cobrem publicação dos arquivos, conteúdo comprimido, proteção das rotas documentadas, allowlists de escrita, metadados do banco e uma escrita autenticada com a origem do Swagger. Não enviam e-mails nem fazem chamadas de IA reais.
