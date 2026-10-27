-- One Postgres container, a database per service (realistic service isolation).
-- Runs once on first init of an empty data volume.
CREATE DATABASE catalog;
CREATE DATABASE auth;
CREATE DATABASE orders;
CREATE DATABASE payments;
CREATE DATABASE inventory;
