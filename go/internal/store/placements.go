// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

// PlacementCandidate is one node's verdict in a placement decision. The
// decision's audit is the activity row of kind placement, which names the
// verdict of every node.
type PlacementCandidate struct {
	Node     string  `json:"node"`
	Feasible bool    `json:"feasible"`
	Score    float64 `json:"score"`
	Reason   string  `json:"reason"`
	// Peers are, for a service's replica, its peers on the node as the
	// placement's snapshot saw them, which rank the node before the score
	// does (ADR 0018).
	Peers
}

// Peers are the other instances of a service's replica's service on one
// node: Same of the replica's template, Other of another (a rollout's old
// ones while its new ones come). An instance that stops counts for
// nothing, nor does one whose pod holds no place on the node (pending,
// hibernated, lost). A pod that is no service's instance has none.
type Peers struct {
	Same  int `json:"instances,omitempty"`
	Other int `json:"otherInstances,omitempty"`
}
