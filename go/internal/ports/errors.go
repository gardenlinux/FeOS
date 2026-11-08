// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ports

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// verdict is the door's reading of a failed forward: the answer, whether
// the node does not run the pod as the row says (stale: sync, wake and,
// for a repeatable request, try once more), and whether to log it.
type verdict struct {
	reply
	stale bool
	log   bool
}

// classify reads a failed forward. A dial carries the gRPC status of the
// node or the guest, whose codes are ambiguous on their own: the texts
// tell "nothing listens on the port" ("127.0.0.1:<port>: ...") from "the
// guest does not answer" ("the guest of pod ...") from "no connection to
// the node", and a refused Kubernetes listener ("port <p> is a kube
// listener") from a pod in another state ("pod <id> is POD_STATE_...").
// Anything that fails after the stream stood is the server in the guest.
// A request whose client went away fails with its context's error; its
// answer reaches nobody.
func classify(err error, node string, port int) verdict {
	var de *dialError
	switch {
	case errors.Is(err, errCapFull):
		return verdict{reply: reply{http.StatusServiceUnavailable,
			"too many connections into this sandbox on this instance, try again", 1}}
	case errors.As(err, &de):
		return classifyStatus(de.err, node, port)
	}
	return verdict{reply: reply{http.StatusBadGateway, nothingAnswered(port), 0}}
}

func classifyStatus(err error, node string, port int) verdict {
	st, _ := status.FromError(err)
	msg := st.Message()
	switch st.Code() {
	case codes.FailedPrecondition:
		if strings.Contains(msg, "kube listener") {
			return verdict{reply: reply{http.StatusForbidden, kubeText(port), 0}}
		}
		return verdict{reply: reply{http.StatusServiceUnavailable, textWakingUp, 1}, stale: true}
	case codes.NotFound:
		return verdict{reply: reply{http.StatusServiceUnavailable, textWakingUp, 1}, stale: true}
	case codes.Unavailable:
		switch {
		case strings.HasPrefix(msg, "127.0.0.1:"):
			return verdict{reply: reply{http.StatusBadGateway, nothingAnswered(port), 0}}
		case strings.Contains(msg, "the guest of pod"):
			return verdict{reply: reply{http.StatusServiceUnavailable, "the sandbox's guest does not answer, try again", 1}}
		}
		return verdict{reply: reply{http.StatusServiceUnavailable, "the sandbox's node is unavailable", 5}, log: true}
	case codes.ResourceExhausted:
		return verdict{reply: reply{http.StatusServiceUnavailable, msg + ", try again", 1}, log: true}
	case codes.Unimplemented:
		return verdict{reply: reply{http.StatusBadGateway, fmt.Sprintf("node %s does not forward ports yet", node), 0},
			log: true}
	case codes.Canceled, codes.DeadlineExceeded:
		return verdict{reply: reply{http.StatusBadGateway, nothingAnswered(port), 0}}
	}
	return verdict{reply: reply{http.StatusBadGateway, fmt.Sprintf("node %s did not carry the forward", node), 0}, log: true}
}

// Explain is a failed DialPod's or Probe's reason in the door's words,
// for a readiness check a person reads: "nothing answered on port 8080 in
// this sandbox" rather than the node's gRPC status, whose whole stays in
// the error.
func Explain(err error, port int) string {
	var se *slowError
	var de *dialError
	switch {
	case errors.As(err, &se):
		return fmt.Sprintf("port %d did not answer within %s", se.port, se.after)
	case errors.Is(err, errCapFull), errors.As(err, &de):
		return strings.TrimSuffix(classify(err, nodeOf(de), port).msg, ", try again")
	}
	return err.Error()
}

func nodeOf(de *dialError) string {
	if de == nil {
		return ""
	}
	return de.node
}

// slowError is a dial or a probe that did not finish within the probe's
// timeout; err, when the dial failed, is why.
type slowError struct {
	port  int
	after time.Duration
	err   error
}

func (e *slowError) Error() string {
	s := fmt.Sprintf("port %d did not answer within %s", e.port, e.after)
	if e.err != nil {
		s += ": " + e.err.Error()
	}
	return s
}

func (e *slowError) Unwrap() error { return e.err }

func nothingAnswered(port int) string {
	return fmt.Sprintf("nothing answered on port %d in this sandbox", port)
}
