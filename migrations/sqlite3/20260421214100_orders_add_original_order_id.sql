-- +up
-- +begin
ALTER TABLE orders ADD COLUMN original_order_id INTEGER NOT NULL DEFAULT 0;
-- +end

-- +down
-- +begin
ALTER TABLE orders DROP COLUMN original_order_id;
-- +end
