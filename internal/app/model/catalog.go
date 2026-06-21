// Package model is the /v1/models use-case layer: it assembles the OpenAI
// model-list envelope from the LIVE MODEL_ALLOWLIST. It declares its only
// dependency as a config PORT and never imports infra/net.http/sql. Auth lives
// in transport (bearer → install lookup); this layer is pure declaration and
// bills nothing — listing models reserves no quota.
package model

import (
	"github.com/sunweilin/anselm/gateway/internal/domain/config"
	"github.com/sunweilin/anselm/gateway/internal/domain/model"
)

// ConfigLoader is the runtime-config port: a snapshot of the live, atomically
// swapped Config. Satisfied structurally by *infra/configprovider.Provider.
// List() snapshots ONCE per call so a hot-reloaded MODEL_ALLOWLIST is reflected
// on the very next request with no restart (声明热变跟随白名单热变).
type ConfigLoader interface {
	Load() *config.Config
}

// Catalog assembles the model list from the live allowlist.
type Catalog struct {
	cfg ConfigLoader
}

// New wires the Catalog to its config port.
func New(cfg ConfigLoader) *Catalog {
	return &Catalog{cfg: cfg}
}

// List builds the OpenAI envelope from the CURRENT allowlist, preserving its
// order. Data is always non-nil (empty allowlist → [], never null) so the wire
// JSON is "data":[] not "data":null.
func (c *Catalog) List() model.ListEnvelope {
	allow := c.cfg.Load().ModelAllowlist
	data := make([]model.Model, 0, len(allow))
	for _, id := range allow {
		data = append(data, model.Model{
			ID:      id,
			Object:  model.ObjectModel,
			OwnedBy: model.OwnedBy,
		})
	}
	return model.ListEnvelope{Object: model.ObjectList, Data: data}
}
