#!/usr/bin/env python3
"""Explicit, insert-only Supabase data transfer. Credentials and snapshots stay outside Git."""
import argparse
import base64
import datetime
import hashlib
import json
import os
import pathlib
import re
import subprocess
import tempfile
import urllib.parse
import urllib.request
from concurrent.futures import ThreadPoolExecutor

import psycopg
from psycopg import sql
from psycopg.types.json import Jsonb

TABLES = ['profiles','plant_groups','plants','plant_tasks','care_events','identifications',
          'chat_threads','chat_messages','push_tokens','reminder_events','announcements',
          'ad_rewards','credit_purchases','search_budget','species_cache','species_facts']
ORDER = ['users','oauth_identities','stored_files'] + TABLES
ROOT = pathlib.Path(__file__).resolve().parents[2]


def read(path):
    return json.loads(pathlib.Path(path).read_text())


def private_write(path, data):
    path=pathlib.Path(path)
    path.parent.mkdir(parents=True,exist_ok=True,mode=0o700)
    with os.fdopen(os.open(path,os.O_WRONLY|os.O_CREAT|os.O_TRUNC,0o600),'w') as f:
        json.dump(data,f,ensure_ascii=False,sort_keys=True,indent=2)
        f.write('\n')
    path.chmod(0o600)


def source_connect(config):
    # Require an independently obtained CA, never trust a certificate from the
    # unauthenticated database handshake or silently downgrade TLS.
    return psycopg.connect(config['database_url'], sslmode='verify-full',
                          sslrootcert=config['sslrootcert'], connect_timeout=20)


def table_rows(cur,schema,table):
    cur.execute(sql.SQL('select to_jsonb(t) from {}.{} t').format(sql.Identifier(schema),sql.Identifier(table)))
    return [r[0] for r in cur.fetchall()]


def stable_hash(rows):
    items=sorted(json.dumps(r,sort_keys=True,separators=(',',':'),ensure_ascii=False) for r in rows)
    return hashlib.sha256('\n'.join(items).encode()).hexdigest()


def capture(config):
    with source_connect(config) as db:
        with db.cursor() as cur:
            cur.execute('set transaction isolation level repeatable read read only')
            cur.execute("set local timezone='UTC'")
            cur.execute("select tablename from pg_tables where schemaname='public'")
            if {r[0] for r in cur.fetchall()} != set(TABLES):
                raise ValueError('Unexpected source tables: review schema before exporting')
            cur.execute("select count(*) from auth.mfa_factors where status='verified'")
            if cur.fetchone()[0]: raise ValueError('MFA accounts require an explicit migration strategy')
            cur.execute('select now()::text,pg_database_size(current_database())')
            timestamp,size=cur.fetchone()
            tables={t:table_rows(cur,'public',t) for t in TABLES}
            return {'format':1,'captured_at':timestamp,'database_bytes':size,'tables':tables,
                    'users':table_rows(cur,'auth','users'),
                    'identities':table_rows(cur,'auth','identities'),
                    'objects':table_rows(cur,'storage','objects')}


def safe_photo(name,users):
    if not isinstance(name,str) or len(name)>512 or not re.fullmatch(r'[a-zA-Z0-9_./-]+',name):
        raise ValueError('Unsupported photo path')
    if '..' in name or '//' in name or name.endswith('/') or name.split('/')[0] not in users:
        raise ValueError('Invalid photo owner or path')
    return name


def download(config,snapshot,directory):
    directory=pathlib.Path(directory)
    users={r['id'] for r in snapshot['users']}
    manifest=[]
    for item in snapshot['objects']:
        if item['bucket_id']!='plant-photos': raise ValueError('Unexpected storage bucket')
        name=safe_photo(item['name'],users)
        owner=item.get('owner_id') or item.get('owner')
        if owner and str(owner)!=name.split('/')[0]: raise ValueError('Storage owner/path mismatch')
        expected=int(item.get('metadata',{}).get('size',0))
        if expected<=0 or expected>8*1024*1024: raise ValueError('Photo size incompatible with backend')
        url=config['url']+'/storage/v1/object/authenticated/plant-photos/'+urllib.parse.quote(name,safe='/')
        req=urllib.request.Request(url,headers={'apikey':config['secret_key']})
        with urllib.request.urlopen(req,timeout=60) as response:
            data=response.read(8*1024*1024+1)
            mime=response.headers.get_content_type()
        if len(data)!=expected: raise ValueError('Photo changed during export or invalid size')
        if mime not in ['image/jpeg','image/png','image/webp']: raise ValueError('Unsupported photo MIME')
        digest=hashlib.sha256(data).hexdigest()
        file=directory/'objects'/digest
        file.parent.mkdir(mode=0o700,exist_ok=True)
        with os.fdopen(os.open(file,os.O_WRONLY|os.O_CREAT|os.O_TRUNC,0o600),'wb') as f: f.write(data)
        manifest.append({'path':name,'user_id':name.split('/')[0],'size':len(data),'media_type':mime,
                         'created_at':item['created_at'],'sha256':digest})
    return manifest


def transform(snapshot,manifest):
    users=[]
    for u in snapshot['users']:
        if u.get('deleted_at') or u.get('is_anonymous') or not u.get('email'):
            raise ValueError('Deleted, anonymous or email-less identity needs manual review')
        if u.get('banned_until') and datetime.datetime.fromisoformat(u['banned_until'])>datetime.datetime.now(datetime.timezone.utc):
            raise ValueError('Banned identity needs manual review')
        password=u.get('encrypted_password') or '!supabase-provider-only'
        if password!='!supabase-provider-only' and not re.fullmatch(r'\$2[aby]\$\d\d\$[./A-Za-z0-9]{53}',password):
            raise ValueError('Unsupported password hash')
        users.append({'id':u['id'],'email':u['email'].strip().lower(),'password_hash':password,
                      'email_confirmed_at':u.get('email_confirmed_at'),'raw_user_meta_data':u.get('raw_user_meta_data') or {},'created_at':u['created_at']})
    if len({u['email'] for u in users})!=len(users): raise ValueError('Duplicate normalized email')
    identities=[]
    for identity in snapshot['identities']:
        provider=identity['provider']
        if provider=='email': continue
        if provider!='google': raise ValueError('Unsupported identity provider')
        subject=identity.get('identity_data',{}).get('sub')
        if not subject or (identity.get('provider_id') and identity['provider_id']!=subject):
            raise ValueError('Google subject missing or inconsistent')
        identities.append({'provider':'google','subject':subject,'user_id':identity['user_id'],'created_at':identity['created_at']})
    tables={k:[dict(r) for r in v] for k,v in snapshot['tables'].items()}
    for p in tables['profiles']:
        if p.get('trial_ends_at') and datetime.datetime.fromisoformat(p['trial_ends_at'])>datetime.datetime.now(datetime.timezone.utc):
            raise ValueError('Active trial requires an explicit entitlement mapping')
        p['chat_expires_at']=None
        p['chat_period']=None
    tables.update(users=users,oauth_identities=identities,
                  stored_files=[{k:v for k,v in f.items() if k!='sha256'} for f in manifest])
    if set(tables)!=set(ORDER): raise ValueError('Unexpected snapshot tables')
    for table,rows in tables.items():
        for row in rows:
            if table in ['profiles','plants','identifications']:
                field='avatar_path' if table=='profiles' else 'photo_path'
                name=row.get(field)
                if name is not None and name not in {f['path'] for f in manifest}:
                    raise ValueError('Referenced photo is absent from Storage')
    return tables


def verify_database(cur,tables):
    for table,expected in tables.items():
        actual=table_rows(cur,'public',table)
        if stable_hash(actual)!=stable_hash(expected):
            raise ValueError('Imported data differs: '+table)


def import_database(config,tables,commit=False):
    with psycopg.connect(**config) as db:
        with db.cursor() as cur:
            cur.execute("set local timezone='UTC'; set local lock_timeout='10s'; set local statement_timeout='120s'")
            cur.execute('select pg_advisory_xact_lock(730299)')
            # Reject existing application data; never silently merge or overwrite.
            cur.execute(sql.SQL('lock table {} in access exclusive mode').format(sql.SQL(',').join(sql.Identifier(t) for t in ORDER)))
            for table in ORDER+['sessions','file_deletions','auth_tokens','oauth_codes','oauth_attempts','auth_email_sends']:
                cur.execute(sql.SQL('select count(*) from {}').format(sql.Identifier(table)))
                if cur.fetchone()[0]: raise ValueError('Destination is not empty: '+table)
            cur.execute('alter table users disable trigger on_auth_user_created')
            cur.execute('alter table plants disable trigger seed_plant_tasks_trigger')
            for table in ORDER:
                cur.execute("select column_name from information_schema.columns where table_schema='public' and table_name=%s",(table,))
                columns={r[0] for r in cur.fetchall()}
                for row in tables[table]:
                    if set(row)!=columns: raise ValueError('Schema mismatch for '+table)
                names=sorted(columns)
                query=sql.SQL('insert into {} ({}) select {} from jsonb_populate_recordset(null::{}, %s)').format(sql.Identifier(table),sql.SQL(',').join(map(sql.Identifier,names)),sql.SQL(',').join(map(sql.Identifier,names)),sql.Identifier(table))
                cur.execute(query,(Jsonb(tables[table]),))
            cur.execute('alter table users enable trigger on_auth_user_created')
            cur.execute('alter table plants enable trigger seed_plant_tasks_trigger')
            verify_database(cur,tables)
            cur.execute("select count(*) from pg_trigger where tgname in ('on_auth_user_created','seed_plant_tasks_trigger') and tgenabled='O'")
            if cur.fetchone()[0]!=2: raise ValueError('Triggers were not restored')
            if not commit: db.rollback()


def aws(*args):
    r=subprocess.run(['aws',*args,'--output','json'],capture_output=True,text=True)
    if r.returncode: raise RuntimeError('AWS operation failed: '+args[0]+' '+args[1])
    return json.loads(r.stdout) if r.stdout.strip() else {}


def stage_s3(directory,bucket,prefix,region):
    directory=pathlib.Path(directory);manifest=read(directory/'files.json')
    def transfer(item):
        file=directory/'objects'/item['sha256'];data=file.read_bytes()
        if hashlib.sha256(data).hexdigest()!=item['sha256']: raise ValueError('Local photo checksum mismatch')
        key=prefix.rstrip('/')+'/'+item['path']
        head=subprocess.run(['aws','s3api','head-object','--region',region,'--bucket',bucket,'--key',key,'--output','json'],capture_output=True,text=True)
        if head.returncode:
            if not any(code in head.stderr for code in ['(404)','(NoSuchKey)','(NotFound)']):
                raise RuntimeError('Cannot inspect destination object')
            # Conditional create refuses races with another writer. Resuming
            # verifies existing bytes below instead of overwriting them.
            aws('s3api','put-object','--region',region,'--bucket',bucket,'--key',key,'--body',str(file),
                '--if-none-match','*','--content-type',item['media_type'],'--server-side-encryption','AES256',
                '--checksum-algorithm','SHA256','--checksum-sha256',base64.b64encode(bytes.fromhex(item['sha256'])).decode())
        descriptor,temporary=tempfile.mkstemp(prefix='s3-verify-',dir=directory)
        os.close(descriptor)
        check=pathlib.Path(temporary)
        try:
            aws('s3api','get-object','--region',region,'--bucket',bucket,'--key',key,str(check))
            if hashlib.sha256(check.read_bytes()).hexdigest()!=item['sha256']: raise ValueError('S3 photo checksum mismatch')
        finally: check.unlink(missing_ok=True)
        return {'path':item['path'],'sha256':item['sha256']}
    with ThreadPoolExecutor(max_workers=4) as pool: verified=list(pool.map(transfer,manifest))
    private_write(directory/'s3-verified.json',{'bucket':bucket,'prefix':prefix,'region':region,'files':verified})


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action',choices=['export','check-source','rehearse','import','copy','stage-s3'])
    parser.add_argument('--config',help='Private source or target JSON configuration file')
    parser.add_argument('--directory',required=True)
    parser.add_argument('--bucket');parser.add_argument('--prefix',default='photos');parser.add_argument('--region',default='sa-east-1')
    parser.add_argument('--source-quiesced',action='store_true',help='Operator confirms writes are stopped and source check completed')
    args=parser.parse_args();os.umask(0o077);directory=pathlib.Path(args.directory)
    if args.action=='export':
        if directory.exists(): raise ValueError('Choose a new export directory')
        directory.mkdir(parents=True,mode=0o700)
        config=read(args.config);snapshot=capture(config)
        private_write(directory/'snapshot.json',snapshot)
        manifest=download(config,snapshot,directory)
        private_write(directory/'files.json',manifest)
        transform(snapshot,manifest)
        print(json.dumps({'tables':{k:len(v) for k,v in snapshot['tables'].items()},'users':len(snapshot['users']),'files':len(manifest),'bytes':sum(f['size'] for f in manifest)}))
    elif args.action=='check-source':
        current=capture(read(args.config));old=read(directory/'snapshot.json')
        for section in ['users','identities','objects']:
            if stable_hash(current[section])!=stable_hash(old[section]): raise ValueError('Source changed: '+section)
        for table in TABLES:
            if stable_hash(current['tables'][table])!=stable_hash(old['tables'][table]): raise ValueError('Source changed: '+table)
        print('Source matches snapshot; final cutover still requires quiesced writers.')
    elif args.action=='stage-s3':
        stage_s3(directory,args.bucket,args.prefix,args.region);print('All S3 files verified by SHA-256')
    else:
        snapshot=read(directory/'snapshot.json');manifest=read(directory/'files.json')
        if args.action in ['import','copy']:
            if args.action=='import' and not args.source_quiesced: raise ValueError('Final import requires --source-quiesced')
            verified=read(directory/'s3-verified.json')
            if {(v['path'],v['sha256']) for v in verified['files']}!={(v['path'],v['sha256']) for v in manifest}:
                raise ValueError('S3 manifest does not match export')
        import_database(read(args.config),transform(snapshot,manifest),commit=args.action in ['import','copy'])
        print('Snapshot copy committed and verified; source remains authoritative, no cutover' if args.action=='copy' else 'Committed and verified' if args.action=='import' else 'Rehearsal passed; transaction rolled back')

if __name__=='__main__':
    try: main()
    except Exception as error:
        # Database/provider messages can contain user records or secret URLs.
        if isinstance(error,ValueError): print(str(error))
        else: print('Migration stopped safely:',type(error).__name__)
        raise SystemExit(1)
