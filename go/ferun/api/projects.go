// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

// DeleteProjectRequest is DELETE /api/v1/project's body: the project's
// name again, so that a call meant for another project deletes nothing
// (400 name_mismatch when it is not the project the call is for).
type DeleteProjectRequest struct {
	Name string `json:"name"`
}

// DeleteProjectAnswer is its answer (202): the project is being deleted;
// it goes when its sandboxes and environments have, as an operator's
// delete of it does.
type DeleteProjectAnswer struct {
	Project  string `json:"project"`
	Deleting bool   `json:"deleting"`
}

// CodeNameMismatch answers a DELETE /api/v1/project whose body names
// another project than the one the call is for.
const CodeNameMismatch = "name_mismatch"
