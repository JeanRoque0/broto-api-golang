-- Match the frontend agenda: archived plants do not produce reminders.
-- A task without a custom reminder time follows the profile preference.
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
           p.reminder_time,
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
     and extract(hour from coalesce(t.remind_at, c.reminder_time, '09:00'::time))
         = extract(hour from c.local_now)
    join plants pl on pl.id = t.plant_id and pl.archived_at is null
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
revoke execute on function due_reminders() from public;
