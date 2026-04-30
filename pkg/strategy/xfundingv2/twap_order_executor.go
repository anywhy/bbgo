package xfundingv2

import (
	"context"
	"fmt"
	"time"

	"github.com/c9s/bbgo/pkg/bbgo"
	"github.com/c9s/bbgo/pkg/fixedpoint"
	"github.com/c9s/bbgo/pkg/types"
	"github.com/sirupsen/logrus"
)

type TWAPExecutor struct {
	TWAPWorkerConfig

	ctx context.Context

	exchange types.ExchangeOrderQueryService
	market   types.Market
	executor *bbgo.GeneralOrderExecutor

	ordersMap map[types.Order]struct{}

	logger logrus.FieldLogger
}

func NewTWAPOrderExecutor(
	ctx context.Context,
	exchange types.ExchangeOrderQueryService,
	market types.Market,
	executor *bbgo.GeneralOrderExecutor,
	config TWAPWorkerConfig,
) *TWAPExecutor {
	return &TWAPExecutor{
		TWAPWorkerConfig: config,

		ctx:      ctx,
		exchange: exchange,
		executor: executor,
		market:   market,

		ordersMap: make(map[types.Order]struct{}),
	}
}

func (o *TWAPExecutor) SetLogger(logger logrus.FieldLogger) {
	o.logger = logger
}

func (o *TWAPExecutor) Start() {
	if o.logger == nil {
		o.logger = logrus.WithFields(
			logrus.Fields{
				"component": "TWAPOrderExecutor",
				"symbol":    o.market.Symbol,
			},
		)
	}
}

func (o *TWAPExecutor) Stop() error {
	// adding Stop method for future use
	return nil
}

func (o *TWAPExecutor) SyncOrder(order types.Order) error {
	storeOrder, ok := o.executor.OrderStore().Get(order.OrderID)
	if !ok {
		return fmt.Errorf("order not found in store: %s", order)
	}

	if storeOrder.GetRemainingQuantity().IsZero() {
		return nil
	}

	timedCtx, cancel := context.WithTimeout(o.ctx, 500*time.Millisecond)
	defer cancel()

	query := order.AsQuery()
	updatedOrder, err := o.exchange.QueryOrder(
		timedCtx,
		query,
	)
	if err != nil {
		return fmt.Errorf("failed to query order: %v", query)
	}

	trades, err := o.exchange.QueryOrderTrades(timedCtx, query)
	if err != nil {
		return fmt.Errorf("failed to query order trades: %v, %v", query, err)
	}

	// order exists in the store (by the check at the beginning), we update the order
	o.executor.OrderStore().Update(*updatedOrder)
	for _, trade := range trades {
		o.executor.TradeCollector().ProcessTrade(trade)
	}

	return nil
}

func (o *TWAPExecutor) Trades() []types.Trade {
	var trades []types.Trade
	for order := range o.ordersMap {
		ts := o.executor.TradeCollector().TradeStore().GetOrderTrades(order)
		trades = append(trades, ts...)
	}
	return trades
}

func (o *TWAPExecutor) GetOrder(orderID uint64) (types.Order, bool) {
	return o.executor.OrderStore().Get(orderID)
}

// place order
func (o *TWAPExecutor) PlaceOrder(quantity fixedpoint.Value, side types.SideType, orderBook types.OrderBook, deadlineExceeded bool) (*types.Order, error) {
	// find the better price and submit new order
	quantity = o.market.TruncateQuantity(quantity)
	price, err := o.GetPrice(side, orderBook)
	if err != nil {
		o.logger.WithError(err).Warn("[TWAP tick] failed to get price for active order update")
		return nil, err
	}
	price = o.market.TruncatePrice(price)
	order := o.buildSubmitOrder(quantity, price, side, deadlineExceeded)
	if o.market.IsDustQuantity(order.Quantity, order.Price) {
		return nil, fmt.Errorf("order is of dust quantity: %s", quantity)
	}

	timedCtx, cancel := context.WithTimeout(o.ctx, 500*time.Millisecond)
	defer cancel()

	createdOrders, err := o.executor.SubmitOrders(timedCtx, order)
	if err != nil || len(createdOrders) == 0 {
		return nil, fmt.Errorf("failed to submit order: %+v, %v", order, err)
	}
	o.ordersMap[createdOrders[0]] = struct{}{}
	return &createdOrders[0], nil
}

func (o *TWAPExecutor) buildSubmitOrder(quantity, price fixedpoint.Value, side types.SideType, deadlineExceeded bool) types.SubmitOrder {
	if deadlineExceeded {
		return types.SubmitOrder{
			Symbol:   o.market.Symbol,
			Market:   o.market,
			Side:     side,
			Type:     types.OrderTypeMarket,
			Quantity: quantity,
		}
	}
	orderType := types.OrderTypeLimitMaker
	timeInForce := types.TimeInForceGTC

	if o.OrderType == TWAPOrderTypeTaker {
		orderType = types.OrderTypeLimit
		timeInForce = types.TimeInForceIOC
	}

	return types.SubmitOrder{
		Symbol:      o.market.Symbol,
		Market:      o.market,
		Side:        side,
		Type:        orderType,
		Quantity:    quantity,
		Price:       price,
		TimeInForce: timeInForce,
	}
}

func (o *TWAPExecutor) GetPrice(side types.SideType, orderBook types.OrderBook) (price fixedpoint.Value, err error) {
	defer func() {
		if err == nil {
			price = o.market.TruncatePrice(price)
		}
	}()

	switch o.OrderType {
	case TWAPOrderTypeTaker:
		return o.getTakerPrice(side, orderBook)
	case TWAPOrderTypeMaker:
		return o.getMakerPrice(side, orderBook)
	default:
		return o.getTakerPrice(side, orderBook)
	}
}

func (o *TWAPExecutor) getTakerPrice(side types.SideType, orderBook types.OrderBook) (fixedpoint.Value, error) {
	switch side {
	case types.SideTypeBuy:
		ask, ok := orderBook.BestAsk()
		if !ok {
			return fixedpoint.Zero, fmt.Errorf("no ask price available for %s", o.market.Symbol)
		}
		price := ask.Price
		if o.MaxSlippage.Sign() > 0 {
			maxPrice := price.Mul(fixedpoint.One.Add(o.MaxSlippage))
			price = fixedpoint.Min(price, maxPrice)
		}
		return price, nil

	case types.SideTypeSell:
		bid, ok := orderBook.BestBid()
		if !ok {
			return fixedpoint.Zero, fmt.Errorf("no bid price available for %s", o.market.Symbol)
		}
		price := bid.Price
		if o.MaxSlippage.Sign() > 0 {
			minPrice := price.Mul(fixedpoint.One.Sub(o.MaxSlippage))
			price = fixedpoint.Max(price, minPrice)
		}
		return price, nil

	default:
		return fixedpoint.Zero, fmt.Errorf("unknown side: %s", side)
	}
}

func (o *TWAPExecutor) getMakerPrice(side types.SideType, orderBook types.OrderBook) (fixedpoint.Value, error) {
	tickSize := o.market.TickSize
	numOfTicks := fixedpoint.NewFromInt(int64(o.NumOfTicks))
	tickImprovement := tickSize.Mul(numOfTicks)

	switch side {
	case types.SideTypeBuy:
		bid, ok := orderBook.BestBid()
		if !ok {
			return fixedpoint.Zero, fmt.Errorf("no bid price available for %s", o.market.Symbol)
		}
		// improve price by moving closer to spread
		price := bid.Price.Add(tickImprovement)
		// but don't cross the spread
		ask, hasAsk := orderBook.BestAsk()
		if hasAsk && price.Compare(ask.Price) >= 0 {
			price = ask.Price.Sub(tickSize)
		}
		return price, nil

	case types.SideTypeSell:
		ask, ok := orderBook.BestAsk()
		if !ok {
			return fixedpoint.Zero, fmt.Errorf("no ask price available for %s", o.market.Symbol)
		}
		price := ask.Price.Sub(tickImprovement)
		bid, hasBid := orderBook.BestBid()
		if hasBid && price.Compare(bid.Price) <= 0 {
			price = bid.Price.Add(tickSize)
		}
		return price, nil

	default:
		return fixedpoint.Zero, fmt.Errorf("unknown side: %s", side)
	}
}

// cancel order
func (o *TWAPExecutor) CancelOrder(ctx context.Context, order types.Order) error {
	timedCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()

	return o.executor.CancelOrders(timedCtx, order)
}
