-- Native PostgreSQL baseline, derived from schema + legacy + migrations.
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


-- Source: schema.sql
create type plan_tier as enum ('free', 'pro');

create table profiles (
  id                  uuid primary key references users(id) on delete cascade,
  display_name        text,
  plan                plan_tier   not null default 'free',
  plan_expires_at     timestamptz,

  free_used           int         not null default 0,
  period_start        date        not null default date_trunc('month', now())::date,

  ad_credits          int         not null default 0,
  ads_today           int         not null default 0,
  ads_today_date      date        not null default current_date,

  accepted_terms_at     timestamptz,
  terms_version         text,
  accepted_tips         boolean     not null default false,
  accepted_tips_at      timestamptz,

  timezone              text,
  reminder_time         time        not null default '09:00',
  notifications_enabled boolean     not null default true,

  created_at          timestamptz not null default now()
);

create function handle_new_user()
returns trigger
language plpgsql
security definer set search_path = public
as $$
begin
  insert into profiles (id, display_name)
  values (new.id, new.raw_user_meta_data->>'name');
  return new;
end;
$$;

create trigger on_auth_user_created
  after insert on users
  for each row execute function handle_new_user();

create table plants (
  id                    uuid primary key default gen_random_uuid(),
  user_id               uuid not null references users(id) on delete cascade,

  nickname              text not null,
  species_scientific    text,
  species_common        text,
  photo_path            text,
  room                  text,

  watering_interval_days int,
  light                 text,
  care_notes            text,

  last_watered_at       timestamptz,
  notify_watering       boolean not null default true,

  archived_at           timestamptz,
  created_at            timestamptz not null default now(),
  updated_at            timestamptz not null default now()
);

create index plants_user_idx on plants (user_id) where archived_at is null;

create table care_events (
  id          uuid primary key default gen_random_uuid(),
  plant_id    uuid not null references plants(id) on delete cascade,
  user_id     uuid not null references users(id) on delete cascade,
  kind        text not null,
  note        text,
  happened_at timestamptz not null default now()
);

create index care_events_plant_idx on care_events (plant_id, happened_at desc);

create table identifications (
  id                uuid primary key default gen_random_uuid(),
  user_id           uuid not null references users(id) on delete cascade,
  plant_id          uuid references plants(id) on delete set null,

  kind              text not null,
  photo_path        text not null,
  result            jsonb not null,
  confidence        numeric(3,2),

  corrected_species text,
  was_helpful       boolean,

  model             text,
  cost_micros       int,
  created_at        timestamptz not null default now()
);

create index ident_user_idx on identifications (user_id, created_at desc);

create table ad_rewards (
  transaction_id text primary key,
  user_id        uuid not null references users(id) on delete cascade,
  ad_unit        text,
  credited_at    timestamptz not null default now()
);

create index ad_rewards_user_idx on ad_rewards (user_id, credited_at desc);

create function roll_periods(p_user uuid)
returns void
language plpgsql
security definer set search_path = public
as $$
begin
  update profiles set
    free_used      = case when period_start < date_trunc('month', now())::date
                          then 0 else free_used end,
    period_start   = greatest(period_start, date_trunc('month', now())::date),
    ads_today      = case when ads_today_date < current_date then 0 else ads_today end,
    ads_today_date = greatest(ads_today_date, current_date)
  where id = p_user;
end;
$$;

create function consume_credit(p_user uuid, p_free_quota int default 3)
returns jsonb
language plpgsql
security definer set search_path = public
as $$
declare
  prof profiles;
begin
  perform roll_periods(p_user);
  select * into prof from profiles where id = p_user for update;

  if prof is null then
    return jsonb_build_object('ok', false, 'reason', 'no_profile');
  end if;

  if prof.plan = 'pro' and prof.plan_expires_at > now() then
    return jsonb_build_object('ok', true, 'source', 'pro');
  end if;

  if prof.free_used < p_free_quota then
    update profiles set free_used = free_used + 1 where id = p_user;
    return jsonb_build_object('ok', true, 'source', 'free',
                              'remaining', p_free_quota - prof.free_used - 1);
  end if;

  if prof.ad_credits > 0 then
    update profiles set ad_credits = ad_credits - 1 where id = p_user;
    return jsonb_build_object('ok', true, 'source', 'ad',
                              'remaining', prof.ad_credits - 1);
  end if;

  return jsonb_build_object('ok', false, 'reason', 'no_credits');
end;
$$;

create function refund_credit(p_user uuid, p_source text)
returns void
language plpgsql
security definer set search_path = public
as $$
begin
  if p_source = 'free' then
    update profiles set free_used = greatest(0, free_used - 1) where id = p_user;
  elsif p_source = 'ad' then
    update profiles set ad_credits = ad_credits + 1 where id = p_user;
  end if;
end;
$$;

create function credit_ad_reward(
  p_user uuid, p_txn text, p_ad_unit text, p_daily_cap int default 5
)
returns jsonb
language plpgsql
security definer set search_path = public
as $$
declare
  prof profiles;
begin
  perform roll_periods(p_user);
  select * into prof from profiles where id = p_user for update;

  if prof is null then
    return jsonb_build_object('ok', false, 'reason', 'no_profile');
  end if;

  if prof.ads_today >= p_daily_cap then
    return jsonb_build_object('ok', false, 'reason', 'daily_cap');
  end if;

  begin
    insert into ad_rewards (transaction_id, user_id, ad_unit)
    values (p_txn, p_user, p_ad_unit);
  exception when unique_violation then
    return jsonb_build_object('ok', false, 'reason', 'duplicate');
  end;

  update profiles
     set ad_credits = ad_credits + 1,
         ads_today  = ads_today + 1
   where id = p_user;

  return jsonb_build_object('ok', true, 'credits', prof.ad_credits + 1);
end;
$$;

-- Source: legacy/add-avatar.sql
alter table profiles
  add column if not exists avatar_path text;

-- Source: legacy/add-paid-credits.sql
alter table profiles
  add column if not exists paid_credits int not null default 0;

create table if not exists credit_purchases (
  transaction_id text primary key,
  user_id        uuid not null references users(id) on delete cascade,
  quantity       int not null,
  product_id     text,
  credited_at    timestamptz not null default now()
);

create index if not exists credit_purchases_user_idx
  on credit_purchases (user_id, credited_at desc);

create or replace function consume_credit(p_user uuid, p_free_quota int default 3)
returns jsonb
language plpgsql
security definer set search_path = public
as $$
declare
  prof profiles;
begin
  perform roll_periods(p_user);
  select * into prof from profiles where id = p_user for update;

  if prof is null then
    return jsonb_build_object('ok', false, 'reason', 'no_profile');
  end if;

  if prof.plan = 'pro' and prof.plan_expires_at > now() then
    return jsonb_build_object('ok', true, 'source', 'pro');
  end if;

  if prof.free_used < p_free_quota then
    update profiles set free_used = free_used + 1 where id = p_user;
    return jsonb_build_object('ok', true, 'source', 'free',
                              'remaining', p_free_quota - prof.free_used - 1);
  end if;

  if prof.ad_credits > 0 then
    update profiles set ad_credits = ad_credits - 1 where id = p_user;
    return jsonb_build_object('ok', true, 'source', 'ad',
                              'remaining', prof.ad_credits - 1);
  end if;

  if prof.paid_credits > 0 then
    update profiles set paid_credits = paid_credits - 1 where id = p_user;
    return jsonb_build_object('ok', true, 'source', 'paid',
                              'remaining', prof.paid_credits - 1);
  end if;

  return jsonb_build_object('ok', false, 'reason', 'no_credits');
end;
$$;

create or replace function refund_credit(p_user uuid, p_source text)
returns void
language plpgsql
security definer set search_path = public
as $$
begin
  if p_source = 'free' then
    update profiles set free_used = greatest(0, free_used - 1) where id = p_user;
  elsif p_source = 'ad' then
    update profiles set ad_credits = ad_credits + 1 where id = p_user;
  elsif p_source = 'paid' then
    update profiles set paid_credits = paid_credits + 1 where id = p_user;
  end if;
end;
$$;

create or replace function credit_purchase(
  p_user uuid, p_txn text, p_quantity int, p_product text default null
)
returns jsonb
language plpgsql
security definer set search_path = public
as $$
begin
  if p_quantity is null or p_quantity < 1 or p_quantity > 100 then
    return jsonb_build_object('ok', false, 'reason', 'bad_quantity');
  end if;

  insert into credit_purchases (transaction_id, user_id, quantity, product_id)
  values (p_txn, p_user, p_quantity, p_product);

  update profiles
    set paid_credits = paid_credits + p_quantity
    where id = p_user;

  return jsonb_build_object('ok', true, 'quantity', p_quantity);
exception
  when unique_violation then
    return jsonb_build_object('ok', false, 'reason', 'already_credited');
end;
$$;

-- Source: legacy/add-plant-care-notes.sql
alter table plants
  add column if not exists light_note text;

alter table plants
  add column if not exists fertilizer_note text;

-- Source: legacy/add-plant-diagnosis.sql
alter table plants
  add column if not exists toxic_to_pets boolean;

alter table identifications
  add column if not exists resolved_at timestamptz;

create index if not exists ident_plant_idx
  on identifications (plant_id, created_at desc);

-- Source: legacy/add-plant-fertilizer.sql
alter table plants
  add column if not exists fertilizer text;

-- Source: legacy/fix-consent-and-columns.sql
alter table profiles
  add column if not exists accepted_terms_at timestamptz,
  add column if not exists terms_version     text,
  add column if not exists accepted_tips     boolean not null default false,
  add column if not exists accepted_tips_at  timestamptz;

create or replace function protect_consent_record()
returns trigger
language plpgsql
security definer set search_path = public
as $$
begin
  if old.accepted_terms_at is not null then
    new.accepted_terms_at := old.accepted_terms_at;
    new.terms_version := old.terms_version;
  end if;
  return new;
end;
$$;

drop trigger if exists profiles_protect_consent on profiles;

create trigger profiles_protect_consent
  before update on profiles
  for each row execute function protect_consent_record();

-- Source: legacy/fix-consent-versioning.sql
create or replace function protect_consent_record()
returns trigger
language plpgsql
security definer set search_path = public
as $$
begin
  if old.accepted_terms_at is null then
    return new;
  end if;

  if new.accepted_terms_at is null
     or new.accepted_terms_at < old.accepted_terms_at then
    new.accepted_terms_at := old.accepted_terms_at;
    new.terms_version := old.terms_version;
  end if;

  return new;
end;
$$;

drop trigger if exists profiles_protect_consent on profiles;

create trigger profiles_protect_consent
  before update on profiles
  for each row execute function protect_consent_record();

-- Source: supabase/migrations/20260820120000_add_plant_groups.sql
create table if not exists plant_groups (
  id         uuid primary key default gen_random_uuid(),
  user_id    uuid not null references users(id) on delete cascade,
  name       text not null,
  created_at timestamptz not null default now()
);

create index if not exists plant_groups_user_idx
  on plant_groups (user_id, created_at);

alter table plants
  add column if not exists group_id uuid
    references plant_groups(id) on delete set null;

create index if not exists plants_group_idx on plants (group_id);

-- Source: supabase/migrations/20260820160000_add_plant_tasks.sql
create table if not exists plant_tasks (
  id            uuid primary key default gen_random_uuid(),
  plant_id      uuid not null references plants(id) on delete cascade,
  user_id       uuid not null references users(id) on delete cascade,
  kind          text not null,
  interval_days int  not null,
  next_at       date not null,
  enabled       boolean not null default true,
  created_at    timestamptz not null default now(),
  unique (plant_id, kind)
);

create index if not exists plant_tasks_user_idx on plant_tasks (user_id);

create or replace function seed_plant_tasks()
returns trigger
language plpgsql
security definer set search_path = public
as $$
declare
  v_water_days int;
  v_water_next date;
  v_fert_days  int;
  v_prune_next date;
begin
  v_water_days := coalesce(new.watering_interval_days, 7);

  v_water_next := case
    when new.last_watered_at is null then current_date
    else new.last_watered_at::date + v_water_days
  end;

  v_fert_days := case new.fertilizer
    when 'quinzenal'  then 15
    when 'mensal'     then 30
    when 'bimestral'  then 60
    when 'estacional' then 30
    else null
  end;

  v_prune_next := make_date(extract(year from current_date)::int, 9, 1);

  if v_prune_next < current_date then
    v_prune_next := v_prune_next + interval '1 year';
  end if;

  insert into plant_tasks (plant_id, user_id, kind, interval_days, next_at, enabled)
  values
    (new.id, new.user_id, 'water',     v_water_days,             v_water_next,              true),
    (new.id, new.user_id, 'fertilize', coalesce(v_fert_days, 30), current_date + coalesce(v_fert_days, 30), v_fert_days is not null),
    (new.id, new.user_id, 'rotate',    14,                       current_date + 14,         true),
    (new.id, new.user_id, 'repot',     365,                      current_date + 365,        true),
    (new.id, new.user_id, 'prune',     365,                      v_prune_next,              true)
  on conflict (plant_id, kind) do nothing;

  return new;
end;
$$;

drop trigger if exists seed_plant_tasks_trigger on plants;

create trigger seed_plant_tasks_trigger
  after insert on plants
  for each row execute function seed_plant_tasks();

insert into plant_tasks (plant_id, user_id, kind, interval_days, next_at, enabled)
select
  p.id,
  p.user_id,
  'water',
  coalesce(p.watering_interval_days, 7),
  case
    when p.last_watered_at is null then current_date
    else p.last_watered_at::date + coalesce(p.watering_interval_days, 7)
  end,
  true
from plants p
on conflict (plant_id, kind) do nothing;

insert into plant_tasks (plant_id, user_id, kind, interval_days, next_at, enabled)
select
  p.id,
  p.user_id,
  'fertilize',
  coalesce(d.days, 30),
  current_date + coalesce(d.days, 30),
  d.days is not null
from plants p
cross join lateral (
  select case p.fertilizer
    when 'quinzenal'  then 15
    when 'mensal'     then 30
    when 'bimestral'  then 60
    when 'estacional' then 30
    else null
  end as days
) d
on conflict (plant_id, kind) do nothing;

insert into plant_tasks (plant_id, user_id, kind, interval_days, next_at, enabled)
select p.id, p.user_id, 'rotate', 14, current_date + 14, true
from plants p
on conflict (plant_id, kind) do nothing;

insert into plant_tasks (plant_id, user_id, kind, interval_days, next_at, enabled)
select p.id, p.user_id, 'repot', 365, current_date + 365, true
from plants p
on conflict (plant_id, kind) do nothing;

insert into plant_tasks (plant_id, user_id, kind, interval_days, next_at, enabled)
select
  p.id,
  p.user_id,
  'prune',
  365,
  case
    when make_date(extract(year from current_date)::int, 9, 1) < current_date
      then make_date(extract(year from current_date)::int, 9, 1) + interval '1 year'
    else make_date(extract(year from current_date)::int, 9, 1)
  end,
  true
from plants p
on conflict (plant_id, kind) do nothing;

-- Source: supabase/migrations/20260821120000_purge_loose_identifications.sql
create or replace function purge_loose_identifications()
returns void
language sql
security definer set search_path = public
as $$
  delete from identifications
  where plant_id is null
    and created_at < now() - interval '7 days';
$$;

-- Source: supabase/migrations/20260821140000_add_mist_and_care_columns.sql
alter table plants
  add column if not exists mist_days       int,
  add column if not exists rotate_days     int,
  add column if not exists repot_months    int,
  add column if not exists prune_month     int;

create or replace function seed_plant_tasks()
returns trigger
language plpgsql
security definer set search_path = public
as $$
declare
  v_water_days  int;
  v_water_next  date;
  v_fert_days   int;
  v_rotate_days int;
  v_repot_days  int;
  v_prune_month int;
  v_prune_next  date;
begin
  v_water_days := coalesce(new.watering_interval_days, 7);

  v_water_next := case
    when new.last_watered_at is null then current_date
    else new.last_watered_at::date + v_water_days
  end;

  v_fert_days := case new.fertilizer
    when 'quinzenal'  then 15
    when 'mensal'     then 30
    when 'bimestral'  then 60
    when 'estacional' then 30
    else null
  end;

  v_rotate_days := coalesce(new.rotate_days, 14);
  v_repot_days  := coalesce(new.repot_months, 12) * 30;
  v_prune_month := coalesce(new.prune_month, 9);

  v_prune_next := make_date(extract(year from current_date)::int, v_prune_month, 1);

  if v_prune_next < current_date then
    v_prune_next := v_prune_next + interval '1 year';
  end if;

  insert into plant_tasks (plant_id, user_id, kind, interval_days, next_at, enabled)
  values
    (new.id, new.user_id, 'water',     v_water_days,              v_water_next,                          true),
    (new.id, new.user_id, 'fertilize', coalesce(v_fert_days, 30), current_date + coalesce(v_fert_days, 30), v_fert_days is not null),
    (new.id, new.user_id, 'mist',      coalesce(new.mist_days, 7), current_date + coalesce(new.mist_days, 7), new.mist_days is not null),
    (new.id, new.user_id, 'rotate',    v_rotate_days,             current_date + v_rotate_days,          true),
    (new.id, new.user_id, 'repot',     v_repot_days,              current_date + v_repot_days,           true),
    (new.id, new.user_id, 'prune',     365,                       v_prune_next,                          new.prune_month is not null)
  on conflict (plant_id, kind) do nothing;

  return new;
end;
$$;

insert into plant_tasks (plant_id, user_id, kind, interval_days, next_at, enabled)
select p.id, p.user_id, 'mist', 7, current_date + 7, false
from plants p
on conflict (plant_id, kind) do nothing;

-- Source: supabase/migrations/20260821160000_add_month_cap.sql
alter table profiles
  add column if not exists analyses_month int not null default 0;

create or replace function roll_periods(p_user uuid)
returns void
language plpgsql
security definer set search_path = public
as $$
begin
  update profiles set
    free_used      = case when period_start < date_trunc('month', now())::date
                          then 0 else free_used end,
    analyses_month = case when period_start < date_trunc('month', now())::date
                          then 0 else analyses_month end,
    period_start   = greatest(period_start, date_trunc('month', now())::date),
    ads_today      = case when ads_today_date < current_date then 0 else ads_today end,
    ads_today_date = greatest(ads_today_date, current_date),
    analyses_today = case when analyses_day < current_date then 0 else analyses_today end,
    analyses_day   = greatest(analyses_day, current_date)
  where id = p_user;
end;
$$;

drop function if exists consume_credit(uuid, int, int);

create or replace function consume_credit(
  p_user       uuid,
  p_free_quota int default 3,
  p_daily_cap  int default 50,
  p_month_cap  int default 100
)
returns jsonb
language plpgsql
security definer set search_path = public
as $$
declare
  prof profiles;
begin
  perform roll_periods(p_user);
  select * into prof from profiles where id = p_user for update;

  if prof is null then
    return jsonb_build_object('ok', false, 'reason', 'no_profile');
  end if;

  if prof.analyses_month >= p_month_cap then
    return jsonb_build_object('ok', false, 'reason', 'month_cap',
                              'cap', p_month_cap);
  end if;

  if prof.analyses_today >= p_daily_cap then
    return jsonb_build_object('ok', false, 'reason', 'daily_cap',
                              'cap', p_daily_cap);
  end if;

  if prof.plan = 'pro' and prof.plan_expires_at > now() then
    update profiles set analyses_today = analyses_today + 1,
                        analyses_month = analyses_month + 1
      where id = p_user;
    return jsonb_build_object('ok', true, 'source', 'pro',
                              'remaining', p_month_cap - prof.analyses_month - 1);
  end if;

  if prof.free_used < p_free_quota then
    update profiles set free_used = free_used + 1,
                        analyses_today = analyses_today + 1,
                        analyses_month = analyses_month + 1
      where id = p_user;
    return jsonb_build_object('ok', true, 'source', 'free',
                              'remaining', p_free_quota - prof.free_used - 1);
  end if;

  if prof.ad_credits > 0 then
    update profiles set ad_credits = ad_credits - 1,
                        analyses_today = analyses_today + 1,
                        analyses_month = analyses_month + 1
      where id = p_user;
    return jsonb_build_object('ok', true, 'source', 'ad',
                              'remaining', prof.ad_credits - 1);
  end if;

  if prof.paid_credits > 0 then
    update profiles set paid_credits = paid_credits - 1,
                        analyses_today = analyses_today + 1,
                        analyses_month = analyses_month + 1
      where id = p_user;
    return jsonb_build_object('ok', true, 'source', 'paid',
                              'remaining', prof.paid_credits - 1);
  end if;

  return jsonb_build_object('ok', false, 'reason', 'no_credits');
end;
$$;

create or replace function refund_credit(p_user uuid, p_source text)
returns void
language plpgsql
security definer set search_path = public
as $$
begin
  update profiles
    set analyses_today = greatest(0, analyses_today - 1),
        analyses_month = greatest(0, analyses_month - 1)
    where id = p_user;

  if p_source = 'free' then
    update profiles set free_used = greatest(0, free_used - 1) where id = p_user;
  elsif p_source = 'ad' then
    update profiles set ad_credits = ad_credits + 1 where id = p_user;
  elsif p_source = 'paid' then
    update profiles set paid_credits = paid_credits + 1 where id = p_user;
  end if;
end;
$$;

-- Source: supabase/migrations/20260821180000_month_cap_sixty.sql
drop function if exists consume_credit(uuid, int, int, int);

create or replace function consume_credit(
  p_user       uuid,
  p_free_quota int default 3,
  p_daily_cap  int default 50,
  p_month_cap  int default 60
)
returns jsonb
language plpgsql
security definer set search_path = public
as $$
declare
  prof profiles;
begin
  perform roll_periods(p_user);
  select * into prof from profiles where id = p_user for update;

  if prof is null then
    return jsonb_build_object('ok', false, 'reason', 'no_profile');
  end if;

  if prof.analyses_today >= p_daily_cap then
    return jsonb_build_object('ok', false, 'reason', 'daily_cap',
                              'cap', p_daily_cap);
  end if;

  if prof.paid_credits > 0 then
    update profiles set paid_credits = paid_credits - 1,
                        analyses_today = analyses_today + 1
      where id = p_user;
    return jsonb_build_object('ok', true, 'source', 'paid',
                              'remaining', prof.paid_credits - 1);
  end if;

  if prof.analyses_month >= p_month_cap then
    return jsonb_build_object('ok', false, 'reason', 'month_cap',
                              'cap', p_month_cap);
  end if;

  if prof.plan = 'pro' and prof.plan_expires_at > now() then
    update profiles set analyses_today = analyses_today + 1,
                        analyses_month = analyses_month + 1
      where id = p_user;
    return jsonb_build_object('ok', true, 'source', 'pro',
                              'remaining', p_month_cap - prof.analyses_month - 1);
  end if;

  if prof.free_used < p_free_quota then
    update profiles set free_used = free_used + 1,
                        analyses_today = analyses_today + 1,
                        analyses_month = analyses_month + 1
      where id = p_user;
    return jsonb_build_object('ok', true, 'source', 'free',
                              'remaining', p_free_quota - prof.free_used - 1);
  end if;

  if prof.ad_credits > 0 then
    update profiles set ad_credits = ad_credits - 1,
                        analyses_today = analyses_today + 1,
                        analyses_month = analyses_month + 1
      where id = p_user;
    return jsonb_build_object('ok', true, 'source', 'ad',
                              'remaining', prof.ad_credits - 1);
  end if;

  return jsonb_build_object('ok', false, 'reason', 'no_credits');
end;
$$;

create or replace function refund_credit(p_user uuid, p_source text)
returns void
language plpgsql
security definer set search_path = public
as $$
begin
  update profiles
    set analyses_today = greatest(0, analyses_today - 1)
    where id = p_user;

  if p_source <> 'paid' then
    update profiles
      set analyses_month = greatest(0, analyses_month - 1)
      where id = p_user;
  end if;

  if p_source = 'free' then
    update profiles set free_used = greatest(0, free_used - 1) where id = p_user;
  elsif p_source = 'ad' then
    update profiles set ad_credits = ad_credits + 1 where id = p_user;
  elsif p_source = 'paid' then
    update profiles set paid_credits = paid_credits + 1 where id = p_user;
  end if;
end;
$$;

-- Source: supabase/migrations/20260821190000_fix_missing_cap_columns.sql
alter table profiles
  add column if not exists analyses_today int  not null default 0,
  add column if not exists analyses_day   date not null default current_date,
  add column if not exists analyses_month int  not null default 0;

create or replace function roll_periods(p_user uuid)
returns void
language plpgsql
security definer set search_path = public
as $$
begin
  update profiles set
    free_used      = case when period_start < date_trunc('month', now())::date
                          then 0 else free_used end,
    analyses_month = case when period_start < date_trunc('month', now())::date
                          then 0 else analyses_month end,
    period_start   = greatest(period_start, date_trunc('month', now())::date),
    ads_today      = case when ads_today_date < current_date then 0 else ads_today end,
    ads_today_date = greatest(ads_today_date, current_date),
    analyses_today = case when analyses_day < current_date then 0 else analyses_today end,
    analyses_day   = greatest(analyses_day, current_date)
  where id = p_user;
end;
$$;

create or replace function consume_credit(
  p_user       uuid,
  p_free_quota int default 3,
  p_daily_cap  int default 50,
  p_month_cap  int default 60
)
returns jsonb
language plpgsql
security definer set search_path = public
as $$
declare
  prof profiles;
begin
  perform roll_periods(p_user);
  select * into prof from profiles where id = p_user for update;

  if prof is null then
    return jsonb_build_object('ok', false, 'reason', 'no_profile');
  end if;

  if prof.analyses_today >= p_daily_cap then
    return jsonb_build_object('ok', false, 'reason', 'daily_cap',
                              'cap', p_daily_cap);
  end if;

  if prof.paid_credits > 0 then
    update profiles set paid_credits = paid_credits - 1,
                        analyses_today = analyses_today + 1
      where id = p_user;
    return jsonb_build_object('ok', true, 'source', 'paid',
                              'remaining', prof.paid_credits - 1);
  end if;

  if prof.analyses_month >= p_month_cap then
    return jsonb_build_object('ok', false, 'reason', 'month_cap',
                              'cap', p_month_cap);
  end if;

  if prof.plan = 'pro' and prof.plan_expires_at > now() then
    update profiles set analyses_today = analyses_today + 1,
                        analyses_month = analyses_month + 1
      where id = p_user;
    return jsonb_build_object('ok', true, 'source', 'pro',
                              'remaining', p_month_cap - prof.analyses_month - 1);
  end if;

  if prof.free_used < p_free_quota then
    update profiles set free_used = free_used + 1,
                        analyses_today = analyses_today + 1,
                        analyses_month = analyses_month + 1
      where id = p_user;
    return jsonb_build_object('ok', true, 'source', 'free',
                              'remaining', p_free_quota - prof.free_used - 1);
  end if;

  if prof.ad_credits > 0 then
    update profiles set ad_credits = ad_credits - 1,
                        analyses_today = analyses_today + 1,
                        analyses_month = analyses_month + 1
      where id = p_user;
    return jsonb_build_object('ok', true, 'source', 'ad',
                              'remaining', prof.ad_credits - 1);
  end if;

  return jsonb_build_object('ok', false, 'reason', 'no_credits');
end;
$$;

create or replace function refund_credit(p_user uuid, p_source text)
returns void
language plpgsql
security definer set search_path = public
as $$
begin
  update profiles
    set analyses_today = greatest(0, analyses_today - 1)
    where id = p_user;

  if p_source <> 'paid' then
    update profiles
      set analyses_month = greatest(0, analyses_month - 1)
      where id = p_user;
  end if;

  if p_source = 'free' then
    update profiles set free_used = greatest(0, free_used - 1) where id = p_user;
  elsif p_source = 'ad' then
    update profiles set ad_credits = ad_credits + 1 where id = p_user;
  elsif p_source = 'paid' then
    update profiles set paid_credits = paid_credits + 1 where id = p_user;
  end if;
end;
$$;

-- Source: supabase/migrations/20260821200000_add_species_cache.sql
create table if not exists species_cache (
  term        text primary key,
  results     jsonb not null,
  source      text  not null,
  created_at  timestamptz not null default now()
);

-- Source: supabase/migrations/20260821220000_clear_species_cache.sql
delete from species_cache;

-- Source: supabase/migrations/20260821230000_clear_species_cache_again.sql
delete from species_cache;

-- Source: supabase/migrations/20260822110000_add_search_budget.sql
create table if not exists search_budget (
  month  date primary key,
  calls  integer not null default 0
);

create or replace function bump_search_budget(cap integer)
returns boolean
language plpgsql
security definer
set search_path = public
as $$
declare
  key date := (date_trunc('month', now() at time zone 'utc'))::date;
  used integer;
begin
  insert into search_budget (month, calls)
  values (key, 0)
  on conflict (month) do nothing;

  select calls into used from search_budget where month = key for update;

  if used >= cap then
    return false;
  end if;

  update search_budget set calls = calls + 1 where month = key;
  return true;
end;
$$;

delete from species_cache;

-- Source: supabase/migrations/20260822130000_clear_cache_wider_search.sql
delete from species_cache;

-- Source: supabase/migrations/20260822140000_welcome_credits_and_chat.sql
alter table profiles
  add column if not exists welcome_credits int not null default 2,
  add column if not exists chat_expires_at timestamptz,
  add column if not exists chat_month      int  not null default 0,
  add column if not exists chat_today      int  not null default 0,
  add column if not exists chat_day        date not null default current_date;

update profiles set welcome_credits = 2 where welcome_credits is null;

create table if not exists chat_threads (
  id              uuid primary key default gen_random_uuid(),
  user_id         uuid not null references users(id) on delete cascade,
  plant_id        uuid references plants(id) on delete set null,
  title           text,
  created_at      timestamptz not null default now(),
  last_message_at timestamptz not null default now()
);

create index if not exists chat_threads_user_recent
  on chat_threads (user_id, last_message_at desc);

create table if not exists chat_messages (
  id         uuid primary key default gen_random_uuid(),
  thread_id  uuid not null references chat_threads(id) on delete cascade,
  user_id    uuid not null references users(id) on delete cascade,
  role       text not null check (role in ('user', 'assistant')),
  content    text not null,
  created_at timestamptz not null default now()
);

create index if not exists chat_messages_thread_order
  on chat_messages (thread_id, created_at);

create or replace function roll_periods(p_user uuid)
returns void
language plpgsql
security definer set search_path = public
as $$
begin
  update profiles set
    free_used      = case when period_start < date_trunc('month', now())::date
                          then 0 else free_used end,
    analyses_month = case when period_start < date_trunc('month', now())::date
                          then 0 else analyses_month end,
    chat_month     = case when period_start < date_trunc('month', now())::date
                          then 0 else chat_month end,
    period_start   = greatest(period_start, date_trunc('month', now())::date),
    ads_today      = case when ads_today_date < current_date then 0 else ads_today end,
    ads_today_date = greatest(ads_today_date, current_date),
    analyses_today = case when analyses_day < current_date then 0 else analyses_today end,
    analyses_day   = greatest(analyses_day, current_date),
    chat_today     = case when chat_day < current_date then 0 else chat_today end,
    chat_day       = greatest(chat_day, current_date)
  where id = p_user;
end;
$$;

create or replace function consume_credit(
  p_user       uuid,
  p_free_quota int default 1,
  p_daily_cap  int default 50,
  p_month_cap  int default 45
)
returns jsonb
language plpgsql
security definer set search_path = public
as $$
declare
  prof profiles;
begin
  perform roll_periods(p_user);
  select * into prof from profiles where id = p_user for update;

  if prof is null then
    return jsonb_build_object('ok', false, 'reason', 'no_profile');
  end if;

  if prof.analyses_today >= p_daily_cap then
    return jsonb_build_object('ok', false, 'reason', 'daily_cap',
                              'cap', p_daily_cap);
  end if;

  if prof.paid_credits > 0 then
    update profiles set paid_credits = paid_credits - 1,
                        analyses_today = analyses_today + 1
      where id = p_user;
    return jsonb_build_object('ok', true, 'source', 'paid',
                              'remaining', prof.paid_credits - 1);
  end if;

  if prof.analyses_month >= p_month_cap then
    return jsonb_build_object('ok', false, 'reason', 'month_cap',
                              'cap', p_month_cap);
  end if;

  if prof.plan = 'pro' and prof.plan_expires_at > now() then
    update profiles set analyses_today = analyses_today + 1,
                        analyses_month = analyses_month + 1
      where id = p_user;
    return jsonb_build_object('ok', true, 'source', 'pro',
                              'remaining', p_month_cap - prof.analyses_month - 1);
  end if;

  if prof.free_used < p_free_quota then
    update profiles set free_used = free_used + 1,
                        analyses_today = analyses_today + 1,
                        analyses_month = analyses_month + 1
      where id = p_user;
    return jsonb_build_object('ok', true, 'source', 'free',
                              'remaining', p_free_quota - prof.free_used - 1);
  end if;

  if prof.welcome_credits > 0 then
    update profiles set welcome_credits = welcome_credits - 1,
                        analyses_today = analyses_today + 1,
                        analyses_month = analyses_month + 1
      where id = p_user;
    return jsonb_build_object('ok', true, 'source', 'welcome',
                              'remaining', prof.welcome_credits - 1);
  end if;

  if prof.ad_credits > 0 then
    update profiles set ad_credits = ad_credits - 1,
                        analyses_today = analyses_today + 1,
                        analyses_month = analyses_month + 1
      where id = p_user;
    return jsonb_build_object('ok', true, 'source', 'ad',
                              'remaining', prof.ad_credits - 1);
  end if;

  return jsonb_build_object('ok', false, 'reason', 'no_credits');
end;
$$;

create or replace function refund_credit(p_user uuid, p_source text)
returns void
language plpgsql
security definer set search_path = public
as $$
begin
  update profiles
    set analyses_today = greatest(0, analyses_today - 1)
    where id = p_user;

  if p_source <> 'paid' then
    update profiles
      set analyses_month = greatest(0, analyses_month - 1)
      where id = p_user;
  end if;

  if p_source = 'free' then
    update profiles set free_used = greatest(0, free_used - 1) where id = p_user;
  elsif p_source = 'welcome' then
    update profiles set welcome_credits = welcome_credits + 1 where id = p_user;
  elsif p_source = 'ad' then
    update profiles set ad_credits = ad_credits + 1 where id = p_user;
  elsif p_source = 'paid' then
    update profiles set paid_credits = paid_credits + 1 where id = p_user;
  end if;
end;
$$;

create or replace function consume_chat_message(
  p_user      uuid,
  p_month_cap int default 150,
  p_daily_cap int default 30
)
returns jsonb
language plpgsql
security definer set search_path = public
as $$
declare
  prof profiles;
begin
  perform roll_periods(p_user);
  select * into prof from profiles where id = p_user for update;

  if prof is null then
    return jsonb_build_object('ok', false, 'reason', 'no_profile');
  end if;

  if not coalesce((prof.plan = 'pro' and prof.plan_expires_at > now())
    or prof.chat_expires_at > now(), false) then
    return jsonb_build_object('ok', false, 'reason', 'no_plan');
  end if;

  if prof.chat_today >= p_daily_cap then
    return jsonb_build_object('ok', false, 'reason', 'daily_cap',
                              'cap', p_daily_cap);
  end if;

  if prof.chat_month >= p_month_cap then
    return jsonb_build_object('ok', false, 'reason', 'month_cap',
                              'cap', p_month_cap);
  end if;

  update profiles set chat_today = chat_today + 1,
                      chat_month = chat_month + 1
    where id = p_user;

  return jsonb_build_object('ok', true,
                            'remaining', p_month_cap - prof.chat_month - 1,
                            'remaining_today', p_daily_cap - prof.chat_today - 1);
end;
$$;

create or replace function refund_chat_message(p_user uuid)
returns void
language plpgsql
security definer set search_path = public
as $$
begin
  update profiles set chat_today = greatest(0, chat_today - 1),
                      chat_month = greatest(0, chat_month - 1)
    where id = p_user;
end;
$$;

create or replace function purge_stale_chat_threads()
returns void
language sql
security definer set search_path = public
as $$
  delete from chat_threads
   where last_message_at < now() - interval '30 days';
$$;

-- Source: supabase/migrations/20260822160000_month_cap_forty.sql
create or replace function consume_credit(
  p_user       uuid,
  p_free_quota int default 1,
  p_daily_cap  int default 50,
  p_month_cap  int default 40
)
returns jsonb
language plpgsql
security definer set search_path = public
as $$
declare
  prof profiles;
begin
  perform roll_periods(p_user);
  select * into prof from profiles where id = p_user for update;

  if prof is null then
    return jsonb_build_object('ok', false, 'reason', 'no_profile');
  end if;

  if prof.analyses_today >= p_daily_cap then
    return jsonb_build_object('ok', false, 'reason', 'daily_cap',
                              'cap', p_daily_cap);
  end if;

  if prof.paid_credits > 0 then
    update profiles set paid_credits = paid_credits - 1,
                        analyses_today = analyses_today + 1
      where id = p_user;
    return jsonb_build_object('ok', true, 'source', 'paid',
                              'remaining', prof.paid_credits - 1);
  end if;

  if prof.analyses_month >= p_month_cap then
    return jsonb_build_object('ok', false, 'reason', 'month_cap',
                              'cap', p_month_cap);
  end if;

  if prof.plan = 'pro' and prof.plan_expires_at > now() then
    update profiles set analyses_today = analyses_today + 1,
                        analyses_month = analyses_month + 1
      where id = p_user;
    return jsonb_build_object('ok', true, 'source', 'pro',
                              'remaining', p_month_cap - prof.analyses_month - 1);
  end if;

  if prof.free_used < p_free_quota then
    update profiles set free_used = free_used + 1,
                        analyses_today = analyses_today + 1,
                        analyses_month = analyses_month + 1
      where id = p_user;
    return jsonb_build_object('ok', true, 'source', 'free',
                              'remaining', p_free_quota - prof.free_used - 1);
  end if;

  if prof.welcome_credits > 0 then
    update profiles set welcome_credits = welcome_credits - 1,
                        analyses_today = analyses_today + 1,
                        analyses_month = analyses_month + 1
      where id = p_user;
    return jsonb_build_object('ok', true, 'source', 'welcome',
                              'remaining', prof.welcome_credits - 1);
  end if;

  if prof.ad_credits > 0 then
    update profiles set ad_credits = ad_credits - 1,
                        analyses_today = analyses_today + 1,
                        analyses_month = analyses_month + 1
      where id = p_user;
    return jsonb_build_object('ok', true, 'source', 'ad',
                              'remaining', prof.ad_credits - 1);
  end if;

  return jsonb_build_object('ok', false, 'reason', 'no_credits');
end;
$$;

-- Source: supabase/migrations/20260822180000_clear_cache_thumbnails.sql
delete from species_cache;

-- Source: supabase/migrations/20260822200000_clear_cache_images.sql
delete from species_cache;

-- Source: supabase/migrations/20260825120000_add_species_facts.sql
create table if not exists species_facts (
  scientific  text not null,
  language    text not null,
  data        jsonb not null,
  model       text,
  cost_micros int,
  created_at  timestamptz not null default now(),
  primary key (scientific, language)
);

-- Source: supabase/migrations/20260825200000_add_temperature_unit.sql
alter table profiles
  add column if not exists temperature_unit text;

alter table profiles
  drop constraint if exists profiles_temperature_unit_check;

alter table profiles
  add constraint profiles_temperature_unit_check
  check (temperature_unit is null or temperature_unit in ('celsius', 'fahrenheit'));

-- Source: supabase/migrations/20260826120000_task_remind_at.sql
alter table profiles
  add column if not exists chat_nudge_enabled boolean not null default true;

alter table plant_tasks
  add column if not exists remind_at time;

comment on column plant_tasks.remind_at is
  'Horario do lembrete desta tarefa. Nulo usa o padrao do perfil.';

create or replace function seed_plant_tasks()
returns trigger
language plpgsql
security definer set search_path = public
as $$
declare
  v_water_days  int;
  v_water_next  date;
  v_fert_days   int;
  v_rotate_days int;
  v_repot_days  int;
  v_prune_month int;
  v_prune_next  date;
begin
  v_water_days := coalesce(new.watering_interval_days, 7);

  v_water_next := case
    when new.last_watered_at is null then current_date
    else new.last_watered_at::date + v_water_days
  end;

  v_fert_days := case new.fertilizer
    when 'quinzenal'  then 15
    when 'mensal'     then 30
    when 'bimestral'  then 60
    when 'estacional' then 30
    else null
  end;

  v_rotate_days := coalesce(new.rotate_days, 14);
  v_repot_days  := coalesce(new.repot_months, 12) * 30;
  v_prune_month := coalesce(new.prune_month, 9);

  v_prune_next := make_date(extract(year from current_date)::int, v_prune_month, 1);

  if v_prune_next < current_date then
    v_prune_next := v_prune_next + interval '1 year';
  end if;

  insert into plant_tasks (plant_id, user_id, kind, interval_days, next_at, enabled)
  values
    (new.id, new.user_id, 'water',     v_water_days,              v_water_next,                             true),
    (new.id, new.user_id, 'fertilize', coalesce(v_fert_days, 30), current_date + coalesce(v_fert_days, 30), v_fert_days is not null),
    (new.id, new.user_id, 'mist',      coalesce(new.mist_days, 7), current_date + coalesce(new.mist_days, 7), new.mist_days is not null),
    (new.id, new.user_id, 'rotate',    v_rotate_days,             current_date + v_rotate_days,             true),
    (new.id, new.user_id, 'repot',     v_repot_days,              current_date + v_repot_days,              true),
    (new.id, new.user_id, 'prune',     365,                       v_prune_next,                             new.prune_month is not null)
  on conflict (plant_id, kind) do nothing;

  return new;
end;
$$;

-- Source: supabase/migrations/20260826180000_plant_temperature.sql
alter table plants
  add column if not exists temp_min_c int,
  add column if not exists temp_max_c int;

comment on column plants.temp_min_c is
  'Faixa de temperatura definida pela pessoa. Nulo usa a da especie.';

alter table profiles
  drop column if exists chat_nudge_enabled;

-- Source: supabase/migrations/20260828120000_announcements.sql
create table if not exists announcements (
  id          uuid        primary key default gen_random_uuid(),
  kind        text        not null default 'general',
  title       jsonb       not null,
  body        jsonb       not null,
  starts_at   timestamptz not null default now(),
  ends_at     timestamptz,
  created_at  timestamptz not null default now()
);

comment on table announcements is
  'Avisos mostrados dentro do app. Escritos so pelo painel, nunca pelo cliente.';

comment on column announcements.title is
  'JSON por idioma: {"pt-BR": "...", "en-US": "...", "es-ES": "..."}';

comment on column announcements.kind is
  'price para mudanca de valor, general para o resto.';

alter table profiles
  add column if not exists dismissed_announcement uuid;

comment on column profiles.dismissed_announcement is
  'Ultimo aviso que a pessoa fechou. Nulo mostra o aviso ativo.';

-- Source: supabase/migrations/20260828160000_revoke_terms.sql
alter table profiles
  add column if not exists revoked_terms_at timestamptz;

comment on column profiles.revoked_terms_at is
  'Quando a pessoa revogou o aceite. Mais recente que accepted_terms_at exige aceitar de novo.';

create or replace function protect_consent_record()
returns trigger
language plpgsql
security definer set search_path = public
as $$
begin
  if old.accepted_terms_at is null then
    return new;
  end if;

  if new.accepted_terms_at is null
     or new.accepted_terms_at < old.accepted_terms_at then
    new.accepted_terms_at := old.accepted_terms_at;
    new.terms_version := old.terms_version;
  end if;

  if new.accepted_terms_at > old.accepted_terms_at then
    new.revoked_terms_at := null;
  end if;

  return new;
end;
$$;

drop trigger if exists profiles_protect_consent on profiles;

create trigger profiles_protect_consent
  before update on profiles
  for each row execute function protect_consent_record();

-- Source: supabase/migrations/20260828200000_announcement_notify.sql
alter table announcements
  add column if not exists notify_at timestamptz;

comment on column announcements.notify_at is
  'Quando disparar a notificacao local do aviso. Nulo nao notifica, so mostra a faixa.';

-- Source: supabase/migrations/20260828220000_announcement_detail.sql
alter table announcements
  add column if not exists detail jsonb;

comment on column announcements.detail is
  'Texto longo do modal, por idioma. Nulo repete o body.';

-- Source: supabase/migrations/20260829120000_push_reminders.sql
create table if not exists push_tokens (
  token      text        primary key,
  user_id    uuid        not null references users(id) on delete cascade,
  platform   text,
  updated_at timestamptz not null default now()
);

create index if not exists push_tokens_user_idx on push_tokens (user_id);

create table if not exists reminder_events (
  id        uuid        primary key default gen_random_uuid(),
  user_id   uuid        not null references users(id) on delete cascade,
  plant_id  uuid        references plants(id) on delete cascade,
  kind      text        not null,
  title     text        not null,
  body      text        not null,
  sent_at   timestamptz not null default now(),
  read_at   timestamptz
);

create index if not exists reminder_events_user_idx
  on reminder_events (user_id, sent_at desc);

comment on table reminder_events is
  'Lembretes que sairam de verdade. Alimenta a caixa de lembretes do app.';

alter table profiles
  add column if not exists language     text,
  add column if not exists last_seen_at timestamptz;

comment on column profiles.language is
  'Idioma do app, para o servidor escrever o lembrete na lingua certa.';

comment on column profiles.last_seen_at is
  'Ultima abertura do app. Base do cutucao do Brotinho.';

create or replace function due_reminders()
returns table (
  user_id   uuid,
  plant_id  uuid,
  plant     text,
  language  text,
  kinds     text[],
  late_days int
)
language sql
security definer set search_path = public
as $$
  with clock as (
    select p.id as user_id,
           coalesce(nullif(p.language, ''), 'pt-BR') as language,
           now() at time zone coalesce(nullif(p.timezone, ''), 'America/Sao_Paulo')
             as local_now
    from profiles p
    where p.notifications_enabled
  ),
  due as (
    select c.user_id,
           c.language,
           t.plant_id,
           pl.nickname as plant,
           t.kind,
           (c.local_now::date - t.next_at) as late
    from clock c
    join plant_tasks t
      on t.user_id = c.user_id
     and t.enabled
     and t.next_at <= c.local_now::date
     and extract(hour from coalesce(t.remind_at, '09:00'::time))
         = extract(hour from c.local_now)
    join plants pl on pl.id = t.plant_id
  )
  select user_id,
         plant_id,
         plant,
         language,
         array_agg(kind order by kind),
         max(late)::int
  from due
  group by user_id, plant_id, plant, language;
$$;

create or replace function due_chat_nudges(p_days int)
returns table (user_id uuid, language text)
language sql
security definer set search_path = public
as $$
  select p.id,
         coalesce(nullif(p.language, ''), 'pt-BR')
  from profiles p
  where p.notifications_enabled
    and p.last_seen_at is not null
    and p.last_seen_at < now() - make_interval(days => p_days)
    and not exists (
      select 1 from reminder_events e
      where e.user_id = p.id
        and e.kind = 'chat'
        and e.sent_at > now() - make_interval(days => p_days)
    );
$$;

-- Source: supabase/migrations/20260901120000_plan_period.sql
alter table profiles
  add column if not exists plan_period text,
  add column if not exists chat_period text;

do $$
begin
  if not exists (
    select 1 from pg_constraint where conname = 'profiles_plan_period_check'
  ) then
    alter table profiles
      add constraint profiles_plan_period_check
      check (plan_period in ('monthly', 'annual'));
  end if;

  if not exists (
    select 1 from pg_constraint where conname = 'profiles_chat_period_check'
  ) then
    alter table profiles
      add constraint profiles_chat_period_check
      check (chat_period in ('monthly', 'annual'));
  end if;
end $$;

-- Source: supabase/migrations/20260901140000_purge_reminder_events.sql
create or replace function purge_reminder_events()
returns void
language sql
security definer set search_path = public
as $$
  delete from reminder_events
  where sent_at < now() - interval '14 days';
$$;

comment on function purge_reminder_events is
  'Apaga lembrete entregue ha mais de 14 dias, lido ou nao.';