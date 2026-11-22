// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

// The defaults and limits of the contract's §3.
const (
	MaxServices = 64
	MaxJobs     = 32
	MaxAddons   = 16
	MaxRoutes   = 128
	MaxBindings = 32
	// MaxUses bounds an object's uses and a sandbox's reaches,
	// MaxEgressDomains and MaxEgressBlocks an egress's lists, MaxRouteFrom
	// a route's blocks (ADR 0034, revised).
	MaxUses          = 64
	MaxEgressDomains = 64
	MaxEgressBlocks  = 32
	MaxRouteFrom     = 32
	// MaxRoutePathBytes bounds a route's path, which every instance of
	// ferun-ingress holds for every public environment.
	MaxRoutePathBytes = 1024

	DefaultReplicas  = 1
	MaxReplicas      = 20
	MinVCPUs         = 1
	MaxVCPUs         = 32
	DefaultVCPUs     = 1
	MinMemoryMib     = 256
	MaxMemoryMib     = 65536
	DefaultMemoryMib = 512
	// DefaultDiskGib is the guest's blank data volume, which every pod's
	// disk starts from (feos.pod.v1.Resources.disk_gib): a smaller disk
	// cannot be. MaxDiskGib bounds what one pod books on its node.
	DefaultDiskGib = 8
	MaxDiskGib     = 200
	MaxRetries     = 5

	DefaultRolloutTimeout   = "10m0s"
	DefaultJobTimeout       = "10m0s"
	MaxJobTimeout           = "2h0m0s"
	MinIdle                 = "1m0s"
	DefaultInitialDelay     = "0s"
	DefaultPeriod           = "2s"
	DefaultFailureThreshold = 3
)
