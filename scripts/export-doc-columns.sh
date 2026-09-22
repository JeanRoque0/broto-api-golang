#!/bin/sh
# Read-only metadata export from the locally migrated Compose database.
set -eu
cd "$(dirname "$0")/.."
docker compose exec -T db psql -U broto -d broto -Atc "select jsonb_object_agg(table_name, cols) from (select table_name,jsonb_object_agg(column_name,jsonb_build_object('type',data_type,'nullable',is_nullable='YES','default',column_default is not null,'udt',udt_name)) cols from information_schema.columns where table_schema='public' and table_name in ('profiles','plants','plant_groups','plant_tasks','care_events','identifications','chat_threads','chat_messages','push_tokens','reminder_events','announcements','ad_rewards','credit_purchases') group by table_name) t" | python3 -c '
import json, pathlib, sys
value=json.load(sys.stdin)
if not isinstance(value,dict) or len(value)!=13:
    sys.exit("Expected all 13 public resources; apply migrations first")
pathlib.Path("docs/data-columns.json").write_text(json.dumps(value,ensure_ascii=False,indent=2)+"\n")
'
