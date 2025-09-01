package hyperliquid

import (
	"github.com/c9s/bbgo/pkg/types"
	"github.com/sirupsen/logrus"
)

const ID types.ExchangeName = "hyperliquid"

var log = logrus.WithFields(logrus.Fields{
	"exchange": "hyperliquid",
})

type Exchange struct {
}

func New(key, secret string) *Exchange {
	return &Exchange{}
}

func (e *Exchange) Name() types.ExchangeName {
	return ID
}
