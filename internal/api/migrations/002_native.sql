-- Reject relationships that would cross account boundaries, including server inserts.
create function check_owner_links() returns trigger language plpgsql as $$
declare target uuid;
begin
 if to_jsonb(new) ? 'plant_id' and (to_jsonb(new)->>'plant_id') is not null then
  select user_id into target from plants where id=(to_jsonb(new)->>'plant_id')::uuid;
  if target is distinct from new.user_id then raise exception 'invalid plant owner' using errcode='23514'; end if;
 end if;
 if to_jsonb(new) ? 'group_id' and (to_jsonb(new)->>'group_id') is not null then
  select user_id into target from plant_groups where id=(to_jsonb(new)->>'group_id')::uuid;
  if target is distinct from new.user_id then raise exception 'invalid group owner' using errcode='23514'; end if;
 end if;
 if to_jsonb(new) ? 'thread_id' then
  select user_id into target from chat_threads where id=new.thread_id;
  if target is distinct from new.user_id then raise exception 'invalid thread owner' using errcode='23514'; end if;
 end if;
 return new;
end $$;
do $$ declare t text; begin
 foreach t in array array['plants','plant_tasks','care_events','identifications','chat_threads','chat_messages','reminder_events'] loop
 execute format('create trigger owner_links before insert or update on %I for each row execute function check_owner_links()',t);
 end loop;
end $$;
alter table plant_tasks add constraint task_positive_interval check(interval_days > 0);
alter table plants add constraint valid_prune_month check(prune_month between 1 and 12);
create table stored_files (
 path text primary key, user_id uuid not null references users on delete cascade,
 size bigint not null check(size > 0), media_type text not null, created_at timestamptz not null default now()
);
create index stored_files_user_idx on stored_files(user_id);
create table job_runs (name text primary key, completed_at timestamptz not null);
-- No Supabase roles exist. Only the backend database login can access tables.
revoke execute on all functions in schema public from public;

-- Durable file cleanup: commit the relational deletion before removing bytes.
create table file_deletions (path text primary key, queued_at timestamptz not null default now());
create function queue_file_deletion() returns trigger language plpgsql as $$
begin
 insert into file_deletions(path) values(old.path) on conflict do nothing;
 return old;
end $$;
create trigger queue_file_deletion after delete on stored_files for each row execute function queue_file_deletion();

-- Native FK constraints also prevent delete/attach races for private photos.
alter table plants add constraint plant_photo_fk foreign key(photo_path) references stored_files(path);
alter table profiles add constraint avatar_photo_fk foreign key(avatar_path) references stored_files(path);
alter table identifications add constraint identification_photo_fk foreign key(photo_path) references stored_files(path);
create function check_photo_owner() returns trigger language plpgsql as $$
declare photo text; owner_id uuid; expected uuid;
begin
 if tg_table_name='profiles' then photo:=new.avatar_path; expected:=new.id;
 else photo:=new.photo_path; expected:=new.user_id; end if;
 if photo is not null then
  select user_id into owner_id from stored_files where path=photo;
  if owner_id is distinct from expected then raise exception 'invalid photo owner' using errcode='23514'; end if;
 end if;
 return new;
end $$;
create trigger check_photo_owner before insert or update on profiles for each row execute function check_photo_owner();
create trigger check_photo_owner before insert or update on plants for each row execute function check_photo_owner();
create trigger check_photo_owner before insert or update on identifications for each row execute function check_photo_owner();
revoke execute on all functions in schema public from public;
