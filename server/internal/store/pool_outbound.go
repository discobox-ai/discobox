package store

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"

	"github.com/discobox-ai/discobox/pkg/pooloutbound"
	"github.com/discobox-ai/discobox/server/internal/encryption"
	"github.com/discobox-ai/discobox/server/internal/model"
)

func (s *Store) OpenPoolProxy(ctx context.Context, pool *model.Pool) (*pooloutbound.Intent, error) {
	if len(pool.ProxyIntent) == 0 {
		return nil, nil
	}
	raw, err := s.keyring.Open(s.keyContext(ctx), encryption.PoolProxyIntent(pool.ProjectID, pool.ID), pool.ProxyIntent)
	if err != nil {
		return nil, err
	}
	var i pooloutbound.Intent
	if err := json.Unmarshal(raw, &i); err != nil {
		return nil, errors.New("invalid saved pool proxy intent")
	}
	return &i, nil
}

// SetPoolProxy prepares intent for the caller's pool transaction; nil removes it.
func (s *Store) SetPoolProxy(ctx context.Context, pool *model.Pool, intent *pooloutbound.Intent) error {
	pool.ProxyIntent = nil
	if intent == nil {
		return nil
	}
	if pool.ID == "" {
		if err := pool.BeforeCreate(nil); err != nil {
			return err
		}
	}
	if s.keyring == nil {
		for _, raw := range []string{intent.HTTP, intent.HTTPS} {
			u, _ := url.Parse(raw)
			if u != nil && u.User != nil {
				return errors.New("proxy credentials require a configured encryption provider")
			}
		}
	}
	raw, err := json.Marshal(intent)
	if err != nil {
		return err
	}
	pool.ProxyIntent, err = s.keyring.Seal(s.keyContext(ctx), encryption.PoolProxyIntent(pool.ProjectID, pool.ID), raw)
	return err
}
