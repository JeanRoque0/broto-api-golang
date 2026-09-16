# Broto API

Backend privado em Go. Prompts, regras de créditos e economia pertencem a este backend; não copiar para repositório público do frontend. Nunca commitar `.env`, dumps ou fotos.

`internal/api/migrations` é a fonte do banco nativo. Migrações aplicadas são imutáveis: adicione novo arquivo. `001_initial.sql` foi derivado de `../broto-supabase`; `scripts/port_sources.py` é ferramenta de reprodução da baseline, não deve ser executada para atualizar um banco já publicado. Não execute scripts antigos de reset/truncate.

Preserve as regras dos prompts: sem dosagem de defensivos, sem afirmações de comestibilidade, toxicidade com ressalva veterinária na interface, diagnóstico diferencial ranqueado, uma chamada de visão por análise. Falhas de IA/fotos ilegíveis não consomem créditos.

Validação: `go test ./...`, `go vet ./...`, `sh scripts/test-integration.sh`. O último usa PostgreSQL temporário em RAM e mocks dos provedores externos; não faça chamadas pagas ou envie notificações/e-mails reais durante testes sem autorização explícita.

Há restrição de espaço na máquina. Evite stacks adicionais, volumes de teste persistentes e limpezas globais de Docker/cache de outros projetos. Compose mantém apenas API e PostgreSQL. A integração dos frontends depende de seus repositórios; não afirme que apenas mudar a URL do Supabase substitui o SDK.
