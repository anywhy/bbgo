package xfundingv2

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/c9s/bbgo/pkg/fixedpoint"
	"github.com/c9s/bbgo/pkg/types"
	"github.com/c9s/bbgo/pkg/util/tradingutil"
)

type TWAPOrderType string

const (
	TWAPOrderTypeMaker TWAPOrderType = "maker"
	TWAPOrderTypeTaker TWAPOrderType = "taker"
)

//go:generate stringer -type=TWAPWorkerState
type TWAPWorkerState int

const (
	TWAPWorkerStatePending TWAPWorkerState = iota
	TWAPWorkerStateRunning
	TWAPWorkerStateDone
)

type TWAPWorkerConfig struct {
	// total duration to execute the TWAP order
	Duration time.Duration `json:"duration"`
	// how many slices to split into
	NumSlices int `json:"numSlices"`
	// Taker or Maker
	OrderType TWAPOrderType `json:"orderType"`
	// how often to re-evaluate active order price
	CheckInterval time.Duration `json:"checkInterval,omitempty"`

	// optional configs
	// max slippage ratio for taker orders (e.g. 0.001 = 0.1%)
	MaxSlippage fixedpoint.Value `json:"maxSlippage,omitempty"`
	// max quantity per slice
	MaxSliceSize fixedpoint.Value `json:"maxSliceSize,omitempty"`
	// min quantity per slice
	MinSliceSize fixedpoint.Value `json:"minSliceSize,omitempty"`
	// for maker: number of ticks to improve price
	NumOfTicks int `json:"numOfTicks,omitempty"`
}

//go:generate callbackgen -type=TWAPWorker -output=twap_callbacks.go
type TWAPWorker struct {
	// sync.Mutex protects fields mutated by the background trade goroutine:
	// filledQuantity, activeOrder, trades, state
	sync.Mutex

	config TWAPWorkerConfig

	exchange          types.Exchange
	orderQueryService types.ExchangeOrderQueryService
	market            types.Market
	targetPosition    fixedpoint.Value // positive = buy/long, negative = sell/short
	side              types.SideType   // derived from targetPosition sign

	// state
	state     TWAPWorkerState
	startTime time.Time
	endTime   time.Time

	placeOrderInterval   time.Duration
	currentIntervalStart time.Time
	currentIntervalEnd   time.Time
	lastCheckTime        time.Time

	filledQuantity fixedpoint.Value

	ctx       context.Context
	ctxCancel context.CancelFunc

	activeOrder *types.Order
	ordersMap   map[uint64]types.Order
	tradesMap   map[uint64]types.Trade

	notifyCallbacks []func(trade types.Trade)

	tradeC chan types.Trade

	logger logrus.FieldLogger
}

func NewTWAPWorker(
	exchange types.Exchange,
	market types.Market,
	config TWAPWorkerConfig,
) (*TWAPWorker, error) {
	orderQueryService, ok := exchange.(types.ExchangeOrderQueryService)
	if !ok {
		return nil, fmt.Errorf("exchange %T does not implement ExchangeOrderQueryService", exchange)
	}

	return &TWAPWorker{
		config:            config,
		exchange:          exchange,
		orderQueryService: orderQueryService,
		market:            market,
		state:             TWAPWorkerStatePending,
		targetPosition:    fixedpoint.Zero,
		filledQuantity:    fixedpoint.Zero,
		ordersMap:         make(map[uint64]types.Order),
		tradesMap:         make(map[uint64]types.Trade),
		tradeC:            make(chan types.Trade, 100),
	}, nil
}

// SetTargetPosition sets the target position for the TWAP worker.
// The caller can dynamically and monotonically enlarge the target position by calling this method.
// examples:
// current -> new
// 3 -> 4 (ok)
// -3 -> -4 (ok)
// 3 -> 2 (error, cannot shrink)
// -3 -> -2 (error, cannot shrink)
// 3 -> -3 (error, cannot switch side)
func (w *TWAPWorker) SetTargetPosition(targetPosition fixedpoint.Value) error {
	side := types.SideTypeBuy
	if targetPosition.Sign() < 0 {
		side = types.SideTypeSell
	}
	if w.side != "" && w.side != side {
		return fmt.Errorf("cannot switch side from %s to %s: TWAPWorker does not support side changes", w.side, side)
	}

	// if targetPosition is not zero, it must not shrink the existing target position by the size.
	if !w.targetPosition.IsZero() && w.targetPosition.Abs().Compare(targetPosition.Abs()) > 0 {
		return fmt.Errorf(
			"cannot shrink target position from %s to %s",
			w.targetPosition.String(),
			targetPosition.String(),
		)
	}

	w.side = side
	w.targetPosition = targetPosition
	return nil
}

func (w *TWAPWorker) SetLogger(logger logrus.FieldLogger) {
	w.logger = logger
}

func (w *TWAPWorker) State() TWAPWorkerState {
	w.Lock()
	defer w.Unlock()

	return w.state
}

func (w *TWAPWorker) IsDone() bool {
	w.Lock()
	defer w.Unlock()

	return w.state == TWAPWorkerStateDone
}

func (w *TWAPWorker) AveragePrice() fixedpoint.Value {
	w.Lock()
	defer w.Unlock()

	trades := w.trades()
	return tradingutil.AveragePriceFromTrades(trades)
}

func (w *TWAPWorker) FilledQuantity() fixedpoint.Value {
	w.Lock()
	defer w.Unlock()

	return w.filledQuantity
}

func (w *TWAPWorker) ActiveOrder() *types.Order {
	w.Lock()
	defer w.Unlock()

	return w.activeOrder
}

func (w *TWAPWorker) Trades() []types.Trade {
	w.Lock()
	defer w.Unlock()

	return w.trades()
}

func (w *TWAPWorker) trades() []types.Trade {
	var trades []types.Trade
	for _, t := range w.tradesMap {
		trades = append(trades, t)
	}
	return trades
}

func (w *TWAPWorker) CollectFees() map[string]fixedpoint.Value {
	w.Lock()
	defer w.Unlock()

	trades := w.trades()
	return tradingutil.CollectTradeFee(trades)
}

func (w *TWAPWorker) Orders() []types.Order {
	w.Lock()
	defer w.Unlock()

	var orders []types.Order
	for _, o := range w.ordersMap {
		orders = append(orders, o)
	}
	return orders
}

func (w *TWAPWorker) RemainingQuantity() fixedpoint.Value {
	w.Lock()
	defer w.Unlock()

	return w.remainingQuantity()
}

func (w *TWAPWorker) remainingQuantity() fixedpoint.Value {
	return w.targetPosition.Abs().Sub(w.filledQuantity)
}

func (w *TWAPWorker) TargetPosition() fixedpoint.Value {
	w.Lock()
	defer w.Unlock()

	return w.targetPosition
}

func (w *TWAPWorker) CurrentPosition() fixedpoint.Value {
	w.Lock()
	defer w.Unlock()

	return w.currentPosition()
}

func (w *TWAPWorker) currentPosition() fixedpoint.Value {
	position := w.filledQuantity
	if w.side == types.SideTypeSell {
		position = position.Neg()
	}

	return position
}

func (w *TWAPWorker) Start(ctx context.Context, currentTime time.Time) error {
	// worker should be able to start only once from pending state
	w.Lock()
	defer w.Unlock()

	if w.state != TWAPWorkerStatePending {
		return fmt.Errorf("cannot start TWAPWorker: expected state Pending, got %s", w.state)
	}

	if w.logger == nil {
		w.logger = logrus.WithFields(logrus.Fields{
			"component": "twap",
			"symbol":    w.market.Symbol,
			"side":      w.side.String(),
		})
	}

	w.state = TWAPWorkerStateRunning
	w.ctx, w.ctxCancel = context.WithCancel(ctx)
	w.startTime = currentTime
	w.endTime = currentTime.Add(w.config.Duration)

	numSlices := w.config.NumSlices
	if numSlices <= 0 {
		numSlices = 1
	}
	w.placeOrderInterval = w.config.Duration / time.Duration(numSlices)
	w.currentIntervalStart = currentTime
	w.currentIntervalEnd = w.currentIntervalStart.Add(w.placeOrderInterval)
	if w.currentIntervalEnd.After(w.endTime) {
		w.currentIntervalEnd = w.endTime
	}

	// start background goroutine to process trades
	go w.processTradeLoop()

	if w.targetPosition.IsZero() {
		w.logger.Info("[TWAP Start] started with zero target position, waiting for target update")
	} else {
		w.logger.Infof("[TWAP Start] started: targetPosition=%s, duration=%s, slices=%d, interval=%s",
			w.targetPosition.String(), w.config.Duration, numSlices, w.placeOrderInterval)
	}

	return nil
}

// Stop stops the TWAP worker and cancels any active order on the exchange.
func (w *TWAPWorker) Stop() {
	w.Lock()
	defer w.Unlock()

	defer w.ctxCancel()

	if w.state == TWAPWorkerStateRunning || w.state == TWAPWorkerStatePending {
		if w.activeOrder != nil {
			if err := w.exchange.CancelOrders(w.ctx, *w.activeOrder); err != nil {
				w.logger.WithError(err).Warn("[TWAP Stop] failed to cancel active order")
			}
		}

		// polling update other orders in the ordersMap
		for _, order := range w.ordersMap {
			updatedOrder, err := w.queryOrder(order.AsQuery())
			if err != nil || updatedOrder == nil {
				w.logger.WithError(err).Warnf("[TWAP Stop] failed to query order #%d state during cancel", order.OrderID)
				continue
			}
			w.ordersMap[order.OrderID] = *updatedOrder
		}

		// polling update trades for all orders
		for _, order := range w.ordersMap {
			trades, err := w.queryOrderTrades(order.AsQuery())
			if err != nil {
				w.logger.WithError(err).Warnf("[TWAP Stop] failed to query trades for order #%d during cancel", order.OrderID)
				continue
			}
			for _, trade := range trades {
				if _, exists := w.tradesMap[trade.ID]; !exists {
					w.tradesMap[trade.ID] = trade
					w.logger.Infof("[TWAP Stop] synced trade: %s", trade)
				}
			}
		}

		w.filledQuantity = tradingutil.AggregateTradesQuantity(w.trades())
		w.state = TWAPWorkerStateDone
		w.activeOrder = nil
		w.logger.Infof(
			"[TWAP Stop] stopped: filled=%s / target=%s",
			w.currentPosition(), w.targetPosition,
		)
	}
}

// HandleTrade enqueues a trade for asynchronous processing. It is non-blocking.
// Only trades belonging to orders created by this worker will be processed;
// others are silently ignored.
func (w *TWAPWorker) HandleTrade(trade types.Trade) {
	w.tradeC <- trade
}

func (w *TWAPWorker) processTradeLoop() {
	for {
		select {
		case <-w.ctx.Done():
			w.logger.Info("[TWAP processTradeLoop] trade loop context done, exiting")
			return
		case trade := <-w.tradeC:
			w.Lock()
			w.processTrade(trade)
			w.Unlock()
		}
	}
}

func (w *TWAPWorker) processTrade(trade types.Trade) {
	defer func() {
		w.filledQuantity = tradingutil.AggregateTradesQuantity(w.trades())
	}()

	// only process trades for orders created by this worker
	if _, ok := w.ordersMap[trade.OrderID]; !ok {
		return
	}

	// ignore duplicate trades
	if _, exists := w.tradesMap[trade.ID]; exists {
		return
	}

	// add to tradesMap
	w.tradesMap[trade.ID] = trade

	// stop processing if not running
	if w.state != TWAPWorkerStateRunning {
		// the worker is not started
		return
	}

	// update active order's executed quantity
	if w.activeOrder != nil && trade.OrderID == w.activeOrder.OrderID {
		w.activeOrder.ExecutedQuantity = w.activeOrder.ExecutedQuantity.Add(trade.Quantity)
		w.ordersMap[w.activeOrder.OrderID] = *w.activeOrder
		if w.activeOrder.ExecutedQuantity.Compare(w.activeOrder.Quantity) >= 0 {
			w.activeOrder.Status = types.OrderStatusFilled
			w.ordersMap[w.activeOrder.OrderID] = *w.activeOrder
		}
	}

	w.logger.Infof("[TWAP processTrade] trade processed: %s", trade)

	// notify callbacks
	w.EmitNotify(trade)
}

// syncAndResetActiveOrder queries the exchange for the latest order state and
// its trades via REST API, updates ordersMap and tradesMap accordingly, then
// resets activeOrder to nil. Must be called under lock.
func (w *TWAPWorker) syncAndResetActiveOrder() *types.Order {
	if w.activeOrder == nil {
		return nil
	}

	orderID := w.activeOrder.OrderID
	orderQuery := w.activeOrder.AsQuery()

	// query the latest order state
	updatedOrder, err := w.queryOrder(orderQuery)
	if err != nil || updatedOrder == nil {
		w.logger.WithError(err).Warnf("[TWAP syncAndResetActiveOrder] failed to query order #%d state, resetting active order without sync", orderID)
		w.activeOrder = nil
		return nil
	}

	// query trades for this order
	trades, err := w.queryOrderTrades(orderQuery)
	if err != nil {
		w.logger.WithError(err).Warnf("[TWAP syncAndResetActiveOrder] failed to query trades for order #%d", orderID)
	}

	for _, trade := range trades {
		if _, exists := w.tradesMap[trade.ID]; !exists {
			w.tradesMap[trade.ID] = trade
			w.logger.Infof("[TWAP syncAndResetActiveOrder] synced trade: %s", trade)
		}
	}
	w.ordersMap[orderID] = *updatedOrder
	w.filledQuantity = tradingutil.AggregateTradesQuantity(w.trades())

	oriActiveOrder := w.activeOrder
	w.activeOrder = nil
	return oriActiveOrder
}

// Tick is the main driver. It should be called on each external tick with the
// current time and an orderbook snapshot. It handles cancel-and-replace for
// maker orders, scheduling, quantity/price calculation, and order submission.
func (w *TWAPWorker) Tick(currentTime time.Time, orderBook types.OrderBook) error {
	w.Lock()
	defer w.Unlock()

	return w.tick(currentTime, orderBook)
}

func (w *TWAPWorker) tick(currentTime time.Time, orderBook types.OrderBook) error {
	defer func() {
		if currentTime.After(w.currentIntervalEnd) {
			w.currentIntervalStart = w.currentIntervalEnd
			w.currentIntervalEnd = w.currentIntervalStart.Add(w.placeOrderInterval)
			if w.currentIntervalStart.After(w.endTime) {
				w.currentIntervalStart = w.endTime
			}
			if w.currentIntervalEnd.After(w.endTime) {
				w.currentIntervalEnd = w.endTime
			}
		}
		if currentTime.After(w.endTime) {
			w.state = TWAPWorkerStateDone
		}
	}()

	if w.state != TWAPWorkerStateRunning {
		// the worker is not running
		return nil
	}

	if currentTime.Before(w.currentIntervalStart) {
		// not time for the next order yet
		return nil
	}

	// it's running and currentTime is after the current interval start
	// time to check if we need to place/cancel/replace orders
	market := w.market
	remaining := w.remainingQuantity()

	// target reached, do nothing
	if remaining.Sign() <= 0 {
		return nil
	}

	// check if deadline exceeded
	deadlineExceeded := !currentTime.Before(w.endTime)
	// if deadline exceeded, we want to place a final order for the remaining quantity
	if deadlineExceeded {
		if w.activeOrder != nil {
			if err := w.exchange.CancelOrders(w.ctx, *w.activeOrder); err != nil {
				w.logger.WithError(err).Warn("[TWAP tick] failed to cancel active order when deadline exceeded")
				return nil
			}
			w.syncAndResetActiveOrder()
		}
		createdOrder, err := w.placeOrder(w.remainingQuantity(), orderBook, true)
		if err != nil || createdOrder == nil {
			return fmt.Errorf("failed to place final order when deadline exceeded: %w", err)
		}
		w.activeOrder = createdOrder
		return nil
	}
	// from here, deadline not exceeded

	// we don't have an active order, place a new one
	if w.activeOrder == nil {
		createdOrder, err := w.placeOrder(remaining, orderBook, false)
		if err != nil || createdOrder == nil {
			return fmt.Errorf("failed to place order: %w", err)
		}
		w.activeOrder = createdOrder
		return nil
	}
	// from here, active order is not nil

	// we are within current interval and we have a better price
	if w.shouldUpdateActiveOrder(orderBook) && currentTime.Before(w.currentIntervalEnd) {
		// throttle order updates to avoid excessive cancel-and-replace
		if !w.lastCheckTime.IsZero() && currentTime.Sub(w.lastCheckTime) < w.config.CheckInterval {
			return nil
		}
		w.lastCheckTime = currentTime

		if err := w.exchange.CancelOrders(w.ctx, *w.activeOrder); err != nil {
			w.logger.WithError(err).Warn("[TWAP tick] failed to cancel active order")
			return nil
		}
		// find the better price and submit new order
		createdOrder, err := w.placeOrder(w.activeOrder.GetRemainingQuantity(), orderBook, false)
		if err != nil || createdOrder == nil {
			return fmt.Errorf("failed to place replacement order: %w", err)
		}
		oriActiveOrder := w.syncAndResetActiveOrder()
		executedQuantity := fixedpoint.Zero
		for _, trade := range w.trades() {
			if trade.OrderID == oriActiveOrder.OrderID {
				executedQuantity = executedQuantity.Add(trade.Quantity)
			}
		}
		oriActiveOrder.ExecutedQuantity = executedQuantity
		w.activeOrder = createdOrder
		w.ordersMap[createdOrder.OrderID] = *createdOrder
		w.logger.Infof("[TWAP tick] active order updated: %s %s qty=%s(executed: %s)->%s price=%s->%s",
			createdOrder.Side,
			createdOrder.Type,
			oriActiveOrder.Quantity,
			oriActiveOrder.ExecutedQuantity,
			createdOrder.Quantity,
			oriActiveOrder.Price,
			createdOrder.Price,
		)
		return nil
	}

	// we are within the current interval, just wait for the next tick
	if currentTime.Before(w.currentIntervalEnd) {
		return nil
	}

	// currentTime is after current interval end, time to place the next slice order
	// calculate slice quantity
	sliceQty := w.calculateSliceQuantity(currentTime, remaining, deadlineExceeded)
	sliceQty = market.TruncateQuantity(sliceQty)

	createdOrder, err := w.placeOrder(sliceQty, orderBook, false)
	if err != nil || createdOrder == nil {
		return fmt.Errorf("failed to place order for next slice: %w", err)
	}
	w.activeOrder = createdOrder

	return nil
}

func (w *TWAPWorker) placeOrder(quantity fixedpoint.Value, orderBook types.OrderBook, deadlineExceeded bool) (*types.Order, error) {
	// find the better price and submit new order
	price, err := w.getPrice(orderBook)
	if err != nil {
		w.logger.WithError(err).Warn("[TWAP tick] failed to get price for active order update")
		return nil, err
	}
	price = w.market.TruncatePrice(price)
	order := w.buildSubmitOrder(quantity, price, deadlineExceeded)
	if w.market.IsDustQuantity(order.Quantity, order.Price) {
		return nil, fmt.Errorf("order is of dust quantity: %s", quantity)
	}
	createdOrder, err := w.exchange.SubmitOrder(w.ctx, order)
	if createdOrder != nil {
		w.ordersMap[createdOrder.OrderID] = *createdOrder
	}
	return createdOrder, err
}

func (w *TWAPWorker) calculateSliceQuantity(currentTime time.Time, remaining fixedpoint.Value, deadlineExceeded bool) fixedpoint.Value {
	if deadlineExceeded {
		return remaining
	}

	// dynamic slice: remaining / remaining_slices
	timeLeft := w.endTime.Sub(currentTime)
	if timeLeft <= 0 {
		return remaining
	}

	remainingSlices := int(timeLeft / w.placeOrderInterval)
	if remainingSlices <= 0 {
		remainingSlices = 1
	}

	sliceQty := remaining.Div(fixedpoint.NewFromInt(int64(remainingSlices)))

	// apply min/max slice size constraints
	if w.config.MaxSliceSize.Sign() > 0 && sliceQty.Compare(w.config.MaxSliceSize) > 0 {
		sliceQty = w.config.MaxSliceSize
	}
	if w.config.MinSliceSize.Sign() > 0 && sliceQty.Compare(w.config.MinSliceSize) < 0 {
		// if remaining is less than min, just use remaining
		if remaining.Compare(w.config.MinSliceSize) <= 0 {
			sliceQty = remaining
		} else {
			sliceQty = w.config.MinSliceSize
		}
	}

	// cap at remaining
	if sliceQty.Compare(remaining) > 0 {
		sliceQty = remaining
	}

	return sliceQty
}

func (w *TWAPWorker) getPrice(orderBook types.OrderBook) (fixedpoint.Value, error) {
	switch w.config.OrderType {
	case TWAPOrderTypeTaker:
		return w.getTakerPrice(orderBook)
	case TWAPOrderTypeMaker:
		return w.getMakerPrice(orderBook)
	default:
		return w.getTakerPrice(orderBook)
	}
}

func (w *TWAPWorker) getTakerPrice(orderBook types.OrderBook) (fixedpoint.Value, error) {
	switch w.side {
	case types.SideTypeBuy:
		ask, ok := orderBook.BestAsk()
		if !ok {
			return fixedpoint.Zero, fmt.Errorf("no ask price available for %s", w.market.Symbol)
		}
		price := ask.Price
		if w.config.MaxSlippage.Sign() > 0 {
			maxPrice := price.Mul(fixedpoint.One.Add(w.config.MaxSlippage))
			price = fixedpoint.Min(price, maxPrice)
		}
		return price, nil

	case types.SideTypeSell:
		bid, ok := orderBook.BestBid()
		if !ok {
			return fixedpoint.Zero, fmt.Errorf("no bid price available for %s", w.market.Symbol)
		}
		price := bid.Price
		if w.config.MaxSlippage.Sign() > 0 {
			minPrice := price.Mul(fixedpoint.One.Sub(w.config.MaxSlippage))
			price = fixedpoint.Max(price, minPrice)
		}
		return price, nil

	default:
		return fixedpoint.Zero, fmt.Errorf("unknown side: %s", w.side)
	}
}

func (w *TWAPWorker) getMakerPrice(orderBook types.OrderBook) (fixedpoint.Value, error) {
	tickSize := w.market.TickSize
	numOfTicks := fixedpoint.NewFromInt(int64(w.config.NumOfTicks))
	tickImprovement := tickSize.Mul(numOfTicks)

	switch w.side {
	case types.SideTypeBuy:
		bid, ok := orderBook.BestBid()
		if !ok {
			return fixedpoint.Zero, fmt.Errorf("no bid price available for %s", w.market.Symbol)
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
			return fixedpoint.Zero, fmt.Errorf("no ask price available for %s", w.market.Symbol)
		}
		price := ask.Price.Sub(tickImprovement)
		bid, hasBid := orderBook.BestBid()
		if hasBid && price.Compare(bid.Price) <= 0 {
			price = bid.Price.Add(tickSize)
		}
		return price, nil

	default:
		return fixedpoint.Zero, fmt.Errorf("unknown side: %s", w.side)
	}
}

// shouldUpdateActiveOrder checks whether the active order should be canceled and replaced
// with a better price. For taker orders (IOC), always update. For maker orders,
// compare the current order price against the best computed maker price.
func (w *TWAPWorker) shouldUpdateActiveOrder(orderBook types.OrderBook) bool {
	if w.activeOrder == nil {
		return false
	}

	// taker orders are IOC — always refresh
	if w.config.OrderType == TWAPOrderTypeTaker {
		return true
	}

	newPrice, err := w.getPrice(orderBook)
	if err != nil {
		w.logger.WithError(err).Warn("[TWAP shouldUpdateOrder] failed to get price for order update check")
		return false
	}

	newPrice = w.market.TruncatePrice(newPrice)
	newPriceBtter := false
	switch w.side {
	case types.SideTypeBuy:
		newPriceBtter = newPrice.Compare(w.activeOrder.Price) > 0
	case types.SideTypeSell:
		newPriceBtter = newPrice.Compare(w.activeOrder.Price) < 0
	}
	w.logger.Infof("[TWAP shouldUpdateOrder] order update check: current price=%s, new price=%s, better=%t",
		w.activeOrder.Price.String(), newPrice.String(), newPriceBtter)
	return newPriceBtter
}

func (w *TWAPWorker) buildSubmitOrder(quantity, price fixedpoint.Value, deadlineExceeded bool) types.SubmitOrder {
	orderType := types.OrderTypeLimitMaker
	timeInForce := types.TimeInForceGTC

	if w.config.OrderType == TWAPOrderTypeTaker || deadlineExceeded {
		orderType = types.OrderTypeLimit
		timeInForce = types.TimeInForceIOC
	}

	return types.SubmitOrder{
		Symbol:      w.market.Symbol,
		Market:      w.market,
		Side:        w.side,
		Type:        orderType,
		Quantity:    quantity,
		Price:       price,
		TimeInForce: timeInForce,
	}
}

func (w *TWAPWorker) queryOrder(query types.OrderQuery) (*types.Order, error) {
	// add timeout to avoid blocking the event loop too long
	ctxTimeout, cancel := context.WithTimeout(w.ctx, 500*time.Millisecond)
	defer cancel()

	return w.orderQueryService.QueryOrder(ctxTimeout, query)
}

func (w *TWAPWorker) queryOrderTrades(query types.OrderQuery) ([]types.Trade, error) {
	// add timeout to avoid blocking the event loop too long
	ctxTimeout, cancel := context.WithTimeout(w.ctx, 500*time.Millisecond)
	defer cancel()

	return w.orderQueryService.QueryOrderTrades(ctxTimeout, query)
}
