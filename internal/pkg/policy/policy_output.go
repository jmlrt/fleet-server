// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/elastic/fleet-server/v7/internal/pkg/apikey"
	"github.com/elastic/fleet-server/v7/internal/pkg/bulk"
	"github.com/elastic/fleet-server/v7/internal/pkg/dl"
	"github.com/elastic/fleet-server/v7/internal/pkg/logger"
	"github.com/elastic/fleet-server/v7/internal/pkg/model"
	"github.com/elastic/fleet-server/v7/internal/pkg/smap"
	"github.com/rs/zerolog"
)

const (
	OutputTypeElasticsearch = "elasticsearch"
	OutputTypeLogstash      = "logstash"
)

var (
	ErrNoOutputPerms    = errors.New("output permission sections not found")
	ErrFailInjectAPIKey = errors.New("fail inject api key")
)

type PolicyOutput struct {
	Name string
	Type string
	Role *RoleT
}

func (p *PolicyOutput) Prepare(ctx context.Context, zlog zerolog.Logger, bulker bulk.Bulk, agent *model.Agent, outputMap smap.Map, isDefault bool) error {
	switch p.Type {
	case OutputTypeElasticsearch:
		zlog.Debug().Msg("preparing elasticsearch output")

		// The role is required to do api key management
		if p.Role == nil {
			zlog.Error().Str("name", p.Name).Msg("policy does not contain required output permission section")
			return ErrNoOutputPerms
		}

		// Determine whether we need to generate an output ApiKey.
		// This is accomplished by comparing the sha2 hash stored in the agent
		// record with the precalculated sha2 hash of the role.

		// Note: This will need to be updated when doing multi-cluster elasticsearch support
		// Currently, we only have access to the token for the elasticsearch instance fleet-server
		// is monitors. When updating for multiple ES instances we need to tie the token to the output.
		needNewKey := true
		switch {
		case agent.DefaultApiKey == "":
			zlog.Debug().Msg("must generate api key as default API key is not present")
		case p.Role.Sha2 != agent.PolicyOutputPermissionsHash:
			fmt.Println("!= hash?")
			zlog.Debug().Msg("must generate api key as policy output permissions changed")
		default:
			needNewKey = false
			zlog.Debug().Msg("policy output permissions are the same")
		}

		if needNewKey {
			zlog.Debug().
				RawJSON("roles", p.Role.Raw).
				Str("oldHash", agent.PolicyOutputPermissionsHash).
				Str("newHash", p.Role.Sha2).
				Msg("Generating a new API key")

			outputAPIKey, err := generateOutputAPIKey(ctx, bulker, agent.Id, p.Name, p.Role.Raw)
			if err != nil {
				zlog.Error().Err(err).Msg("fail generate output key")
				return err
			}

			agent.DefaultApiKey = outputAPIKey.Agent()

			// When a new keys is generated we need to update the Agent record,
			// this will need to be updated when multiples Elasticsearch output
			// are used.
			zlog.Info().
				Str("hash.sha256", p.Role.Sha2).
				Str(logger.DefaultOutputApiKeyId, outputAPIKey.Id).
				Msg("Updating agent record to pick up default output key.")

			fields := map[string]interface{}{
				dl.FieldDefaultApiKey:               outputAPIKey.Agent(),
				dl.FieldDefaultApiKeyId:             outputAPIKey.Id,
				dl.FieldPolicyOutputPermissionsHash: p.Role.Sha2,
			}
			if agent.DefaultApiKeyId != "" {
				fields[dl.FieldDefaultApiKeyHistory] = model.DefaultApiKeyHistoryItems{
					Id:        agent.DefaultApiKeyId,
					RetiredAt: time.Now().UTC().Format(time.RFC3339),
				}
			}

			// Using painless script to append the old keys to the history
			body, err := renderUpdatePainlessScript(fields)

			if err != nil {
				return err
			}

			if err = bulker.Update(ctx, dl.FleetAgents, agent.Id, body); err != nil {
				zlog.Error().Err(err).Msg("fail update agent record")
				return err
			}
		}

		// Always insert the `api_key` as part of the output block, this is required
		// because only fleet server know the api key for the specific agent, if we don't
		// add it the agent will not receive the `api_key` and will not be able to connect
		// to Elasticsearch.
		//
		// TODO(ph) Investigate allocation with the new LS output, we had optimization
		// in place to reduce number of agent policy allocation when distribution the updated
		// keys to multiples agents.
		if ok := setMapObj(outputMap, agent.DefaultApiKey, p.Name, "api_key"); !ok {
			return ErrFailInjectAPIKey
		}
	case OutputTypeLogstash:
		zlog.Debug().Msg("preparing logstash output")
		zlog.Info().Msg("no actions required for logstash output preparation")
	default:
		zlog.Error().Msgf("unknown output type: %s; skipping preparation", p.Type)
		return fmt.Errorf("encountered unexpected output type while preparing outputs: %s", p.Type)
	}
	return nil
}

func renderUpdatePainlessScript(fields map[string]interface{}) ([]byte, error) {
	var source strings.Builder
	for field := range fields {
		if field == dl.FieldDefaultApiKeyHistory {
			source.WriteString(fmt.Sprint("if (ctx._source.", field, "==null) {ctx._source.", field, "=new ArrayList();} ctx._source.", field, ".add(params.", field, ");"))
		} else {
			source.WriteString(fmt.Sprint("ctx._source.", field, "=", "params.", field, ";"))
		}
	}

	body, err := json.Marshal(map[string]interface{}{
		"script": map[string]interface{}{
			"lang":   "painless",
			"source": source.String(),
			"params": fields,
		},
	})

	return body, err
}

func generateOutputAPIKey(ctx context.Context, bulk bulk.Bulk, agentID, outputName string, roles []byte) (*apikey.ApiKey, error) {
	name := fmt.Sprintf("%s:%s", agentID, outputName)
	return bulk.ApiKeyCreate(
		ctx,
		name,
		"",
		roles,
		apikey.NewMetadata(agentID, apikey.TypeOutput),
	)
}

func setMapObj(obj map[string]interface{}, val interface{}, keys ...string) bool {
	if len(keys) == 0 {
		return false
	}

	for _, k := range keys[:len(keys)-1] {
		v, ok := obj[k]
		if !ok {
			return false
		}

		obj, ok = v.(map[string]interface{})
		if !ok {
			return false
		}
	}

	k := keys[len(keys)-1]
	obj[k] = val

	return true
}
