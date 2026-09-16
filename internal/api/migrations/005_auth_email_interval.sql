-- Reserve one SMTP attempt per user across confirmation/recovery and replicas.
create table auth_email_sends (
 user_id uuid primary key references users(id) on delete cascade,
 attempted_at timestamptz not null
);
