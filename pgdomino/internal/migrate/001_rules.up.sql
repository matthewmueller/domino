create table domino_rules (
  id          bigint generated always as identity primary key,
  owner_id    text not null,
  name        text not null,
  trigger     text not null,
  created_at  timestamptz not null default now(),
  updated_at  timestamptz not null default now()
);
create index domino_rules_owner_id_trigger on domino_rules (owner_id, trigger);

create table domino_conditions (
  id       bigint generated always as identity primary key,
  rule_id  bigint not null references domino_rules (id) on delete cascade,
  position int not null,
  field    text not null,
  op       text not null check (op in ('=', '!=', '>', '<', '>=', '<=', 'in', 'not_in',
                                       'contains', 'not_contains', 'starts_with', 'ends_with')),
  value    jsonb not null,
  unique (rule_id, position)
);

create table domino_actions (
  id       bigint generated always as identity primary key,
  rule_id  bigint not null references domino_rules (id) on delete cascade,
  position int not null,
  name     text not null,
  settings jsonb not null default '{}' check (jsonb_typeof(settings) = 'object'),
  unique (rule_id, position)
);
