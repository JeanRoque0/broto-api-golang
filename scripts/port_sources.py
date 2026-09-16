"""Reproduce the initial native SQL and AI assets from the sibling source repo.
Run only when intentionally rebasing the initial migration (never after deployment).
"""
import pathlib,re,json
src=pathlib.Path(__file__).resolve().parents[2]/'broto-supabase'
dst=pathlib.Path(__file__).resolve().parents[1]/'internal/api'

def statements(sql):
    # SQL tokenization: preserve quoted strings and dollar-quoted function bodies.
    pattern=r"(--[^\n]*|/\*[\s\S]*?\*/|'(?:''|[^'])*'|\"(?:\"\"|[^\"])*\"|\$\w*\$[\s\S]*?\$\w*\$|;)"
    buf=''
    for part in re.split(pattern,sql):
        if part.startswith('--') or part.startswith('/*'):continue
        buf+=part
        if part==';':
            yield buf.strip();buf=''

files=[src/'schema.sql']+sorted(p for p in (src/'legacy').glob('*.sql') if not p.name.startswith('schedule'))+sorted(p for p in (src/'supabase/migrations').glob('*.sql') if 'species_seed' not in p.name)
parts=['''-- Native PostgreSQL baseline, derived from schema + legacy + migrations.
create table users (
 id uuid primary key default gen_random_uuid(), email text not null unique,
 password_hash text not null, email_confirmed_at timestamptz,
 raw_user_meta_data jsonb not null default '{}', created_at timestamptz not null default now()
);
create table sessions (
 token_hash text primary key, user_id uuid not null references users on delete cascade,
 expires_at timestamptz not null, created_at timestamptz not null default now()
);
create index sessions_user_idx on sessions(user_id);
create table auth_tokens (
 token_hash text primary key, user_id uuid not null references users on delete cascade,
 kind text not null check(kind in ('email','recovery')), expires_at timestamptz not null
);
''']
for p in files:
    out=[]
    for s in statements(p.read_text()):
        lo=s.lower()
        if re.match(r'(create|drop) policy|grant |revoke ',lo):continue
        if 'enable row level security' in lo:continue
        if any(x in lo for x in ['storage.', 'cron.', 'vault.', 'create extension']):continue
        s=s.replace("if not (\n    (prof.plan = 'pro' and prof.plan_expires_at > now())\n    or prof.chat_expires_at > now()\n  ) then", "if not coalesce((prof.plan = 'pro' and prof.plan_expires_at > now())\n    or prof.chat_expires_at > now(), false) then")
        out.append(s.replace('auth.users','users'))
    if out:parts.append('-- Source: '+str(p.relative_to(src))+'\n'+'\n\n'.join(out))
(dst/'migrations/001_initial.sql').write_text('\n\n'.join(parts))
for file,names in [('identify/prompt.ts',['SYSTEM_PROMPT','RESULT_SCHEMA']),('identify/species.ts',['SPECIES_PROMPT','SPECIES_SCHEMA','CONFIRM_PROMPT','CONFIRM_SCHEMA']),('chat/prompt.ts',['SYSTEM'])]:
    text=(src/'supabase/functions'/file).read_text()
    for name in names:
        if name.endswith('SCHEMA'):
            obj=re.search(r'export const '+name+r' = (\{[\s\S]*?\}) as const;',text)[1]
            obj=re.sub(r'\b([a-zA-Z_][a-zA-Z_0-9]*):',r'"\1":',obj)
            obj=re.sub(r',\s*([}\]])',r'\1',obj)
            # TS permits adjacent line breaks after a colon, JSON also does.
            (dst/'assets'/f'{name}.json').write_text(json.dumps(json.loads(obj),ensure_ascii=False,indent=2))
        else:
            value=re.search(r'export const '+name+r' = `([\s\S]*?)`;',text)[1]
            (dst/'assets'/f'{name}.txt').write_text(value)
print('Ported SQL and original prompts/schemas')
