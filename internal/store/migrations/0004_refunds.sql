-- Purchase refunds (purchase kind, negative amount) used to be stored as non-spending
-- without a category. Now they belong to the merchant's category and reduce it: a known
-- merchant gets its category right away, others go to questions with their purchases.
INSERT INTO splits (tx_id, category_id, amount_minor)
SELECT t.id, r.category_id, t.amount_minor
FROM transactions t JOIN merchant_rules r ON r.merchant_norm = t.merchant_norm
JOIN categories c ON c.id = r.category_id AND c.archived = 0
WHERE t.source = 'import' AND t.kind = 'Покупка' AND t.status = 'info' AND t.amount_minor < 0;

UPDATE transactions SET status = 'done'
WHERE source = 'import' AND kind = 'Покупка' AND status = 'info' AND amount_minor < 0
  AND id IN (SELECT tx_id FROM splits);

UPDATE transactions SET status = 'review'
WHERE source = 'import' AND kind = 'Покупка' AND status = 'info' AND amount_minor < 0;
