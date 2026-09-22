CREATE TABLE task_meta
(
    id          bigserial,
    title       varchar(255)    not null default '',
    account     varchar(64)     not null,
    batch       int     not null default 0,
    tolerance   int     not null default 0,
    timeout     int     not null default 0,
    pause       varchar(255)    not null default '',
    script      text            not null,
    args        varchar(512)    not null default '',
    stdin       varchar(1024)   not null default '' ,
    creator     varchar(64)     not null default '',
    created     timestamp       not null default CURRENT_TIMESTAMP,
    PRIMARY KEY (id)
) ;
CREATE INDEX task_meta_creator_idx ON task_meta (creator);
CREATE INDEX task_meta_created_idx ON task_meta (created);

/* start|cancel|kill|pause */
CREATE TABLE task_action
(
    id     bigint  not null,
    action varchar(32)     not null,
    clock  bigint          not null default 0,
    PRIMARY KEY (id)
) ;

CREATE TABLE task_scheduler
(
    id        bigint  not null,
    scheduler varchar(128)    not null default ''
) ;
CREATE INDEX task_scheduler_id_scheduler_idx ON task_scheduler (id, scheduler);


CREATE TABLE task_scheduler_health
(
    scheduler varchar(128) not null,
    clock     bigint       not null,
    UNIQUE (scheduler)
) ;
CREATE INDEX task_scheduler_health_clock_idx ON task_scheduler_health (clock);


CREATE TABLE task_host_doing
(
    id     bigint  not null,
    host   varchar(128)    not null,
    clock  bigint          not null default 0,
    action varchar(16)     not null
) ;
CREATE INDEX task_host_doing_id_idx ON task_host_doing (id);
CREATE INDEX task_host_doing_host_idx ON task_host_doing (host);

