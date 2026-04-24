package xfundingv2

import (
	"errors"

	"github.com/c9s/bbgo/pkg/fixedpoint"
	"github.com/c9s/bbgo/pkg/types"
)

const annualFundingHours = 24 * 365

// AnnualizedRate converts a per-period funding rate to annualized rate
func AnnualizedRate(fundingRate fixedpoint.Value, fundingIntervalHours int) fixedpoint.Value {
	numFundingPeriods := int64(annualFundingHours / fundingIntervalHours)
	return fundingRate.Mul(fixedpoint.NewFromInt(numFundingPeriods))
}

type CostEstimator struct {
	market types.Market

	// targetPosition is the open position size in futures (can be positive or negative)
	// the open position size in spot is always the same but with opposite sign
	targetPosition fixedpoint.Value

	futuresOrderBook, spotOrderBook *types.StreamOrderBook

	futuresFeeRate, spotFeeRate types.ExchangeFee
}

func NewCostEstimator(market types.Market, futuresOrderBook, spotOrderBook *types.StreamOrderBook) *CostEstimator {
	return &CostEstimator{
		market:           market,
		futuresOrderBook: futuresOrderBook,
		spotOrderBook:    spotOrderBook,
	}
}

func (c *CostEstimator) SetFuturesFeeRate(feeRate types.ExchangeFee) *CostEstimator {
	c.futuresFeeRate = feeRate
	return c
}

func (c *CostEstimator) SetSpotFeeRate(feeRate types.ExchangeFee) *CostEstimator {
	c.spotFeeRate = feeRate
	return c
}

func (c *CostEstimator) SetTargetPosition(position fixedpoint.Value) *CostEstimator {
	c.targetPosition = position
	return c
}

// EstimatedCost represents the estimated cost of a transaction, either entry or exit
type EstimatedCost struct {
	FuturesPosition fixedpoint.Value
	SpotFee         fixedpoint.Value
	FuturesFee      fixedpoint.Value
	SpreadPnL       fixedpoint.Value
}

func (e *EstimatedCost) TotalFeeCost() fixedpoint.Value {
	return e.SpotFee.Add(e.FuturesFee)
}

func (e *EstimatedCost) Spread() fixedpoint.Value {
	return e.SpreadPnL.Div(e.FuturesPosition)
}

// EstimateEntryCost calculates the cost of entering the position
// Note that the cost is based on the current order book, the cost is estimated by the time this function is called.
func (c *CostEstimator) EstimateEntryCost(isMaker bool) (EstimatedCost, error) {
	if c.targetPosition.IsZero() {
		return EstimatedCost{}, nil
	}
	var spotPV, futuresPV types.PriceVolume
	var spotOk, futuresOk bool
	if c.targetPosition.Sign() < 0 {
		// short futures, long spot
		// buy spot at best ask
		spotPV, spotOk = c.spotOrderBook.BestAsk()
		// sell futures at best bid
		futuresPV, futuresOk = c.futuresOrderBook.BestBid()
	} else {
		// long futures, short spot
		// sell spot at best bid
		spotPV, spotOk = c.spotOrderBook.BestBid()
		// buy futures at best ask
		futuresPV, futuresOk = c.futuresOrderBook.BestAsk()
	}

	if !spotOk || !futuresOk {
		return EstimatedCost{}, errors.New("order book data is not ready yet")
	}

	return c.estimateCost(isMaker, spotPV.Price, futuresPV.Price), nil
}

func (c *CostEstimator) EstimateExitCost(isMaker bool) (EstimatedCost, error) {
	if c.targetPosition.IsZero() {
		return EstimatedCost{}, nil
	}

	var spotPV, futuresPV types.PriceVolume
	var spotOk, futuresOk bool
	if c.targetPosition.Sign() < 0 {
		// long futures, short spot to close
		spotPV, spotOk = c.spotOrderBook.BestBid()          // sell spot at best bid
		futuresPV, futuresOk = c.futuresOrderBook.BestAsk() // buy futures at best ask
	} else {
		// short futures, long spot to close
		spotPV, spotOk = c.spotOrderBook.BestAsk()          // buy spot at best ask
		futuresPV, futuresOk = c.futuresOrderBook.BestBid() // sell futures at best bid
	}

	if !spotOk || !futuresOk {
		return EstimatedCost{}, errors.New("order book data is not ready yet")
	}

	return c.estimateCost(isMaker, spotPV.Price, futuresPV.Price), nil
}

func (c *CostEstimator) estimateCost(isMaker bool, spotPrice, futuresPrice fixedpoint.Value) EstimatedCost {
	priceSpread := spotPrice.Sub(futuresPrice)
	// note that the c.targetPosition can be positive or negative, the spread PnL is hence:
	// let positoinSize = c.targetPosition.Abs()
	// spreadPnL = (spotPrice - futuresPrice) * positoinSize if c.targetPosition > 0 (long futures, short spot)
	// spreadPnL = (futuresPrice - spotPrice) * positoinSize if c.targetPosition < 0 (short futures, long spot)
	// then we have spreadPnL = (spotPrice - futuresPrice) * c.targetPosition, which works for both cases
	spreadPnL := priceSpread.Mul(c.targetPosition)

	var spotFeeRate, futuresFeeRate fixedpoint.Value
	if isMaker {
		spotFeeRate = c.spotFeeRate.MakerFeeRate
		futuresFeeRate = c.futuresFeeRate.MakerFeeRate
	} else {
		spotFeeRate = c.spotFeeRate.TakerFeeRate
		futuresFeeRate = c.futuresFeeRate.TakerFeeRate
	}

	// calculate fees in quote currency
	positionSize := c.targetPosition.Abs()
	spotFee := spotPrice.Mul(positionSize).Mul(spotFeeRate)
	futuresFee := futuresPrice.Mul(positionSize).Mul(futuresFeeRate)

	return EstimatedCost{
		FuturesPosition: c.targetPosition,
		SpotFee:         spotFee,
		FuturesFee:      futuresFee,
		SpreadPnL:       spreadPnL,
	}
}
