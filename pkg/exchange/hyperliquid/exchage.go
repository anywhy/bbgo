package hyperliquid

import (
	"github.com/c9s/bbgo/pkg/exchange/hyperliquid/hyperapi"
	"github.com/c9s/bbgo/pkg/types"
	"github.com/sirupsen/logrus"
)

const (
	PlatformToken = "HYPE"

	ID types.ExchangeName = "hyperliquid"
)

var log = logrus.WithFields(logrus.Fields{
	"exchange": "hyperliquid",
})

type Exchange struct {
	types.FuturesSettings

	secret string

	client *hyperapi.Client
}

func New(secret, vaultAddress string) *Exchange {
	client := hyperapi.NewClient()
	if len(secret) > 0 {
		client.Auth(secret)
	}

	if len(vaultAddress) > 0 {
		client.SetVaultAddress(vaultAddress)
	}
	return &Exchange{
		secret: secret,
		client: client,
	}
}

func (e *Exchange) Name() types.ExchangeName {
	return ID
}
