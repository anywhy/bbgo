package mysql

import (
	"context"

	"github.com/c9s/rockhopper/v2"
)

func init() {
	AddMigration("main", up_main_ordersAddOriginalOrderId, down_main_ordersAddOriginalOrderId)
}

func up_main_ordersAddOriginalOrderId(ctx context.Context, tx rockhopper.SQLExecutor) (err error) {
	// This code is executed when the migration is applied.
	_, err = tx.ExecContext(ctx, "ALTER TABLE `orders` ADD COLUMN `original_order_id` BIGINT UNSIGNED NOT NULL DEFAULT 0;")
	if err != nil {
		return err
	}
	return err
}

func down_main_ordersAddOriginalOrderId(ctx context.Context, tx rockhopper.SQLExecutor) (err error) {
	// This code is executed when the migration is rolled back.
	_, err = tx.ExecContext(ctx, "ALTER TABLE `orders` DROP COLUMN `original_order_id`;")
	if err != nil {
		return err
	}
	return err
}
