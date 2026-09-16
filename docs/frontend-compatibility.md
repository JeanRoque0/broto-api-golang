# Compatibilidade com broto-app

Comparação feita em 14/09/2026 com o checkout local de `broto-app`, commit **c56e17e50897e576297a015d1a7a661288bca022**. O frontend foi inspecionado e permaneceu sem alterações. Os testes executam o backend com chamadas equivalentes às que os serviços do app precisarão fazer; não executam o React Native.

## Dificuldade da adaptação

**Média para a integração principal; maior para paridade completa com Google/Apple, exportação de fotos e operações compostas.** O app já separa telas/hooks de acesso aos dados: há 15 módulos em `src/services/supabase`, três serviços de IA e um cliente Axios compartilhado. Encontramos referências a Supabase em 49 arquivos e consultas literais a 13 tabelas. Muitas referências são apenas imports dos serviços, por isso a maior parte das alterações pode se concentrar nessa camada.

Não há chamadas a RPC genérica ou assinaturas Realtime nas fontes inspecionadas. Os modelos de plantas, perfil, tarefas, análise e mensagens estão próximos do schema da API. O acoplamento mais forte é autenticação: o SDK emite eventos, restaura sessões e renova tokens; o app depende desses eventos para sair da tela de login.

A primeira avaliação, feita somente com `broto-supabase`, não tinha como detectar login social no app. Agora está confirmado: Google usa OAuth com troca de código, e Apple envia identity token. Google foi implementado no backend em 15/09/2026, conforme [google-auth.md](google-auth.md), e ainda precisa de configuração e adaptação no app. Apple continua ausente.

## Mapa dos serviços e adaptações

| Fluxo / fonte no frontend | Situação após as correções deste trabalho | Adaptação necessária |
|---|---|---|
| `services/api/identify.ts`, `chat.ts` | Payloads, resultados, IDs e códigos de teto compatíveis | URL e token da broto-api; ajustar timeout e tratamento de erros de infraestrutura |
| `services/api/search.ts` | API já normaliza e consulta cache | Remover a leitura direta de `species_cache` e chamar somente `/v1/search` |
| `supabase/auth.ts`, `client.ts`, `store/authStore.ts`, `hooks/useAuth.ts` | Auth por e-mail existe; sessão agora contém e-mail, metadados e expiração | Substituir tipos Supabase, persistência, eventos, restauração e refresh; adaptação de retorno `{session,user}` |
| `supabase/oauth.ts`, `useAuthDeepLink.ts`, `auth-callback.tsx` | **Google implementado; Apple ausente** | Configurar o OAuth Google e adaptar app/sessão conforme `google-auth.md`; implementar Apple separadamente. Uma troca de URL não resolve |
| `supabase/profile.ts` | Campos e consentimentos cobertos por contrato | Usar perfil da sessão em `/v1/data/profiles`, sem enviar `id`/`user_id` no corpo |
| `supabase/plants.ts` | CRUD, arquivamento, eventos e tipos cobertos | Remover `user_id` e `updated_at` do corpo; datas administrativas vêm do servidor |
| `supabase/plantTasks.ts` | Agenda ativa disponível com `?active=true` | Substituir join `plants!inner`; adaptar upsert de recheck para busca + criação/edição por ID |
| `supabase/groups.ts` | CRUD e ordenação crescente disponíveis | Pedir `order=asc`; adaptar `.in()`/alterações em lote de `setGroupPlants` |
| `supabase/identifications.ts` | Vincular planta, feedback, resolver/desfazer funcionam | `resolvePlantIdentifications` exige listar as análises e atualizar por ID, ou futura rota transacional |
| `supabase/storage.ts`, `AvatarSheet.tsx` | Upload binário, assinatura de 1 h e troca de avatar cobertos | Manter compressão Expo, enviar bytes e guardar o `path` retornado pelo servidor |
| `supabase/chat.ts` | Histórico e remoção cobertos, ordenação configurável | Pedir `order=asc` nas mensagens e `limit=200`; threads permanecem decrescentes |
| `supabase/speciesFacts.ts` | **Nova leitura** `/v1/species-facts` | Retorna diretamente a ficha ou `null`, sem wrapper `{data}` |
| `supabase/reminders.ts` | Lista, contagem total e marcação coletiva cobertas | Usar `/v1/reminders/unread-count` e `/v1/reminders/read`; a contagem inclui itens fora dos 60 exibidos |
| `services/push.ts` | **Registro repetido agora é idempotente para o mesmo usuário** | POST com token/plataforma; remover `user_id`/`updated_at`; desregistrar antes do logout |
| `supabase/announcements.ts` | Apenas avisos ativos, detalhe e dispensa cobertos | GET com `limit=1`, extrair primeiro item ou `null` |
| `supabase/exportData.ts` | Dados podem ser paginados; exemplo testado com 501 eventos | Não existe exportação completa em uma rota nem assinatura em lote válida por 7 dias; exige adaptação própria |
| `services/purchases.ts` | `purchasesAvailable=false` e operações lançam `purchases_unavailable` no próprio app | Compra real está ausente também na origem; não é uma funcionalidade já pronta que a migração perdeu |

## Incompatibilidades encontradas e corrigidas

1. **Senha válida no app recusada na API.** O app exige oito caracteres, uma maiúscula Unicode e um caractere especial. A API exigia dez bytes e não verificava maiúscula/especial. Cadastro e troca de senha agora seguem a regra do app, incluindo espaços e letras acentuadas. Mantém-se o limite adicional de 72 bytes do bcrypt; o frontend precisa exibir esse máximo. Login continua aceitando credenciais antigas mais curtas quando o hash for compatível.
2. **Sessão sem e-mail/nome.** `profile`, `editProfile`, `chat` e exportação usam `user.email`/`user.user_metadata.name`. As respostas de sessão agora incluem esses campos, além de `expires_at`; hashes e tokens internos não são expostos.
3. **Push duplicado.** O app registra o mesmo token a cada abertura. Um segundo POST falhava por chave duplicada. Agora atualiza plataforma/data para o mesmo usuário; outra conta recebe 409 e não consegue tomar o token.
4. **Agenda com plantas arquivadas.** A consulta do app usa join para removê-las. Agora `/v1/data/plant_tasks?active=true` reproduz esse comportamento; a consulta sem filtro preserva tarefas arquivadas para exportação.
5. **Lembretes de plantas arquivadas e horário padrão ignorado.** A nova migration `003_frontend_reminders.sql` também corrige o scheduler: plantas arquivadas não geram lembretes; `remind_at` nulo usa `profiles.reminder_time`. Os overrides por tarefa continuam tendo prioridade.
6. **Ordem de mensagens/grupos.** A listagem genérica só retornava ordem decrescente. Agora aceita `order=asc|desc`, com desempate por chave, antes da paginação. Query strings inválidas e tentativas de injetar SQL na ordenação retornam 400.
7. **Fichas e caixa de lembretes.** Foram adicionadas leituras de fichas por espécie/idioma, contagem exata de não lidos e marcação de todos os lembretes próprios como lidos.

Os testes correspondentes foram executados antes das correções e reproduziram falhas. Depois passaram junto com a suíte anterior. As migrations `001` e `002` não foram reescritas.

## Como adaptar o frontend

### Cliente HTTP e sessão primeiro

Crie um cliente para `EXPO_PUBLIC_API_URL`. Mantenha a chave Anthropic e credenciais do PostgreSQL somente no backend. Remova o header `apikey` e `EXPO_PUBLIC_SUPABASE_ANON_KEY` desse cliente. É possível usar os aliases `/functions/v1/*` para IA ou apontar diretamente para `/v1/identify`, `/v1/chat`, `/v1/search`.

A função `signInWithEmail` atual apenas aguarda o SDK; quem atualiza o Zustand é `onAuthStateChange`. O adaptador novo deve gravar a sessão retornada, marcar hidratação e preservar a limpeza do React Query, análise e cache persistido no logout. O app também precisa verificar a sessão persistida ao reabrir e tratar 401, sem confiar apenas em `!!session` no armazenamento local.

`signUpWithEmail` deve continuar devolvendo `{session,user}` ao hook: resposta 200 da API vira `session`; resposta 202 de confirmação vira `session:null`. Caso contrário, o hook mostrará confirmação pendente mesmo quando o backend local já tiver feito login.

Guarde o token no SecureStore do celular. Na troca de token, serialize o refresh: uma chamada a `/v1/auth/refresh` invalida o token anterior, e dois refreshes simultâneos com o mesmo token não podem ambos vencer. Antes de logout, desregistre o push enquanto ainda existe sessão; hoje o listener tenta desregistrar após `SIGNED_OUT`, quando a autorização já pode ter acabado.

### Preserve as assinaturas dos serviços

É possível manter `listPlants`, `getProfile`, `uploadPhoto`, `sendMessage`, etc. e mudar seus corpos para HTTP. Isso preserva os hooks e o formato de retorno consumido pelas telas. `getProfile` continua retornando um objeto; `listPlants` uma lista; `getSpeciesFacts` uma ficha ou `null`.

Exemplos de equivalência:

```text
listPlants           GET /v1/data/plants?archived_at=null
listPlantTasks       GET /v1/data/plant_tasks?active=true
listGroups           GET /v1/data/plant_groups?order=asc
listMessages         GET /v1/data/chat_messages?thread_id=<id>&order=asc&limit=200
getSpeciesFacts      GET /v1/species-facts?scientific=<nome>&language=pt-BR
countUnreadReminders GET /v1/reminders/unread-count
markRemindersRead    POST /v1/reminders/read
registerPushToken    POST /v1/data/push_tokens
```

O `user_id` continua podendo existir como argumento local para montar query keys, mas não deve ir no corpo de gravação da API. O servidor deriva o dono da sessão. Atualização de planta também não aceita `updated_at` enviado pelo cliente.

### Paginação e operações compostas

A API limita cada página a 200 registros. `listUserCareEvents` pede 500 no app; divida isso em páginas. Na exportação, percorra todas as páginas por tabela, incluindo plantas arquivadas. O teste com 501 eventos protege contra truncamento no limite de uma página, mas o app ainda precisa implementar a coleta.

`logCareEvent` insere evento, avança tarefa e atualiza `last_watered_at`; `setGroupPlants` primeiro limpa o grupo e depois atribui os selecionados; a criação de planta pode envolver foto, análise e tarefa. Nenhum desses conjuntos de chamadas é uma transação entre requisições. Há risco de estado parcial ou duplicação em retry, inclusive no acesso Supabase original. Para robustez sob falha de rede, o próximo passo recomendado são rotas específicas transacionais e chaves de idempotência. Os testes atuais verificam os efeitos da sequência bem-sucedida e o isolamento, não prometem atomicidade da sequência inteira.

O app exporta URLs de fotos válidas por sete dias; a API fornece uma hora e não tem assinatura em lote. Uma exportação grande também pode atingir o rate limit ao assinar arquivo por arquivo. É necessário definir um fluxo de exportação/arquivo completo ou mudar explicitamente essa experiência. Não basta renomear campos.

### Erros e timeout

Os códigos de plano usados pelas telas foram preservados: análise 402/`sem_credito`, 429/`limite_mensal` ou `limite_diario`; chat 402/`no_plan`, 429/`month_cap` ou `daily_cap`; imagem ilegível 200/`foto_ilegivel`.

`useAuthErrorMessage` reconhece mensagens em inglês do Supabase; adapte para `erro` da API. O cliente também deve distinguir 429/`muitas_requisicoes` de teto de assinatura, tratar 409/`operacao_em_andamento` e 507/`armazenamento_cheio`. Hoje o hook trata qualquer 429 como teto, o que mostraria a mensagem errada para rate limit.

O Axios atual tem timeout de 120 s, enquanto análise pode usar até 150 s no servidor. Alinhe o timeout do cliente (por exemplo 180 s), o proxy e o cancelamento. Os testes comprovam rollback ao cancelar a chamada, mas não garantem reconhecimento de cancelamento após uma resposta já ter sido confirmada no banco.

## Testes entregues

Na etapa de compatibilidade foram adicionadas **15 funções de teste** com subcasos e fixtures ligadas às fontes do app, totalizando então 19 funções `Test*`. A ampliação de infraestrutura acrescentou outras 10, descritas em [testing.md](testing.md).

| Arquivo | Casos |
|---|---|
| `frontend_unit_test.go` | Senhas/Unicode/limite bcrypt, códigos consumidos pelas telas, normalização de busca e preflight CORS |
| `frontend_contract_test.go` | Campos/tipos de perfil/planta/tarefa/evento, sessão, consentimento, preferências, jardim, recheck, avatar, push, fichas, paginação, não lidos e avisos |
| `frontend_credits_test.go` | Planos grátis/Pro/chat, expiração, boas-vindas, avulsas, anúncio, tetos e virada UTC; rollback |
| `frontend_ai_test.go` | Foto → análise → planta → feedback → chat; cache por idioma, uma visão por análise, histórico em ordem, falha/cancelamento sem cobrança e scheduler alinhado à agenda |
| `api_test.go` | Suíte anterior, incluindo concorrência no último crédito, exclusão de conta, arquivos e isolamento |
| `testdata/frontend-contract.json` | Campos públicos de sete interfaces, commit e hashes das fontes inspecionadas |

Os testes usam PostgreSQL real em contêiner temporário com dados em RAM e removem o contêiner ao finalizar. Cada fixture de integração nova tem um banco exclusivo. Chamadas externas são simuladas; a suíte não envia e-mails/push reais nem gasta tokens. Não foi necessário instalar dependências Expo ou iniciar emulador.

Validação realizada:

```sh
go test ./... -count=1
go vet ./...
sh scripts/test-integration.sh -coverprofile=/tmp/broto-frontend-coverage.out
```

Resultado da etapa de compatibilidade, antes dos testes adicionais de infraestrutura: suíte completa aprovada com `-race`, **67,2% de cobertura de statements em `internal/api`** e **65,9% no projeto inteiro**, incluindo o `main`. Cobertura não equivale a garantia de estabilidade: entrega real de e-mail, login social, compra, UI/dispositivos, entrega final de push, rede real dos modelos e todos os caminhos de erro não estão cobertos. O fixture é um snapshot do commit auditado; mudanças futuras no frontend exigem revisão do contrato, não são detectadas automaticamente sem atualizar/comparar as fontes.

## Próxima sequência prática

1. Adaptar sessão/cliente HTTP e login por e-mail; integrar os serviços mantendo seus retornos.
2. Validar jardim, tarefas, foto/análise, chat e configurações em um dispositivo real e na versão web.
3. Configurar e integrar o Google OAuth já implementado, implementar Apple e adaptar deep links, além das páginas de confirmação/recuperação no `broto-web` quando disponível.
4. Fechar exportação e operações compostas/idempotentes; adicionar testes desses novos contratos.
5. Rodar a suíte em CI a cada alteração num repositório privado do backend; hoje a pasta `broto-api` ainda não é um repositório Git.

Nenhuma adaptação do app, publicação ou importação de dados de produção foi feita neste trabalho.
