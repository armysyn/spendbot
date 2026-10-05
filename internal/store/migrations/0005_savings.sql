-- Savings categories (deposits, investments) can be excluded from spending on the analytics page.
ALTER TABLE categories ADD COLUMN savings INTEGER NOT NULL DEFAULT 0;
UPDATE categories SET savings = 1 WHERE name IN ('Investments', 'Savings', 'Deposits',
  'Инвестиции', 'Сбережения', 'Накопления', 'Депозит', 'Депозиты'); -- Russian names of existing databases
