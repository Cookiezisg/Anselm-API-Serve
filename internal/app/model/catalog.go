// Package model is the /v1/models use-case layer: it assembles the OpenAI
// model-list envelope from the one live provider-neutral PUBLIC_MODEL_ID. It declares its only
// dependency as a config PORT and never imports infra/net.http/sql. Auth lives
// in transport (device proof → install lookup); this layer is pure declaration and
// bills nothing — listing models reserves no quota.
package model

import (
	"github.com/sunweilin/anselm/gateway/internal/domain/config"
	"github.com/sunweilin/anselm/gateway/internal/domain/model"
)

// ConfigLoader is the runtime-config port: a snapshot of the live, atomically
// swapped Config. Satisfied structurally by *infra/configprovider.Provider.
// List() snapshots ONCE per call so a hot-reloaded PUBLIC_MODEL_ID is reflected
// on the very next request with no restart.
type ConfigLoader interface {
	Load() *config.Config
}

// Catalog assembles the model list from the live logical model id.
type Catalog struct {
	cfg ConfigLoader
}

// New wires the Catalog to its config port.
func New(cfg ConfigLoader) *Catalog {
	return &Catalog{cfg: cfg}
}

// List exposes exactly one provider-neutral logical model. Provider model ids
// are intentionally absent: request shape selects text vs multimodal and a
// client can never buy/select a hidden "tier" through the model string.
func (c *Catalog) List() model.ListEnvelope {
	id := c.cfg.Load().PublicModelID
	data := make([]model.Model, 0, 1)
	if id != "" {
		data = append(data, model.Model{ID: id, Object: model.ObjectModel, OwnedBy: model.OwnedBy})
	}
	return model.ListEnvelope{Object: model.ObjectList, Data: data}
}
