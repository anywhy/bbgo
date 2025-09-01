package hyperapi

import (
	"github.com/c9s/requestgen"
)

//go:generate requestgen -method POST -url "/info" -type FuturesGetMetaRequest -responseType FuturesGetMetaResponse
type FuturesGetMetaRequest struct {
	client requestgen.APIClient

	metaType string `param:"type,private" default:"meta"`
}

type FuturesGetMetaResponse struct{}

func (c *Client) NewGetFuturesMetaAndAssetCtxsRequest() *FuturesGetMetaRequest {
	return &FuturesGetMetaRequest{
		client: c,
	}
}
