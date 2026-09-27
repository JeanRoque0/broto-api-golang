# Importação Supabase → PostgreSQL nativo

Ferramenta administrativa de uso explícito. Não é executada pelo deploy. As migrations nativas devem estar aplicadas no destino, incluindo `006_import_trial_history.sql`.

A origem é somente leitura: uma transação REPEATABLE READ exporta as 16 tabelas públicas conhecidas, `auth.users`, `auth.identities` e metadados de `storage.objects`. As fotos são baixadas separadamente; antes da carga final, interrompa gravações na origem e compare novamente o snapshot. Esse export é um backup dos dados necessários à migração, não um dump completo do serviço Supabase (sessões, MFA, configurações e extensões não são restaurados).

Configure os arquivos fora do repositório, com permissão 0600 e diretório 0700. `source.json` contém `database_url`, `sslrootcert` (CA oficial Supabase), `url` (HTTPS do projeto) e `secret_key`. `target.json` usa parâmetros de conexão psycopg: `host`, `port`, `dbname`, `user`, `password`, `sslmode=verify-full`, `sslrootcert`. Para túnel SSM, use o hostname RDS em `host` e `127.0.0.1` em `hostaddr`; a verificação TLS continua usando o hostname real.

```sh
python3 -m venv /tmp/broto-migrate
/tmp/broto-migrate/bin/pip install -r scripts/migration/requirements.txt
/tmp/broto-migrate/bin/python scripts/migration/migrate.py export \
  --config /caminho/privado/source.json --directory /caminho/privado/export-novo
/tmp/broto-migrate/bin/python scripts/migration/migrate.py rehearse \
  --config /caminho/privado/test-database.json --directory /caminho/privado/export-novo
/tmp/broto-migrate/bin/python scripts/migration/migrate.py stage-s3 \
  --directory /caminho/privado/export-novo --bucket BUCKET --prefix photos --region sa-east-1
# Com gravações da origem interrompidas:
/tmp/broto-migrate/bin/python scripts/migration/migrate.py check-source \
  --config /caminho/privado/source.json --directory /caminho/privado/export-novo
/tmp/broto-migrate/bin/python scripts/migration/migrate.py import \
  --config /caminho/privado/target.json --directory /caminho/privado/export-novo --source-quiesced
```

`rehearse` executa a carga e validação completas e desfaz a transação. Prefira um PostgreSQL descartável com as mesmas migrations. `import` confirma a transação somente após comparar todas as linhas com os dados transformados. Ele exige destino vazio, inclusive sessões e filas; não faz upsert, truncate nem merge. Faça backup do destino antes de qualquer importação; se houver dados, pare e defina uma estratégia de reconciliação.

Somente os triggers que criam perfis e tarefas automaticamente são desativados dentro da transação. Constraints e triggers de propriedade continuam ativos; os dois triggers são reativados antes do commit. Falhas desfazem dados e alterações de triggers. UUIDs, hashes bcrypt, metadados, créditos, datas e consentimentos são preservados. Identidades Google usam o subject real do provedor, nunca o ID da linha. Contas sem senha mantêm um marcador não autenticável por senha. Sessões existentes não são migradas; será necessário novo login e configurar o Google OAuth no destino.

`trial_ends_at` é preservado como histórico somente leitura. O importador não concede novos benefícios de trial nem inventa assinaturas de chat ausentes na origem. Contas com MFA verificado, banimento ativo, provedor desconhecido, hash incompatível ou relacionamentos inválidos interrompem o processo para revisão.

Fotos são limitadas a 8 MiB, vinculadas ao UUID do proprietário e verificadas por SHA-256 depois de upload e download no S3. Upload condicional recusa sobrescrever objetos. Se a etapa S3 falhar parcialmente, os objetos já enviados permanecem privados. Reexecutar com o mesmo snapshot verifica os bytes dos objetos existentes e copia somente os ausentes; conteúdo divergente interrompe a operação, sem sobrescrita. Registros `stored_files` só são gravados na transação final. O snapshot local contém dados pessoais e hashes; nunca o publique no Git. Evite iniciar jobs no destino durante ensaios; depois da ativação, a retenção normal pode remover fotos antigas sem referências e históricos vencidos.

A migração do banco não troca o frontend automaticamente. A publicação do app, HTTPS, OAuth e a decisão de encerrar o Supabase são etapas distintas. Voltar ao Supabase após novas gravações no RDS exige reconciliação.

## Testes

```sh
/tmp/broto-migrate/bin/python -m unittest discover -s scripts/migration -p 'test_*.py'
BROTO_MIGRATION_PYTHON=/tmp/broto-migrate/bin/python sh scripts/test-integration.sh
```

A segunda opção executa os testes Go e do importador no mesmo PostgreSQL descartável. Verifica rollback, restauração dos triggers, preservação de dados, bloqueio de sobrescrita e constraints. Nenhuma chamada a Supabase/S3, SMTP ou IA é feita pelos testes.


## Cópia prévia sem virar o app

`copy` tem a mesma validação/transação de `import`, mas identifica explicitamente uma cópia pontual, sem afirmar que a origem foi congelada ou que houve virada. Use-a somente com o app ainda ligado ao Supabase e os jobs do destino pausados (`JOBS_ENABLED=false`; variável Terraform `jobs_enabled=false`). A origem permanece autoritativa. Novas gravações precisam ser reconciliadas antes da virada; o importador deliberadamente recusa mesclar ou sobrescrever a cópia já preenchida. A configuração normal é `JOBS_ENABLED=true`, a ser reativada após a validação e sincronização final.
