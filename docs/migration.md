# Migração Supabase → broto-api

## O que está pronto e o que depende do ambiente

A implementação local porta os arquivos existentes em `broto-supabase`: banco (schema + legacy + migrations), funções de IA, cadastro/sessões, fotos e jobs. Ela inicia um **banco novo**, não se conecta ao projeto hospedado e não importa dados automaticamente.

O `broto-app` foi posteriormente clonado e comparado com a API; veja [o relatório de compatibilidade](frontend-compatibility.md). Suas chamadas ainda não foram substituídas. O `broto-web` continua ausente. Não foram fornecidos acesso ao banco de produção ou arquivos do bucket. SMTP e Anthropic foram configurados posteriormente no `.env` privado; veja o resultado dos testes reais em [testing.md](testing.md). Isso não impede executar e testar a infraestrutura local.

## Mapeamento

| Antes | Depois |
|---|---|
| `supabase.auth.*` / GoTrue | `/v1/auth/*`, tokens opacos da broto-api |
| `supabase.from('plants')...` / PostgREST + RLS | `/v1/data/plants...`, campos permitidos + isolamento no servidor e constraints no banco |
| `storage.from('plant-photos')` | `/v1/photos`, volume privado e URLs HMAC |
| Edge Functions `identify/chat/search/delete-account` | Go, com aliases `/functions/v1/*` |
| RPC de créditos | Funções PostgreSQL internas, sem acesso pelo cliente |
| pg_cron, pg_net, Vault | Scheduler Go, com locks entre instâncias |
| `auth.users` | `public.users` |
| Supabase SMTP | SMTP próprio com STARTTLS |

Não basta mudar `SUPABASE_URL`: o SDK usa contratos próprios para autenticação, PostgREST e storage. Substitua essas operações pelo [contrato HTTP](api.md). Sessões antigas do Supabase não são aceitas; planeje novo login. O frontend usa Google/Apple e troca de código. Google foi implementado na API, mas precisa da configuração do provedor e da adaptação do app: veja [google-auth.md](google-auth.md). Apple ainda não está implementado. MFA, Realtime e webhooks de pagamento também não foram implementados como equivalência ao Supabase inteiro.

## Banco novo (sem usuários em produção)

1. Configure `.env` com `python3 scripts/setup_env.py` e execute o Compose.
2. Teste cadastro/login, plantas/tarefas, fotos e edição no novo app.
3. Configure a chave Anthropic e um modelo de visão autorizado. Teste uma foto legível, uma ilegível, chat Pro e busca.
4. Configure SMTP, desative `DEV_AUTO_CONFIRM` e adapte `/confirmado` e `/nova-senha` no site.
5. Configure o domínio HTTPS e os endereços de API/site/CORS. Em dispositivo físico, `localhost` representa o celular; use um endpoint alcançável e ajuste o bind do Compose/proxy para o ambiente escolhido.

## Se já existe produção

A importação precisa ser planejada contra o estado real do projeto. Antes de executá-la, forneça o caminho dos frontends e confirme se existem dados a preservar. Configure credenciais em arquivo local ignorado ou gerenciador de secrets, nunca no chat/repositório.

São necessários:

- Acesso de leitura PostgreSQL à origem (para `public`, `auth.users` e metadados de storage) ou exportações equivalentes. Uma anon key não permite exportar todos os usuários/dados.
- Acesso aos objetos privados `plant-photos` (exportação ou service-role **somente no processo de migração**).
- Contagem e tamanho total dos dados/fotos, para reservar espaço no destino. A quota padrão de 512 MB deve ser ajustada se a origem for maior.

Roteiro de transferência para uma janela de manutenção:

1. Faça backup da origem e registre contagens, hashes dos arquivos e versão aplicada do schema. Não execute os scripts antigos de reset/truncate.
2. Crie e valide um destino separado. Pare gravações no app antigo durante a cópia final para evitar divergência.
3. Migre `auth.users` para `users`, preservando `id`, e-mail normalizado, `email_confirmed_at`, `raw_user_meta_data`, `created_at` e, quando existir e for bcrypt compatível, `encrypted_password` → `password_hash`. Usuários sem senha utilizável exigem recuperação. Senhas novas exigem 8 caracteres, maiúscula e especial, até 72 bytes; login aceita senhas bcrypt antigas mais curtas, até 72 bytes. Sessões e tokens de recuperação antigos não são importados. Para contas Google, importe também o subject real do provedor em `oauth_identities`, associado ao mesmo `users.id`, conforme `google-auth.md`. O backend não associa identidades apenas por e-mail.
4. Ao importar, suspenda temporariamente **apenas** os triggers `on_auth_user_created` em `users` e `seed_plant_tasks_trigger` em `plants`, dentro da transação de carga, para não gerar perfis/tarefas duplicados. Mantenha constraints de propriedade ativas.
5. Copie primeiro os usuários, depois bytes das fotos e registros `stored_files` (path, dono, tamanho, MIME e data), perfis, grupos, plantas, tarefas/eventos/análises, threads/mensagens e demais tabelas. Preserve UUIDs, timestamps, créditos, planos e consentimentos. Reative os triggers antes do commit.
6. Valide contagens, referências, hashes e ausência de vínculos entre contas. Compare o schema real com a baseline local: `schema.sql` sozinho não representa produção. Não aplique um dump completo de Supabase sobre o banco nativo — ele carrega schemas/extensões/roles específicos.
7. Faça um ensaio no app/site apontando para o destino, com contas de teste e confirmação de e-mail. Verifique lembretes e consumo/reembolso com a chave real.
8. Publique a versão do app que usa a broto-api e só então retire tráfego do Supabase. Guarde o backup e a origem durante o período de rollback; voltar após novas gravações exige reconciliar dados.

Nenhuma transferência de produção ou alteração no projeto Supabase foi realizada aqui. O roteiro acima depende da inspeção da origem; não há script que pressuponha silenciosamente compatibilidade do dump.

## Diferenças deliberadas e limites

- O SQL mais recente tem crédito avulso antes de Pro/cota grátis; comentários antigos descrevem outra ordem. A implementação segue as migrations.
- A verificação de assinatura de chat foi corrigida para tratar datas nulas como ausência de plano. O `IF NOT (...)` original podia avaliar SQL NULL e liberar acesso indevido.
- Falhas de análise/chat desfazem a transação de consumo; não dependem de uma segunda requisição de reembolso. Isso mantém uma conexão PostgreSQL durante a chamada da IA; o pool tem 10 conexões e uma operação de análise/chat por usuário por vez.
- Fotos têm constraints de referência e propriedade. A fila de exclusão elimina arquivos somente depois da exclusão relacional confirmada; falhas de disco são retomadas pelo scheduler. Uma exclusão extensa pode terminar a limpeza de fotos em lotes subsequentes.
- Lembrete só entra no histórico depois de aceito pelo Expo. Tickets aceitos não são comprovantes de entrega ao aparelho; receipts do Expo não são consultados. Uma queda entre envio e commit pode causar repetição no retry, pois a API de push não oferece transação com o PostgreSQL.
- O scheduler usa a periodicidade horária das funções antigas e não executa trabalhos enquanto a API está parada. Não há recuperação retroativa de todas as horas perdidas. O Compose deve usar uma instância de API e o volume local correspondente; múltiplas máquinas exigem armazenamento compartilhado.
- O rate limit de HTTP fica em memória por instância, por usuário ou IP direto. Não confia em `X-Forwarded-For` enviado pelo cliente; ajuste proxy confiável e proteção de borda ao publicar.
- O campo `cost_micros` mantém a tabela de preços presente na origem. Modelo não listado fica com custo zero (não precificado); atualize-a com os preços da sua conta antes de usar esse campo para relatórios financeiros.
- Cache de busca mantém o orçamento global de 1.000 chamadas mensais da origem. É uma limitação compartilhada por todos os usuários.
