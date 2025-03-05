package coinbase

import (
	"hash/fnv"
	"math"
	"time"

	api "github.com/c9s/bbgo/pkg/exchange/coinbase/api/v1"
	"github.com/c9s/bbgo/pkg/fixedpoint"
	"github.com/c9s/bbgo/pkg/types"
)

func toGlobalOrder(cbOrder *api.Order) types.Order {
	return types.Order{
		Exchange:       types.ExchangeCoinBase,
		Status:         cbOrder.Status.GlobalOrderStatus(),
		UUID:           cbOrder.ID,
		OrderID:        FNV64a(cbOrder.ID),
		OriginalStatus: string(cbOrder.Status),
		CreationTime:   cbOrder.CreatedAt,
	}
}

func toGlobalTrade(cbTrade *api.Trade) types.Trade {
	return types.Trade{
		ID:            uint64(cbTrade.TradeID),
		OrderID:       FNV64a(cbTrade.OrderID),
		Exchange:      types.ExchangeCoinBase,
		Price:         cbTrade.Price,
		Quantity:      cbTrade.Size,
		QuoteQuantity: cbTrade.Size.Mul(cbTrade.Price),
		Symbol:        cbTrade.ProductID,
		Side:          cbTrade.Side.GlobalSideType(),
		IsBuyer:       cbTrade.Liquidity == api.LiquidityTaker,
		IsMaker:       cbTrade.Liquidity == api.LiquidityMaker,
		Fee:           cbTrade.Fee,
		FeeCurrency:   cbTrade.FundingCurrency,
	}
}

// The max order size is estimated according to the trading rules. See below:
// - https://www.coinbase.com/legal/trading_rules
//
// According to the markets list, the PPP is the max slippage percentage:
// - https://exchange.coinbase.com/markets
func toGlobalMarket(cbMarket *api.MarketInfo) types.Market {
	pricePrecision := int(math.Log10(fixedpoint.One.Div(cbMarket.QuoteIncrement).Float64()))
	volumnPrecision := int(math.Log10(fixedpoint.One.Div(cbMarket.BaseIncrement).Float64()))

	// NOTE: Coinbase does not appose a min quantity, but a min notional.
	// So we set the min quantity to the base increment. Or it may require more API calls
	// to calculate the excact min quantity, which is costy.
	minQuantity := cbMarket.BaseIncrement
	// TODO: estimate max quantity by PPP
	// fill a dummy value for now.
	maxQuantity := minQuantity.Mul(fixedpoint.NewFromFloat(1.5))

	return types.Market{
		Exchange:        types.ExchangeCoinBase,
		Symbol:          toGlobalSymbol(cbMarket.ID),
		LocalSymbol:     cbMarket.ID,
		PricePrecision:  pricePrecision,
		VolumePrecision: volumnPrecision,
		QuoteCurrency:   cbMarket.QuoteCurrency,
		BaseCurrency:    cbMarket.BaseCurrency,
		MinNotional:     cbMarket.MinMarketFunds,
		MinAmount:       cbMarket.MinMarketFunds,
		TickSize:        cbMarket.QuoteIncrement,
		StepSize:        cbMarket.BaseIncrement,
		MinPrice:        fixedpoint.Zero,
		MaxPrice:        fixedpoint.Zero,
		MinQuantity:     minQuantity,
		MaxQuantity:     maxQuantity,
	}
}

func FNV64a(text string) uint64 {
	hash := fnv.New64a()
	// In hash implementation, it says never return an error.
	_, _ = hash.Write([]byte(text))
	return hash.Sum64()
}

func toGlobalKline(symbol string, interval types.Interval, candle *api.Candle) types.KLine {
	startTime := candle.Time.Time()
	endTime := startTime.Add(interval.Duration())
	kline := types.KLine{
		Exchange:  types.ExchangeCoinBase,
		Symbol:    symbol,
		StartTime: types.Time(startTime),
		EndTime:   types.Time(endTime),
		Interval:  interval,
		Open:      candle.Open,
		Close:     candle.Close,
		High:      candle.High,
		Low:       candle.Low,
		Volume:    candle.Volume,
	}
	return kline
}

func toGlobalTicker(cbTicker *api.Ticker) types.Ticker {
	ticker := types.Ticker{
		Time:   time.Time(cbTicker.Time),
		Volume: cbTicker.Volume,
		Buy:    cbTicker.Bid,
		Sell:   cbTicker.Ask,
	}
	return ticker
}

func toGlobalBalance(cur string, cbBalance *api.Balance) types.Balance {
	balance := types.NewZeroBalance(cur)
	balance.Available = cbBalance.Available
	balance.Locked = cbBalance.Hold
	balance.NetAsset = cbBalance.Balance
	return balance
}
