// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// MergePatch applies the RFC 7396 merge patch patch to the JSON document
// doc and returns the result: an object in patch merges into doc's
// object member by member, null removes a member, anything else replaces.
// Both must be JSON; doc may be empty (an empty object). Numbers keep
// their text.
func MergePatch(doc, patch []byte) ([]byte, error) {
	var d any = map[string]any{}
	if len(bytes.TrimSpace(doc)) > 0 {
		var err error
		if d, err = decodeOne(doc); err != nil {
			return nil, fmt.Errorf("merge patch: the document: %w", err)
		}
	}
	p, err := decodeOne(patch)
	if err != nil {
		return nil, fmt.Errorf("merge patch: the patch: %w", err)
	}
	return json.Marshal(mergePatch(d, p))
}

// decodeOne reads exactly one JSON value, numbers as json.Number.
func decodeOne(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("data after the value")
	}
	return v, nil
}

func mergePatch(target, patch any) any {
	pm, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	tm, ok := target.(map[string]any)
	if !ok {
		tm = map[string]any{}
	}
	for k, v := range pm {
		if v == nil {
			delete(tm, k)
			continue
		}
		tm[k] = mergePatch(tm[k], v)
	}
	return tm
}
