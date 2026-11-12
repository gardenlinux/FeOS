// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package visits

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/google/uuid"
)

// maxAnswer is how much of a provider's answer is read.
const maxAnswer = 64 << 10

// ask is the provider call (§1.9): POST {"visit": id} to the provider's
// URL as stored, with no credential, through the control plane's
// destination rule (https, IPv4, every connect judged, no redirect, no
// proxy). Only a 200 with a JSON object whose admitted is false is a no,
// one whose admitted is true a yes; anything else is no answer, with the
// error that says why (never the body).
func (s *Service) ask(ctx context.Context, provider string, visit uuid.UUID) (verdict, error) {
	body, _ := json.Marshal(map[string]string{"visit": visit.String()})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, provider, bytes.NewReader(body))
	if err != nil {
		return verdictNone, fmt.Errorf("the provider's URL: %w", err)
	}
	req.Header = http.Header{
		"Content-Type": {"application/json"},
		"Accept":       {"application/json"},
		"User-Agent":   {"ferund-visit"},
	}
	resp, err := s.client.Do(req)
	if err != nil {
		// The URL error names the provider's URL; its cause is enough.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return verdictNone, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxAnswer))
		return verdictNone, fmt.Errorf("answered %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer+1))
	if err != nil {
		return verdictNone, fmt.Errorf("read the answer: %w", err)
	}
	if len(raw) > maxAnswer {
		return verdictNone, fmt.Errorf("an answer of more than %d bytes", maxAnswer)
	}
	var answer map[string]json.RawMessage
	if err := json.Unmarshal(raw, &answer); err != nil || answer == nil {
		return verdictNone, errors.New("an answer that is no JSON object")
	}
	switch string(bytes.TrimSpace(answer["admitted"])) {
	case "false":
		return verdictNo, nil
	case "true":
		return verdictYes, nil
	}
	return verdictNone, errors.New("an answer without admitted true or false")
}
