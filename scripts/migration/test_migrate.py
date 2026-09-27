import copy
import os
import pathlib
import unittest
import uuid
import tempfile
import json
import hashlib
from unittest.mock import patch

import psycopg
from psycopg import sql
import migrate

USER='00000000-0000-0000-0000-000000000001'
HASH='$2b$12$'+'A'*53
STAMP='2026-09-01T00:00:00+00:00'


def snapshot():
    return {'users':[{'id':USER,'email':'Owner@Example.invalid','encrypted_password':HASH,'created_at':STAMP,'email_confirmed_at':STAMP}],
            'identities':[], 'tables':{name:[] for name in migrate.TABLES}}


class MappingTests(unittest.TestCase):
    def test_password_and_google_subject_are_preserved(self):
        s=snapshot();s['identities']=[{'id':'internal-row-id','provider':'google','provider_id':'google-subject','identity_data':{'sub':'google-subject'},'user_id':USER,'created_at':STAMP}]
        result=migrate.transform(s,[])
        self.assertEqual(result['users'][0]['password_hash'],HASH)
        self.assertEqual(result['users'][0]['id'],USER)
        self.assertEqual(result['users'][0]['email'],'owner@example.invalid')
        self.assertEqual(result['oauth_identities'][0]['subject'],'google-subject')

    def test_passwordless_account_does_not_get_a_shared_password(self):
        s=snapshot();s['users'][0]['encrypted_password']=''
        self.assertEqual(migrate.transform(s,[])['users'][0]['password_hash'],'!supabase-provider-only')

    def test_unrecognized_password_provider_and_subject_fail(self):
        s=snapshot();s['users'][0]['encrypted_password']='plaintext'
        with self.assertRaises(ValueError):migrate.transform(s,[])
        for identity in [{'provider':'apple'}, {'provider':'google','identity_data':{}}, {'provider':'google','provider_id':'wrong','identity_data':{'sub':'actual'}}]:
            s=snapshot();s['identities']=[identity]
            with self.assertRaises(ValueError):migrate.transform(s,[])

    def test_normalization_collision_fails(self):
        s=snapshot();s['users'].append(dict(s['users'][0],id=str(uuid.uuid4()),email='owner@example.invalid'))
        with self.assertRaises(ValueError):migrate.transform(s,[])

    def test_trial_history_preserved_without_granting_chat(self):
        s=snapshot();s['tables']['profiles']=[{'id':USER,'trial_ends_at':STAMP}]
        p=migrate.transform(s,[])['profiles'][0]
        self.assertEqual(p['trial_ends_at'],STAMP)
        self.assertIsNone(p['chat_expires_at'])
        self.assertIsNone(p['chat_period'])

    def test_paths_cannot_escape_owner(self):
        self.assertEqual(migrate.safe_photo(USER+'/photo.jpg',{USER}),USER+'/photo.jpg')
        for path in ['../photo.jpg',USER+'/../photo.jpg',USER+'//photo.jpg',USER+'/x%2fy','unknown/photo.jpg']:
            with self.assertRaises(ValueError):migrate.safe_photo(path,{USER})

    def test_missing_photo_stops_import(self):
        s=snapshot();s['tables']['plants']=[{'photo_path':USER+'/missing.jpg'}]
        with self.assertRaises(ValueError):migrate.transform(s,[])

    def test_hash_is_order_independent_but_detects_changes(self):
        self.assertEqual(migrate.stable_hash([{'a':1},{'a':2}]),migrate.stable_hash([{'a':2},{'a':1}]))
        self.assertNotEqual(migrate.stable_hash([{'a':1}]),migrate.stable_hash([{'a':2}]))


class StorageTests(unittest.TestCase):
    def test_resume_verifies_existing_objects_without_upload(self):
        with tempfile.TemporaryDirectory() as d:
            root=pathlib.Path(d);(root/'objects').mkdir()
            data=b'test-photo';digest=hashlib.sha256(data).hexdigest()
            (root/'objects'/digest).write_bytes(data)
            migrate.private_write(root/'files.json',[{'path':USER+'/a.jpg','sha256':digest,'media_type':'image/jpeg'}, {'path':USER+'/b.jpg','sha256':digest,'media_type':'image/jpeg'}])
            downloads=[]
            def fake_aws(*args):
                self.assertEqual(args[1],'get-object')
                downloads.append(args[-1]);pathlib.Path(args[-1]).write_bytes(data)
                return {}
            with patch.object(migrate.subprocess,'run',return_value=type('Result',(),{'returncode':0})()),patch.object(migrate,'aws',side_effect=fake_aws):
                migrate.stage_s3(root,'bucket','photos','sa-east-1')
            self.assertEqual(len(set(downloads)),2)
            self.assertEqual(len(migrate.read(root/'s3-verified.json')['files']),2)

    def test_wrong_existing_content_cannot_get_verified_receipt(self):
        with tempfile.TemporaryDirectory() as d:
            root=pathlib.Path(d);(root/'objects').mkdir()
            data=b'expected';digest=hashlib.sha256(data).hexdigest();(root/'objects'/digest).write_bytes(data)
            migrate.private_write(root/'files.json',[{'path':USER+'/a.jpg','sha256':digest,'media_type':'image/jpeg'}])
            def fake_aws(*args):pathlib.Path(args[-1]).write_bytes(b'wrong');return {}
            with patch.object(migrate.subprocess,'run',return_value=type('Result',(),{'returncode':0})()),patch.object(migrate,'aws',side_effect=fake_aws):
                with self.assertRaises(ValueError):migrate.stage_s3(root,'bucket','photos','sa-east-1')
            self.assertFalse((root/'s3-verified.json').exists())


@unittest.skipUnless(os.getenv('MIGRATION_TEST_URL'),'Requires disposable PostgreSQL')
class DatabaseTests(unittest.TestCase):
    def setUp(self):
        self.admin=psycopg.connect(os.environ['MIGRATION_TEST_URL'],autocommit=True)
        self.name='migration_'+uuid.uuid4().hex
        self.admin.execute(sql.SQL('create database {}').format(sql.Identifier(self.name)))
        self.config=psycopg.conninfo.conninfo_to_dict(os.environ['MIGRATION_TEST_URL'])
        self.config['dbname']=self.name
        with psycopg.connect(**self.config) as db:
            for path in sorted((migrate.ROOT/'internal/api/migrations').glob('*.sql')):db.execute(path.read_text())
        with psycopg.connect(**self.config) as db:
            with db.cursor() as cur:
                cur.execute("set local timezone='UTC'")
                cur.execute('insert into users(id,email,password_hash,email_confirmed_at) values(%s,%s,%s,now())',(USER,'fixture@example.invalid',HASH))
                cur.execute("insert into stored_files(path,user_id,size,media_type) values(%s,%s,10,'image/jpeg')",(USER+'/plant.jpg',USER))
                cur.execute('insert into plants(user_id,nickname,photo_path) values(%s,%s,%s)',(USER,'Fixture',USER+'/plant.jpg'))
                self.tables={t:migrate.table_rows(cur,'public',t) for t in migrate.ORDER}
                db.rollback()

    def tearDown(self):
        self.admin.execute(sql.SQL('drop database {} with (force)').format(sql.Identifier(self.name)))
        self.admin.close()

    def test_rehearsal_commit_and_no_overwrite(self):
        migrate.import_database(self.config,self.tables,commit=False)
        with psycopg.connect(**self.config) as db:self.assertEqual(db.execute('select count(*) from users').fetchone()[0],0)
        migrate.import_database(self.config,self.tables,commit=True)
        with psycopg.connect(**self.config) as db:
            with db.cursor() as cur:migrate.verify_database(cur,self.tables)
        with self.assertRaises(ValueError):migrate.import_database(self.config,self.tables,commit=True)

    def test_relationship_failure_rolls_back_and_restores_triggers(self):
        tables=copy.deepcopy(self.tables)
        tables['plants'][0]['photo_path']=USER+'/absent.jpg'
        with self.assertRaises(psycopg.Error):migrate.import_database(self.config,tables,commit=True)
        with psycopg.connect(**self.config) as db:
            self.assertEqual(db.execute('select count(*) from users').fetchone()[0],0)
            self.assertEqual(db.execute("select count(*) from pg_trigger where tgname in ('on_auth_user_created','seed_plant_tasks_trigger') and tgenabled='O'").fetchone()[0],2)

if __name__=='__main__': unittest.main()
