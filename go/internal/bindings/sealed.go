// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package bindings

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"

	"github.com/gardenlinux/feos/go/internal/podid"
)

// The tables whose rows carry a sealed credential (internal/seal); each
// is part of the additional data of its blobs.
const (
	tableKube = "kube_bindings"
	tableSSH  = "ssh_bindings"
	tableHTTP = "http_bindings"
	tableCA   = "pod_intercept_cas"
)

// sealAAD binds a blob to its row: a blob copied into another binding,
// another sandbox's row, another auth type or another table does not
// open.
func sealAAD(table string, sandboxID podid.ID, name, authType string) []byte {
	return []byte("ferun/" + table + "/v1\x00" + sandboxID.String() + "\x00" + name + "\x00" + authType)
}

// podVersion is the version of a pod's bindings as IssuePodCredentials
// seals and issued_certs records it: "" when the pod has neither set,
// otherwise the lowercase hex SHA-256 of the Kubernetes set's version, the
// SSH set's version and, for each SSH binding by name, its name and
// server, each followed by a zero byte. The SSH set's version leaves the
// server out (it is ferund's business on the REST surface), but the pod
// receives it, so the pod version covers it: the version changes exactly
// when what PodBindings delivers does.
func podVersion(kubeVersion, sshVersion string, ssh []sshStored) string {
	if kubeVersion == "" && sshVersion == "" {
		return ""
	}
	sorted := append([]sshStored(nil), ssh...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	h := sha256.New()
	h.Write([]byte(kubeVersion + "\x00" + sshVersion + "\x00"))
	for _, b := range sorted {
		h.Write([]byte(b.Name + "\x00" + b.server + "\x00"))
	}
	return hex.EncodeToString(h.Sum(nil))
}
