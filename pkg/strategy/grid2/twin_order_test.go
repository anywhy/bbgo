package grid2

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/c9s/bbgo/pkg/fixedpoint"
	"github.com/c9s/bbgo/pkg/strategy/grid2/grid2types"
	"github.com/c9s/bbgo/pkg/types"
)

func TestTwinOrderBook(t *testing.T) {
	assert := assert.New(t)
	pins := []grid2types.Pin{
		grid2types.Pin(fixedpoint.NewFromInt(3)),
		grid2types.Pin(fixedpoint.NewFromInt(4)),
		grid2types.Pin(fixedpoint.NewFromInt(1)),
		grid2types.Pin(fixedpoint.NewFromInt(5)),
		grid2types.Pin(fixedpoint.NewFromInt(2)),
	}

	book := newTwinOrderBook(pins)
	assert.Equal(0, book.Size())
	assert.Equal(4, book.EmptyTwinOrderSize())
	for _, pin := range pins {
		twinOrder := book.GetTwinOrder(fixedpoint.Value(pin))
		if fixedpoint.NewFromInt(1) == fixedpoint.Value(pin) {
			assert.Nil(twinOrder)
			continue
		}

		if !assert.NotNil(twinOrder) {
			continue
		}

		assert.False(twinOrder.Exist())
	}

	orders := []types.Order{
		{
			OrderID: 1,
			SubmitOrder: types.SubmitOrder{
				Price: fixedpoint.NewFromInt(2),
				Side:  types.SideTypeBuy,
			},
		},
		{
			OrderID: 2,
			SubmitOrder: types.SubmitOrder{
				Price: fixedpoint.NewFromInt(4),
				Side:  types.SideTypeSell,
			},
		},
	}

	for _, order := range orders {
		assert.NoError(book.AddOrder(order, false))
	}
	assert.Equal(2, book.Size())
	assert.Equal(2, book.EmptyTwinOrderSize())

	for _, order := range orders {
		pin, err := book.GetTwinOrderPin(order)
		if !assert.NoError(err) {
			continue
		}
		twinOrder := book.GetTwinOrder(pin)
		if !assert.True(twinOrder.Exist()) {
			continue
		}

		assert.Equal(order.OrderID, twinOrder.GetOrder().OrderID)
	}
}
