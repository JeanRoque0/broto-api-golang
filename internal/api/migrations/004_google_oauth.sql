-- Google identity is keyed by its stable OIDC subject, never by email alone.
create table oauth_identities (
 provider text not null check(provider = 'google'),
 subject text not null check(length(subject) between 1 and 255),
 user_id uuid not null references users(id) on delete cascade,
 created_at timestamptz not null default now(),
 primary key(provider, subject), unique(user_id, provider)
);

-- State and browser cookie values are stored as hashes. The Google PKCE
-- verifier is temporary and must be available for the server-side exchange.
create table oauth_attempts (
 state_hash text primary key,
 app_challenge text not null,
 return_uri text not null,
 browser_hash text,
 nonce_hash text,
 google_verifier text,
 expires_at timestamptz not null default now() + interval '10 minutes'
);
create index oauth_attempts_expiry on oauth_attempts(expires_at);

create table oauth_codes (
 code_hash text primary key,
 user_id uuid not null references users(id) on delete cascade,
 app_challenge text not null,
 expires_at timestamptz not null default now() + interval '1 minute'
);
create index oauth_codes_expiry on oauth_codes(expires_at);
