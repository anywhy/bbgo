package grid2types

import (
	"math"

	"github.com/c9s/bbgo/pkg/fixedpoint"
	"github.com/c9s/bbgo/pkg/util"
)

type Pin fixedpoint.Value

func RemoveDuplicatedPins(pins []Pin) []Pin {
	var buckets = map[string]struct{}{}
	var out []Pin

	for _, pin := range pins {
		p := fixedpoint.Value(pin)

		if _, exists := buckets[p.String()]; exists {
			continue
		}

		out = append(out, pin)
		buckets[p.String()] = struct{}{}
	}

	return out
}

func CalculateArithmeticPins(lower, upper, spread, tickSize fixedpoint.Value) []Pin {
	var pins []Pin

	// tickSize number is like 0.01, 0.1, 0.001
	var ts = tickSize.Float64()
	var prec = int(math.Round(math.Log10(ts) * -1.0))
	for p := lower; p.Compare(upper.Sub(spread)) <= 0; p = p.Add(spread) {
		price := util.RoundAndTruncatePrice(p, prec)
		pins = append(pins, Pin(price))
	}

	// this makes sure there is no error at the upper price
	upperPrice := util.RoundAndTruncatePrice(upper, prec)
	pins = append(pins, Pin(upperPrice))

	return pins
}

type PinCacheMap map[Pin]struct{}

func BuildPinCache(pins []Pin) PinCacheMap {
	cache := make(PinCacheMap, len(pins))
	for _, pin := range pins {
		cache[pin] = struct{}{}
	}

	return cache
}

type PinCalculator func() []Pin
