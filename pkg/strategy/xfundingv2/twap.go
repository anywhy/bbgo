package xfundingv2

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/c9s/bbgo/pkg/bbgo"
	"github.com/c9s/bbgo/pkg/fixedpoint"
	"github.com/c9s/bbgo/pkg/types"
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
	// Duration is the total time duration for the TWAP execution
	Duration time.Duration `json:"duration"`
	// NumSlices is how many slices to divide the total time duration into.
	NumSlices int `json:"numSlices"`
	// OrderType specifies whether to use maker or taker orders for execution.
	OrderType TWAPOrderType `json:"orderType"`
	// CheckInterval is how often to check for price improvement for the active order.
	CheckInterval time.Duration `json:"checkInterval,omitempty"`

	// optional configs
	// MaxSlippage is the maximum slippage ratio for taker orders (e.g. 0.001 = 0.1%)
	MaxSlippage fixedpoint.Value `json:"maxSlippage,omitempty"`
	// MaxSliceSize is the maximum quantity for each slice order
	MaxSliceSize fixedpoint.Value `json:"maxSliceSize,omitempty"`
	// MinSliceSize is the minimum quantity for each slice order
	MinSliceSize fixedpoint.Value `json:"minSliceSize,omitempty"`
	// NumOfTicks is for orders: number of ticks to improve price, maker only
	NumOfTicks int `json:"numOfTicks,omitempty"`
}

//go:generate callbackgen -type=TWAPWorker -output=twap_callbacks.go
type TWAPWorker struct {
	// sync.Mutex protects fields mutated by the background trade goroutine:
	// filledQuantity, activeOrder, trades, state
	mu sync.Mutex

	config TWAPWorkerConfig

	targetPosition fixedpoint.Value // positive = buy/long, negative = sell/short
	side           types.SideType   // derived from targetPosition sign

	// state
	state     TWAPWorkerState
	startTime time.Time
	endTime   time.Time

	placeOrderInterval   time.Duration
	currentIntervalStart time.Time
	currentIntervalEnd   time.Time
	lastCheckTime        time.Time

	symbol   string
	position *types.Position

	ctx       context.Context
	ctxCancel context.CancelFunc

	activeOrder  *types.Order
	twapExecutor *TWAPExecutor

	tradeCallbacks []func(trade types.Trade)

	logger logrus.FieldLogger
}

func NewTWAPWorker(
	ctx context.Context,
	symbol string,
	session *bbgo.ExchangeSession,
	generalExecutor *bbgo.GeneralOrderExecutor,
	config TWAPWorkerConfig,
) (*TWAPWorker, error) {
	market, found := session.Market(symbol)
	if !found {
		return nil, fmt.Errorf("market not found for symbol: %s", symbol)
	}
	service, ok := session.Exchange.(types.ExchangeOrderQueryService)
	if !ok {
		return nil, fmt.Errorf("exchange does not support OrderQueryService: %s", session.Exchange.Name())
	}
	w := &TWAPWorker{
		config:         config,
		symbol:         symbol,
		position:       generalExecutor.Position(),
		state:          TWAPWorkerStatePending,
		targetPosition: fixedpoint.Zero,
		side:           types.SideTypeNone,
	}
	w.ctx, w.ctxCancel = context.WithCancel(ctx)
	w.twapExecutor = NewTWAPOrderExecutor(
		w.ctx,
		service,
		market,
		generalExecutor,
		config,
	)
	return w, nil
}

// SetTargetPosition sets the target position for the TWAP worker.
func (w *TWAPWorker) SetTargetPosition(targetPosition fixedpoint.Value) {
	targetDiff := targetPosition.Sub(w.position.Base)
	w.targetPosition = targetPosition
	if targetDiff.Sign() > 0 {
		w.side = types.SideTypeBuy
	} else if targetDiff.Sign() < 0 {
		w.side = types.SideTypeSell
	}
}

func (w *TWAPWorker) SetLogger(logger logrus.FieldLogger) {
	w.logger = logger
}

func (w *TWAPWorker) State() TWAPWorkerState {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.state
}

func (w *TWAPWorker) IsDone() bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.state == TWAPWorkerStateDone
}

func (w *TWAPWorker) AveragePrice() fixedpoint.Value {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.position.AverageCost
}

func (w *TWAPWorker) FilledQuantity() fixedpoint.Value {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.position.Base
}

func (w *TWAPWorker) ActiveOrder() *types.Order {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.activeOrder
}

func (w *TWAPWorker) Trades() []types.Trade {
	return w.twapExecutor.Trades()
}

func (w *TWAPWorker) TotalFee() map[string]fixedpoint.Value {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.position.TotalFee
}

func (w *TWAPWorker) RemainingQuantity() fixedpoint.Value {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.remainingQuantity()
}

func (w *TWAPWorker) remainingQuantity() fixedpoint.Value {
	// remaining = target - filled
	// NOTE: the remaining quantity can be positive or negative.
	return w.targetPosition.Sub(w.position.Base)
}

func (w *TWAPWorker) TargetPosition() fixedpoint.Value {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.targetPosition
}

func (w *TWAPWorker) Position() *types.Position {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.position
}

func (w *TWAPWorker) Start(ctx context.Context, currentTime time.Time) error {
	// worker should be able to start only once from pending state
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.state != TWAPWorkerStatePending {
		return fmt.Errorf("cannot start TWAPWorker: expected state Pending, got %s", w.state)
	}

	if w.logger == nil {
		w.logger = logrus.WithFields(logrus.Fields{
			"component": "twap",
			"symbol":    w.symbol,
			"side":      w.side.String(),
		})
	}

	// start the executor
	w.twapExecutor.SetLogger(w.logger)
	w.twapExecutor.Start()

	w.resetTime(currentTime, w.config.Duration)

	w.logger.Infof(
		"[TWAP Start] started: targetPosition=%s, duration=%s, interval=%s",
		w.targetPosition,
		w.config.Duration,
		w.placeOrderInterval,
	)
	return nil
}

func (w *TWAPWorker) RemainingDuration(currentTime time.Time) time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()

	if currentTime.After(w.endTime) {
		return 0
	}
	return w.endTime.Sub(currentTime)
}

// ResetTime resets the start and end time of the TWAP execution.
// It can be used to extend the execution time by resetting the end time to a later time.
// ex: TWAP worker is opening a position and then switch to closing the position, we can reset the time for the closing.
func (w *TWAPWorker) ResetTime(currentTime time.Time, duration time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.resetTime(currentTime, duration)
}

func (w *TWAPWorker) resetTime(currentTime time.Time, duration time.Duration) {
	w.state = TWAPWorkerStateRunning
	w.startTime = currentTime
	w.config.Duration = duration
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

	w.syncAndResetActiveOrder()
}

// Stop stops the TWAP worker and cancels any active order on the exchange.
func (w *TWAPWorker) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()

	defer w.ctxCancel()

	if w.state == TWAPWorkerStateRunning || w.state == TWAPWorkerStatePending {
		if w.activeOrder != nil {
			err := w.twapExecutor.CancelOrder(w.ctx, *w.activeOrder)
			if err != nil {
				w.logger.WithError(err).Warnf("[TWAP Stop] failed to cancel active order: %s", w.activeOrder)
			}
		}

		// stop executor
		w.twapExecutor.Stop()

		w.state = TWAPWorkerStateDone
		w.activeOrder = nil
		w.logger.Infof(
			"[TWAP Stop] stopped: filled=%s / target=%s",
			w.position.Base, w.targetPosition,
		)
	}
}

// syncAndResetActiveOrder queries the exchange for the latest order state and
// its trades via REST API, updates ordersMap and tradesMap accordingly, then
// resets activeOrder to nil. Must be called under lock.
func (w *TWAPWorker) syncAndResetActiveOrder() *types.Order {
	if w.activeOrder == nil {
		return nil
	}

	if err := w.twapExecutor.SyncOrder(*w.activeOrder); err != nil {
		w.logger.WithError(err).Warnf("[TWAP syncAndResetActiveOrder] fail to sync active order, resetting: %s", w.activeOrder.String())
		w.activeOrder = nil
		return nil
	}

	oriActiveOrder, _ := w.twapExecutor.GetOrder(w.activeOrder.OrderID)
	w.activeOrder = nil
	return &oriActiveOrder
}

// Tick is the main driver. It should be called on each external tick with the
// current time and an orderbook snapshot. It handles cancel-and-replace for
// maker orders, scheduling, quantity/price calculation, and order submission.
func (w *TWAPWorker) Tick(currentTime time.Time, orderBook types.OrderBook) error {
	w.mu.Lock()
	defer w.mu.Unlock()

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
	remaining := w.remainingQuantity().Abs()

	// target reached, do nothing
	if remaining.IsZero() {
		return nil
	}

	// check if deadline exceeded
	deadlineExceeded := !currentTime.Before(w.endTime)
	// if deadline exceeded, we want to place a final order for the remaining quantity
	if deadlineExceeded {
		if w.activeOrder != nil {
			if err := w.twapExecutor.CancelOrder(w.ctx, *w.activeOrder); err != nil {
				w.logger.WithError(err).Warn("[TWAP tick] failed to cancel active order when deadline exceeded")
				return nil
			}
			w.syncAndResetActiveOrder()
		}
		createdOrder, err := w.twapExecutor.PlaceOrder(
			remaining,
			w.side,
			orderBook,
			true,
		)
		if err != nil || createdOrder == nil {
			return fmt.Errorf("failed to place final order when deadline exceeded: %w", err)
		}
		w.activeOrder = createdOrder
		return nil
	}
	// from here, deadline not exceeded

	// we don't have an active order, place a new one
	if w.activeOrder == nil {
		sliceQty := w.calculateSliceQuantity(currentTime, remaining, false)
		createdOrder, err := w.twapExecutor.PlaceOrder(sliceQty, w.side, orderBook, false)
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

		if err := w.twapExecutor.CancelOrder(w.ctx, *w.activeOrder); err != nil {
			w.logger.WithError(err).Warn("[TWAP tick] failed to cancel active order")
			return nil
		}
		// find the better price and submit new order
		createdOrder, err := w.twapExecutor.PlaceOrder(
			w.activeOrder.GetRemainingQuantity(),
			w.activeOrder.Side,
			orderBook,
			deadlineExceeded,
		)
		if err != nil || createdOrder == nil {
			return fmt.Errorf("failed to place replacement order: %w", err)
		}
		oriActiveOrder := w.syncAndResetActiveOrder()
		w.activeOrder = createdOrder
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
	createdOrder, err := w.twapExecutor.PlaceOrder(sliceQty, w.side, orderBook, deadlineExceeded)
	if err != nil || createdOrder == nil {
		return fmt.Errorf("failed to place order for next slice: %w", err)
	}
	w.activeOrder = createdOrder

	return nil
}

func (w *TWAPWorker) calculateSliceQuantity(currentTime time.Time, remaining fixedpoint.Value, deadlineExceeded bool) fixedpoint.Value {
	remaining = remaining.Abs()

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

	newPrice, err := w.twapExecutor.GetPrice(w.side, orderBook)
	if err != nil {
		w.logger.WithError(err).Warn("[TWAP shouldUpdateOrder] failed to get price for order update check")
		return false
	}

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
