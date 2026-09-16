# Google OAuth na broto-api

Implementado em 15/09/2026. O backend autentica diretamente com Google via OAuth Authorization Code + OpenID Connect e emite suas próprias sessões. Não usa Supabase. O `broto-app` permanece sem alterações; seu botão ainda usa o Supabase até a adaptação descrita abaixo. Apple não foi implementado.

## Configuração

No `.env` privado:

```dotenv
GOOGLE_AUTH_ENABLED=true
GOOGLE_CLIENT_ID=<ID do cliente OAuth Web>
GOOGLE_CLIENT_SECRET=<segredo do cliente Web>
GOOGLE_REDIRECT_URL=https://<dominio-da-api>/v1/auth/google/callback
GOOGLE_RETURN_URLS=broto://auth-callback,https://<dominio-do-app>/auth-callback
```

Cadastre **exatamente** `GOOGLE_REDIRECT_URL` nas URIs de redirecionamento autorizadas do cliente Web no Google. Configure público/usuários de teste e consentimento conforme o projeto. É possível manter o callback Supabase durante a transição. A configuração acima é proposta para este backend, não uma exportação automática dos ajustes do Supabase.

Somente o backend recebe o segredo. O campo aceita um único Client ID Web, não uma lista de clientes Android/iOS. As rotas não aceitam ID tokens arbitrários enviados pelo app. Não há opção para ignorar nonce ou permitir identidade sem e-mail verificado.

No desenvolvimento web local, `http://localhost:8080/v1/auth/google/callback` é permitido. No aparelho, localhost identifica o próprio celular: use um endereço HTTPS alcançável. O retorno `broto://auth-callback` é para a API devolver o usuário ao app; o callback cadastrado no Google é o endereço HTTP(S) da API.

`GOOGLE_RETURN_URLS` é uma lista exata, sem curingas, query strings, fragmentos ou credenciais na URL. Aceita HTTPS, HTTP loopback local e o deep link exato `broto://auth-callback`. Adicione a origem web também em `CORS_ORIGINS`.

Por padrão Google está desativado e suas rotas retornam 503/`google_nao_configurado`. Com Google habilitado, configuração incompleta impede a inicialização. Google pode funcionar sem SMTP, inclusive com `DEV_AUTO_CONFIRM=false`; os fluxos de e-mail ainda exigem SMTP. Desativar Google impede novos logins por esse provedor, mas não revoga sessões Broto já emitidas.

## Contrato e fluxo do app

1. O app gera um `code_verifier` aleatório, criptograficamente seguro, com 43–128 caracteres do conjunto `[A-Za-z0-9._~-]`. Calcula `BASE64URL(SHA256(verifier))`, sem padding, como `code_challenge`. PKCE `plain` não é aceito.
2. Envia `POST /v1/auth/google/start`:

```json
{"code_challenge":"<SHA-256 em base64url>","redirect_uri":"broto://auth-callback"}
```

Resposta 200:

```json
{"url":"https://<api>/v1/auth/google/authorize?state=<aleatorio>","state":"<aleatorio>","expires_in":600}
```

3. Guarda `state` e `code_verifier` associados à tentativa. Abre `url` com `WebBrowser.openAuthSessionAsync(url, redirectURI)`. **Abra a URL da API devolvida**, para que o navegador receba o cookie de proteção; não monte diretamente uma URL Google. `/authorize` só aceita uma abertura por tentativa. Ao cancelar ou reiniciar, gere outra tentativa.
4. A API coloca cookie HttpOnly, Secure em HTTPS e SameSite=Lax no navegador, vincula esse cookie ao estado e redireciona ao Google. Pede apenas `openid email profile`, além de nonce e um PKCE separado entre API e Google.
5. Google retorna à API. O callback exige o cookie correto e estado não expirado, consome o estado antes da troca externa e verifica assinatura RS256, emissor, audience, expiração, nonce, subject e e-mail verificado. Chaves Google são obtidas do JWKS oficial com cache da biblioteca OIDC. Não há chamada Google na inicialização do servidor.
6. A API encontra/cria o usuário e devolve ao app:

```text
broto://auth-callback?state=<estado-original>&code=<codigo-temporario>
```

O app deve conferir que `state` corresponde à tentativa pendente, recuperar seu verifier e enviar:

```http
POST /v1/auth/google/exchange
Content-Type: application/json

{"code":"<codigo-temporario>","code_verifier":"<verifier-original>"}
```

7. A resposta 200 é o mesmo objeto de sessão de `/v1/auth/login`: `access_token`, `token_type`, `expires_in`, `expires_at` e `user`. Salve a sessão no armazenamento seguro e atualize o estado de autenticação. Refresh/logout/troca de senha continuam usando `/v1/auth/*`.

O código final expira em **60 segundos**, é armazenado como hash e só pode produzir uma sessão. Trocas concorrentes têm um único vencedor. Código incorreto/expirado, verifier incorreto ou replay retornam 400/`oauth_codigo_invalido`. Se a gravação da sessão falha, a transação preserva o código para retry dentro da validade. Nenhum access token da API ou do Google é incluído na URL de retorno.

## Erros e cancelamento

Após validar estado/cookie, erros voltam ao retorno permitido com `state` e `error`, sem código:

| Erro | Tratamento no app |
|---|---|
| `google_login_cancelado` | Encerrar a tentativa sem entrar; inclui recusa do Google |
| `google_resposta_invalida` | Reiniciar o login |
| `google_autenticacao_falhou` | Reiniciar; pode ser falha do Google, configuração ou identidade inválida |
| `google_email_em_uso` | A conta precisa de migração do vínculo ou vinculação autenticada; não criar duplicata |
| `google_indisponivel` | Falha local de persistência; reiniciar a tentativa |

Estado/cookie inválido, expirado ou reutilizado recebe JSON 400/`oauth_state_invalido` diretamente, sem redirecionamento. O limite por IP é 30 requisições por minuto, compartilhado entre as quatro rotas Google; excesso retorna 429/`muitas_requisicoes`. As respostas usam `Cache-Control: no-store` e `Referrer-Policy: no-referrer`. Ao colocar um proxy, evite gravar query strings de OAuth nos access logs.

`GET /authorize` existe porque o cliente HTTP nativo e o navegador podem usar cookies diferentes. O cookie do navegador e o PKCE do app cumprem funções distintas e ambos são exigidos.

## Usuários e migração

A migration `004_google_oauth.sql` adiciona `oauth_identities`, `oauth_attempts` e `oauth_codes`. As migrations antigas não foram reescritas. Registros temporários expirados são removidos pelo job de limpeza. A exclusão de conta remove identidades e códigos finais vinculados.

O vínculo é `(provider='google', subject=<sub do Google>) → users.id`. Login repetido usa esse vínculo, mesmo se o e-mail Google mudar. O backend não altera automaticamente o e-mail cadastrado nem une contas por e-mail. Novas contas recebem perfil inicial e senha inutilizável `!google-only`; nenhuma senha Google é recebida ou armazenada. Aceite de termos não é presumido pelo OAuth.

Se já houver uma conta com o mesmo e-mail, o fluxo retorna `google_email_em_uso`. Antes da migração de produção, importe o subject real da identidade Google do Supabase e associe ao **mesmo UUID de usuário**. Não use o UUID interno da linha de identidade como se fosse o subject Google: confirme os campos contra a exportação real. Isso preserva plantas, planos e históricos. Não há endpoint de vinculação automática nem importação de produção executada.

O frontend deve substituir `signInWithOAuth` e `exchangeCodeForSession` em `src/services/supabase/oauth.ts`, adaptar o handler de deep link e atualizar diretamente o store de sessão. O evento Supabase `onAuthStateChange` não será emitido por este backend.

## Testes

`google_test.go` verifica configuração, vetor PKCE, validação de tokens RSA/JWKS, nonce, audience, emissor, expiração, e-mail, falhas do provedor e rotas desativadas. `google_integration_test.go` usa PostgreSQL real descartável para perfil/sessão, conflito e migração de identidade, cookie/estado, cancelamento, expiração, replay, concorrência de callbacks/logins/trocas, rollback da sessão e limpeza.

O transporte Google é simulado e os tokens de teste são assinados de verdade com chaves RSA temporárias. Nenhum login em conta Google real foi executado. A ativação real depende da configuração no Google e da adaptação do app.

Referências: [Google OpenID Connect](https://developers.google.com/identity/openid-connect/openid-connect), [Google OAuth Web Server](https://developers.google.com/identity/protocols/oauth2/web-server), [go-oidc](https://github.com/coreos/go-oidc), [oauth2](https://pkg.go.dev/golang.org/x/oauth2).
