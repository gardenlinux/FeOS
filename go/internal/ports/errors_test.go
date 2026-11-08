// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ports

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestExplain(t *testing.T) {
	refused := &dialError{node: "n1", err: status.Error(codes.Unavailable, "127.0.0.1:8080: Connection refused (os error 111)")}
	for _, c := range []struct {
		err  error
		want string
	}{
		{refused, "nothing answered on port 8080 in this sandbox"},
		{fmt.Errorf("wrapped: %w", refused), "nothing answered on port 8080 in this sandbox"},
		{&dialError{node: "n1", err: status.Error(codes.Unavailable, "the guest of pod p1 does not answer")},
			"the sandbox's guest does not answer"},
		{&dialError{node: "n1", err: status.Error(codes.Unavailable, "transport is closing")}, "the sandbox's node is unavailable"},
		{&slowError{port: 8080, after: 2 * time.Second, err: refused}, "port 8080 did not answer within 2s"},
		{errCapFull, "too many connections into this sandbox on this instance"},
		{errors.New("port 8080: unexpected EOF"), "port 8080: unexpected EOF"},
	} {
		if got := Explain(c.err, 8080); got != c.want {
			t.Errorf("Explain(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}
