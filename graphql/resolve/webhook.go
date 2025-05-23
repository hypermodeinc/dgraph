/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package resolve

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/golang/glog"
	"github.com/pkg/errors"

	"github.com/hypermodeinc/dgraph/v25/graphql/authorization"
	"github.com/hypermodeinc/dgraph/v25/graphql/schema"
	"github.com/hypermodeinc/dgraph/v25/x"
)

type webhookPayload struct {
	Resolver   string             `json:"resolver"`
	AccessJWT  string             `json:"X-Dgraph-AccessToken,omitempty"`
	AuthHeader *authHeaderPayload `json:"authHeader,omitempty"`
	Event      eventPayload       `json:"event"`
}

type authHeaderPayload struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type eventPayload struct {
	Typename  string              `json:"__typename"`
	Operation schema.MutationType `json:"operation"`
	CommitTs  uint64              `json:"commitTs"`
	Add       *addEvent           `json:"add,omitempty"`
	Update    *updateEvent        `json:"update,omitempty"`
	Delete    *deleteEvent        `json:"delete,omitempty"`
}

type addEvent struct {
	RootUIDs []string      `json:"rootUIDs"`
	Input    []interface{} `json:"input"`
}

type updateEvent struct {
	RootUIDs    []string    `json:"rootUIDs"`
	SetPatch    interface{} `json:"setPatch"`
	RemovePatch interface{} `json:"removePatch"`
}

type deleteEvent struct {
	RootUIDs []string `json:"rootUIDs"`
}

// sendWebhookEvent forms an HTTP payload required for the webhooks configured with @lambdaOnMutate
// directive, and then sends that payload to the lambda URL configured with Alpha. There is no
// guarantee that the payload will be delivered successfully to the lambda server.
func sendWebhookEvent(ctx context.Context, m schema.Mutation, commitTs uint64, rootUIDs []string) {
	accessJWT, _ := x.ExtractJwt(ctx)
	var authHeader *authHeaderPayload
	if m.GetAuthMeta() != nil {
		authHeader = &authHeaderPayload{
			Key:   m.GetAuthMeta().GetHeader(),
			Value: authorization.GetJwtToken(ctx),
		}
	}

	payload := webhookPayload{
		Resolver:   "$webhook",
		AccessJWT:  accessJWT,
		AuthHeader: authHeader,
		Event: eventPayload{
			Typename:  m.MutatedType().Name(),
			Operation: m.MutationType(),
			CommitTs:  commitTs,
		},
	}

	switch payload.Event.Operation {
	case schema.AddMutation:
		input, _ := m.ArgValue(schema.InputArgName).([]interface{})
		payload.Event.Add = &addEvent{
			RootUIDs: rootUIDs,
			Input:    input,
		}
	case schema.UpdateMutation:
		inp, _ := m.ArgValue(schema.InputArgName).(map[string]interface{})
		payload.Event.Update = &updateEvent{
			RootUIDs:    rootUIDs,
			SetPatch:    inp["set"],
			RemovePatch: inp["remove"],
		}
	case schema.DeleteMutation:
		payload.Event.Delete = &deleteEvent{RootUIDs: rootUIDs}
	}

	b, err := json.Marshal(payload)
	if err != nil {
		glog.Error(errors.Wrap(err, "error marshalling webhook payload"))
		// don't care to send the payload if there are JSON marshalling errors
		return
	}

	// send the request
	ns, _ := x.ExtractNamespace(ctx)
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	resp, err := schema.MakeHttpRequest(nil, http.MethodPost, x.LambdaUrl(ns), headers, b)

	// just log the response errors, if any.
	if err != nil {
		glog.V(3).Info(errors.Wrap(err, "unable to send webhook event"))
		return
	}

	defer func() {
		if err = resp.Body.Close(); err != nil {
			glog.Errorf("Error while closing response body: %v", err)
		}
	}()

	if resp != nil && (resp.StatusCode < 200 || resp.StatusCode >= 300) {
		glog.V(3).Info(errors.Errorf("got unsuccessful status from webhook: %s", resp.Status))
	}
}
