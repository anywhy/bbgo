package xfundingv2

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"

	"github.com/c9s/bbgo/pkg/fixedpoint"
	. "github.com/c9s/bbgo/pkg/testing/testhelper"
	"github.com/c9s/bbgo/pkg/types"
	"github.com/c9s/bbgo/pkg/types/mocks"
)

var testLogger = logrus.WithField("test", "twap")

var testMarket = Market("BTCUSDT")

func newTestOrderBook(bestBid, bestAsk float64) *types.SliceOrderBook {
	return &types.SliceOrderBook{
		Symbol: "BTCUSDT",
		Bids: types.PriceVolumeSlice{
			{Price: Number(bestBid), Volume: Number(10)},
		},
		Asks: types.PriceVolumeSlice{
			{Price: Number(bestAsk), Volume: Number(10)},
		},
	}
}

func waitForTrades(trades *sync.Map, id uint64) {
	_, found := trades.Load(id)
	for !found {
		time.Sleep(10 * time.Millisecond)
		_, found = trades.Load(id)
	}
}

// expectSyncActiveOrder sets up mock expectations for QueryOrder and QueryOrderTrades
// that syncAndResetActiveOrder will call after canceling an active order.
func expectSyncActiveOrder(mockEx *mocks.MockExchangeExtended, order *types.Order) {
	mockEx.EXPECT().QueryOrder(gomock.Any(), gomock.Any()).Return(order, nil)
	mockEx.EXPECT().QueryOrderTrades(gomock.Any(), gomock.Any()).Return(nil, nil)
}

func TestTWAPWorker_TakerBasic(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockEx := mocks.NewMockExchangeExtended(ctrl)

	config := TWAPWorkerConfig{
		Duration:  10 * time.Minute,
		NumSlices: 5,
		OrderType: TWAPOrderTypeTaker,
	}

	worker, err := NewTWAPWorker(mockEx, testMarket, config)
	assert.NoError(t, err)
	worker.SetLogger(testLogger)
	assert.NoError(t, worker.SetTargetPosition(Number(1.0)))
	assert.Equal(t, TWAPWorkerStatePending, worker.State())

	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	assert.NoError(t, worker.Start(ctx, now))
	assert.Equal(t, TWAPWorkerStateRunning, worker.State())
	assert.Equal(t, 2*time.Minute, worker.placeOrderInterval)

	ob := newTestOrderBook(50000.0, 50010.0)

	orderID := uint64(1)
	// first tick: no active order → place order for full remaining quantity
	mockEx.EXPECT().SubmitOrder(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, order types.SubmitOrder) (*types.Order, error) {
			assert.Equal(t, types.SideTypeBuy, order.Side)
			assert.Equal(t, types.OrderTypeLimit, order.Type)
			assert.Equal(t, types.TimeInForceIOC, order.TimeInForce)
			// taker buy uses best ask price
			assert.Equal(t, Number(50010.0), order.Price)
			// full remaining (no slicing on initial placement)
			assert.Equal(t, Number(1.0), order.Quantity)
			return &types.Order{
				SubmitOrder:      order,
				OrderID:          orderID,
				ExecutedQuantity: fixedpoint.Zero,
			}, nil
		},
	)

	err = worker.Tick(now, ob)
	assert.NoError(t, err)
	assert.NotNil(t, worker.ActiveOrder())
	assert.Len(t, worker.Orders(), 1)

	// simulate partial fill (0.2 of 1.0)
	trades := &sync.Map{}
	worker.OnNotify(func(trade types.Trade) {
		trades.Store(trade.ID, struct{}{})
	})
	worker.HandleTrade(types.Trade{
		ID:       1,
		OrderID:  orderID,
		Quantity: Number(0.2),
		Price:    Number(50010.0),
	})
	waitForTrades(trades, 1)
	// activeOrder stays non-nil
	assert.NotNil(t, worker.ActiveOrder())
	assert.Equal(t, Number(0.2), worker.FilledQuantity())
	assert.Equal(t, Number(0.8), worker.RemainingQuantity())
	assert.Len(t, worker.Trades(), 1)

	// trade for unknown order should be ignored (processTrade skips it, no notify)
	worker.HandleTrade(types.Trade{
		ID:       2,
		OrderID:  9999,
		Quantity: Number(1.0),
		Price:    Number(50010.0),
	})
	// give the background goroutine a moment to process (it will skip this trade)
	time.Sleep(50 * time.Millisecond)
	assert.Len(t, worker.Trades(), 1)
	assert.Equal(t, Number(0.2), worker.FilledQuantity())

	// second tick at +1min: within interval [0, 2min), taker → cancel-and-replace active order
	orderID = 2
	mockEx.EXPECT().CancelOrders(gomock.Any(), gomock.Any()).Return(nil)
	mockEx.EXPECT().SubmitOrder(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, order types.SubmitOrder) (*types.Order, error) {
			// replacement for remaining on active order: 1.0 - 0.2 = 0.8
			assert.Equal(t, Number(0.8), order.Quantity)
			return &types.Order{
				SubmitOrder:      order,
				OrderID:          orderID,
				ExecutedQuantity: fixedpoint.Zero,
			}, nil
		},
	)
	expectSyncActiveOrder(mockEx, &types.Order{
		OrderID:          1,
		ExecutedQuantity: Number(0.2),
		SubmitOrder: types.SubmitOrder{
			Quantity: Number(1.0),
		},
	})

	err = worker.Tick(now.Add(1*time.Minute), ob)
	assert.NoError(t, err)
	assert.Len(t, worker.Orders(), 2)

	// simulate partial fill of second order
	worker.HandleTrade(types.Trade{
		ID:       3,
		OrderID:  orderID,
		Quantity: Number(0.2),
		Price:    Number(50010.0),
	})
	waitForTrades(trades, 3)
	assert.Equal(t, Number(0.4), worker.FilledQuantity())

	// third tick at +2min+1ns: past interval end [0, 2min), enters "next slice" path
	// calculates slice: remaining=0.6, timeLeft≈8min, slices=3, qty=0.6/3=0.2
	orderID = 3
	mockEx.EXPECT().SubmitOrder(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, order types.SubmitOrder) (*types.Order, error) {
			assert.Equal(t, Number(0.2), order.Quantity)
			return &types.Order{
				SubmitOrder:      order,
				OrderID:          orderID,
				ExecutedQuantity: fixedpoint.Zero,
			}, nil
		},
	)

	err = worker.Tick(now.Add(2*time.Minute+time.Nanosecond), ob)
	assert.NoError(t, err)
	assert.Len(t, worker.Orders(), 3)
}

func TestTWAPWorker_MakerCancelAndReplace(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockEx := mocks.NewMockExchangeExtended(ctrl)

	config := TWAPWorkerConfig{
		Duration:   4 * time.Minute,
		NumSlices:  2,
		OrderType:  TWAPOrderTypeMaker,
		NumOfTicks: 1,
	}

	worker, err := NewTWAPWorker(mockEx, testMarket, config)
	assert.NoError(t, err)
	worker.SetLogger(testLogger)
	assert.NoError(t, worker.SetTargetPosition(Number(-0.5)))
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	assert.NoError(t, worker.Start(ctx, now))

	ob := newTestOrderBook(50000.0, 50010.0)

	// first tick: place maker sell order
	var firstOrder *types.Order
	mockEx.EXPECT().SubmitOrder(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, order types.SubmitOrder) (*types.Order, error) {
			assert.Equal(t, types.SideTypeSell, order.Side)
			assert.Equal(t, types.OrderTypeLimitMaker, order.Type)
			// maker sell: best ask (50010) - 1 tick (0.01) = 50009.99
			assert.Equal(t, Number(50009.99), order.Price)
			// full remaining quantity
			assert.Equal(t, Number(0.5), order.Quantity)
			firstOrder = &types.Order{
				SubmitOrder:      order,
				OrderID:          1,
				ExecutedQuantity: fixedpoint.Zero,
			}
			return firstOrder, nil
		},
	)

	err = worker.Tick(now, ob)
	assert.NoError(t, err)
	assert.NotNil(t, worker.ActiveOrder())

	// second tick at +1min: within interval [0, 2min), price moved DOWN (better for sell)
	// shouldUpdateActiveOrder returns true because new price < current price
	mockEx.EXPECT().CancelOrders(gomock.Any(), gomock.Any()).Return(nil)

	// new orderbook with lower ask — better price for sell
	ob2 := newTestOrderBook(49900.0, 49910.0)

	// after cancel, place replacement order, then syncAndResetActiveOrder
	mockEx.EXPECT().SubmitOrder(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, order types.SubmitOrder) (*types.Order, error) {
			// maker sell: best ask (49910) - 1 tick (0.01) = 49909.99
			assert.Equal(t, Number(49909.99), order.Price)
			// remaining on active order: 0.5 - 0 = 0.5
			assert.Equal(t, Number(0.5), order.Quantity)
			return &types.Order{
				SubmitOrder:      order,
				OrderID:          2,
				ExecutedQuantity: fixedpoint.Zero,
			}, nil
		},
	)
	expectSyncActiveOrder(mockEx, firstOrder)

	err = worker.Tick(now.Add(1*time.Minute), ob2)
	assert.NoError(t, err)
}

func TestTWAPWorker_PartialFillAdjusts(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockEx := mocks.NewMockExchangeExtended(ctrl)

	config := TWAPWorkerConfig{
		Duration:  4 * time.Minute,
		NumSlices: 2,
		OrderType: TWAPOrderTypeTaker,
	}

	worker, err := NewTWAPWorker(mockEx, testMarket, config)
	assert.NoError(t, err)
	worker.SetLogger(testLogger)
	assert.NoError(t, worker.SetTargetPosition(Number(1.0)))
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	assert.NoError(t, worker.Start(ctx, now))

	ob := newTestOrderBook(50000.0, 50010.0)

	// first order: full remaining quantity
	mockEx.EXPECT().SubmitOrder(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, order types.SubmitOrder) (*types.Order, error) {
			assert.Equal(t, Number(1.0), order.Quantity)
			return &types.Order{
				SubmitOrder:      order,
				OrderID:          1,
				ExecutedQuantity: fixedpoint.Zero,
			}, nil
		},
	)

	err = worker.Tick(now, ob)
	assert.NoError(t, err)

	// partial fill: only 0.3 of 1.0 filled
	trades := &sync.Map{}
	worker.OnNotify(func(trade types.Trade) {
		trades.Store(trade.ID, struct{}{})
	})
	worker.HandleTrade(types.Trade{
		ID:       1,
		OrderID:  1,
		Quantity: Number(0.3),
		Price:    Number(50010.0),
	})
	waitForTrades(trades, 1)
	assert.Equal(t, Number(0.3), worker.FilledQuantity())
	// active order still exists (0.7 remaining on order)
	assert.NotNil(t, worker.ActiveOrder())

	// tick at +1min: within interval [0, 2min), active order exists
	// shouldUpdateActiveOrder returns true for taker, cancel-and-replace happens
	mockEx.EXPECT().CancelOrders(gomock.Any(), gomock.Any()).Return(nil)
	mockEx.EXPECT().SubmitOrder(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, order types.SubmitOrder) (*types.Order, error) {
			// replacement for remaining on active order: 1.0 - 0.3 = 0.7
			assert.Equal(t, Number(0.7), order.Quantity)
			return &types.Order{
				SubmitOrder:      order,
				OrderID:          2,
				ExecutedQuantity: fixedpoint.Zero,
			}, nil
		},
	)
	expectSyncActiveOrder(mockEx, &types.Order{
		OrderID:          1,
		ExecutedQuantity: Number(0.3),
		SubmitOrder: types.SubmitOrder{
			Quantity: Number(1.0),
		},
	})

	err = worker.Tick(now.Add(1*time.Minute), ob)
	assert.NoError(t, err)
}

func TestTWAPWorker_DeadlineMarketOrder(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockEx := mocks.NewMockExchangeExtended(ctrl)

	config := TWAPWorkerConfig{
		Duration:  2 * time.Minute,
		NumSlices: 2,
		OrderType: TWAPOrderTypeMaker,
	}

	worker, err := NewTWAPWorker(mockEx, testMarket, config)
	assert.NoError(t, err)
	worker.SetLogger(testLogger)
	assert.NoError(t, worker.SetTargetPosition(Number(-0.5)))
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	assert.NoError(t, worker.Start(ctx, now))

	ob := newTestOrderBook(50000.0, 50010.0)

	// first tick: normal maker order for full remaining
	var firstOrder *types.Order
	mockEx.EXPECT().SubmitOrder(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, order types.SubmitOrder) (*types.Order, error) {
			assert.Equal(t, Number(0.5), order.Quantity)
			firstOrder = &types.Order{
				SubmitOrder:      order,
				OrderID:          1,
				ExecutedQuantity: fixedpoint.Zero,
			}
			return firstOrder, nil
		},
	)
	err = worker.Tick(now, ob)
	assert.NoError(t, err)

	// simulate partial fill
	trades := &sync.Map{}
	worker.OnNotify(func(trade types.Trade) {
		trades.Store(trade.ID, struct{}{})
	})
	worker.HandleTrade(types.Trade{
		ID:       1,
		OrderID:  1,
		Quantity: Number(0.1),
		Price:    Number(50010.0),
	})
	waitForTrades(trades, 1)

	// tick at deadline: cancel active order, sync, then use IOC limit order for remaining
	mockEx.EXPECT().CancelOrders(gomock.Any(), gomock.Any()).Return(nil)
	expectSyncActiveOrder(mockEx, firstOrder)
	mockEx.EXPECT().SubmitOrder(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, order types.SubmitOrder) (*types.Order, error) {
			// should be IOC limit for remaining 0.4
			assert.Equal(t, types.OrderTypeLimit, order.Type)
			assert.Equal(t, types.TimeInForceIOC, order.TimeInForce)
			assert.Equal(t, Number(0.4), order.Quantity)
			return &types.Order{
				SubmitOrder:      order,
				OrderID:          2,
				ExecutedQuantity: fixedpoint.Zero,
			}, nil
		},
	)

	// tick slightly past deadline to trigger state=Done via deferred
	err = worker.Tick(now.Add(2*time.Minute+time.Nanosecond), ob)
	assert.NoError(t, err)
	assert.True(t, worker.IsDone())
}

func TestTWAPWorker_Cancel(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockEx := mocks.NewMockExchangeExtended(ctrl)

	config := TWAPWorkerConfig{
		Duration:  10 * time.Minute,
		NumSlices: 5,
		OrderType: TWAPOrderTypeTaker,
	}

	worker, err := NewTWAPWorker(mockEx, testMarket, config)
	assert.NoError(t, err)
	worker.SetLogger(testLogger)
	assert.NoError(t, worker.SetTargetPosition(Number(1.0)))
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	assert.NoError(t, worker.Start(ctx, now))

	worker.Stop()
	assert.Equal(t, TWAPWorkerStateDone, worker.State())
	assert.True(t, worker.IsDone())

	// tick after cancel should be no-op
	ob := newTestOrderBook(50000.0, 50010.0)
	err = worker.Tick(now.Add(time.Minute), ob)
	assert.NoError(t, err)
}

func TestTWAPWorker_DustQuantityStaysRunning(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockEx := mocks.NewMockExchangeExtended(ctrl)

	config := TWAPWorkerConfig{
		Duration:  4 * time.Minute,
		NumSlices: 2,
		OrderType: TWAPOrderTypeTaker,
	}

	worker, err := NewTWAPWorker(mockEx, testMarket, config)
	assert.NoError(t, err)
	worker.SetLogger(testLogger)
	assert.NoError(t, worker.SetTargetPosition(Number(0.5)))
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	assert.NoError(t, worker.Start(ctx, now))

	ob := newTestOrderBook(50000.0, 50010.0)

	// first order
	mockEx.EXPECT().SubmitOrder(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, order types.SubmitOrder) (*types.Order, error) {
			return &types.Order{
				SubmitOrder:      order,
				OrderID:          1,
				ExecutedQuantity: fixedpoint.Zero,
			}, nil
		},
	)
	err = worker.Tick(now, ob)
	assert.NoError(t, err)

	// fill almost everything, leaving dust (< MinQuantity 0.001)
	trades := &sync.Map{}
	worker.OnNotify(func(trade types.Trade) {
		trades.Store(trade.ID, struct{}{})
	})
	worker.HandleTrade(types.Trade{
		ID:       1,
		OrderID:  1,
		Quantity: Number(0.4995),
		Price:    Number(50010.0),
	})
	waitForTrades(trades, 1)

	// next tick at +2min: remaining 0.0005 is dust → placeOrder returns error, but worker stays running
	err = worker.Tick(now.Add(2*time.Minute), ob)
	assert.Error(t, err)
	assert.Equal(t, TWAPWorkerStateRunning, worker.State())
}

func TestTWAPWorker_HandleTradeKeepsRunningAfterFill(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockEx := mocks.NewMockExchangeExtended(ctrl)

	config := TWAPWorkerConfig{
		Duration:  4 * time.Minute,
		NumSlices: 2,
		OrderType: TWAPOrderTypeTaker,
	}

	worker, err := NewTWAPWorker(mockEx, testMarket, config)
	assert.NoError(t, err)
	worker.SetLogger(testLogger)
	assert.NoError(t, worker.SetTargetPosition(Number(0.5)))
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	assert.NoError(t, worker.Start(ctx, now))

	// simulate an active order (must also be in orderMap)
	order := types.Order{
		OrderID:          1,
		ExecutedQuantity: fixedpoint.Zero,
		SubmitOrder: types.SubmitOrder{
			Quantity: Number(0.5),
		},
	}
	worker.activeOrder = &order
	worker.ordersMap[order.OrderID] = order

	// full fill via trade
	trades := &sync.Map{}
	worker.OnNotify(func(trade types.Trade) {
		trades.Store(trade.ID, struct{}{})
	})
	worker.HandleTrade(types.Trade{
		ID:       1,
		OrderID:  1,
		Quantity: Number(0.5),
		Price:    Number(50010.0),
	})
	waitForTrades(trades, 1)
	assert.Equal(t, TWAPWorkerStateRunning, worker.State())
	assert.False(t, worker.IsDone())
	// processTrade no longer nils activeOrder; it sets status to FILLED instead
	assert.NotNil(t, worker.ActiveOrder())
	assert.Equal(t, types.OrderStatusFilled, worker.ActiveOrder().Status)
	assert.Len(t, worker.Trades(), 1)
	assert.Equal(t, Number(0.5), worker.FilledQuantity())
}
