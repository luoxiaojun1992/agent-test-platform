CREATE DATABASE IF NOT EXISTS target_fixture
  CHARACTER SET utf8mb4
  COLLATE utf8mb4_0900_ai_ci;

CREATE TABLE target_fixture.orders (
    id         VARCHAR(32) PRIMARY KEY,
    status     VARCHAR(32) NOT NULL,
    updated_at TIMESTAMP(6) NOT NULL
);

INSERT INTO target_fixture.orders (id, status, updated_at) VALUES
    ('O10001', 'PAID', '2026-09-15 07:00:00.000000'),
    ('O10002', 'SHIPPED', '2026-09-15 07:00:00.000000');

CREATE USER IF NOT EXISTS 'target_reader'@'%' IDENTIFIED BY 'target-read-development';
GRANT SELECT ON target_fixture.* TO 'target_reader'@'%';
FLUSH PRIVILEGES;
